package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/slopus/pods/internal/api"
)

type sitesFile struct {
	Sites map[string]siteMeta `json:"sites"`
}

type siteMeta struct {
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) sitesDir() string {
	return filepath.Join(s.cfg.DataDir, "sites")
}

func (s *Server) siteDir(name string) string {
	return filepath.Join(s.sitesDir(), name)
}

func (s *Server) sitesMetaPath() string {
	return filepath.Join(s.cfg.DataDir, "sites.json")
}

// listSites walks the flat sites directory and returns api.Site entries sorted
// by name (os.ReadDir returns entries in lexical order).
func (s *Server) listSites() ([]api.Site, error) {
	entries, err := os.ReadDir(s.sitesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []api.Site{}, nil
	}
	if err != nil {
		return nil, err
	}
	meta, err := s.loadSiteMeta()
	if err != nil {
		return nil, err
	}
	var sites []api.Site
	for _, entry := range entries {
		if !entry.IsDir() || !siteNameRe.MatchString(entry.Name()) {
			continue
		}
		site, err := s.statSite(entry.Name(), meta[entry.Name()])
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, nil
}

func (s *Server) statSite(name string, meta siteMeta) (api.Site, error) {
	root := s.siteDir(name)
	info, err := os.Stat(root)
	if err != nil {
		return api.Site{}, err
	}
	var files int
	var size int64
	err = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			files++
			size += fi.Size()
		}
		return nil
	})
	if err != nil {
		return api.Site{}, err
	}
	updatedAt := meta.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = info.ModTime().UTC()
	}
	site := api.Site{Name: name, Files: files, Bytes: size, UpdatedAt: updatedAt.UTC()}
	if s.identity != nil {
		if owner, ok, err := s.identity.siteOwner(name); err == nil && ok {
			site.OwnerID = owner.ID
			site.OwnerLogin = owner.Login
		}
	}
	return site, nil
}

func (s *Server) loadSiteMeta() (map[string]siteMeta, error) {
	out := map[string]siteMeta{}
	data, err := os.ReadFile(s.sitesMetaPath())
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var file sitesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse sites metadata: %w", err)
	}
	if file.Sites != nil {
		out = file.Sites
	}
	return out, nil
}

func (s *Server) saveSiteMeta(meta map[string]siteMeta) error {
	data, err := json.MarshalIndent(sitesFile{Sites: meta}, "", "  ")
	if err != nil {
		return err
	}
	path := s.sitesMetaPath()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sites-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (s *Server) setSiteMeta(name string, updatedAt time.Time) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	meta, err := s.loadSiteMeta()
	if err != nil {
		return err
	}
	meta[name] = siteMeta{UpdatedAt: updatedAt.UTC()}
	return s.saveSiteMeta(meta)
}

func (s *Server) removeSiteMeta(name string) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	meta, err := s.loadSiteMeta()
	if err != nil {
		return err
	}
	delete(meta, name)
	return s.saveSiteMeta(meta)
}

// GET /api/sites
func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	sites, err := s.listSites()
	if err != nil {
		respondErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SiteList{Sites: sites})
}

// PUT /api/sites/{name}
func (s *Server) handleSiteDeploy(w http.ResponseWriter, r *http.Request) {
	s.deploySite(w, r, r.PathValue("name"))
}

func (s *Server) deploySite(w http.ResponseWriter, r *http.Request, name string) {
	if !siteNameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid site name %q", name)
		return
	}
	user, ok := s.requireSiteAccess(w, r, name)
	if !ok {
		return
	}
	unlock := s.lockSite(name)
	defer unlock()

	// A site with no current owner is a fresh claim: drop any orphan document
	// store left behind by a previously deleted site of the same name, so the
	// new owner never inherits stale or pre-seeded data.
	_, owned, err := s.identity.siteOwner(name)
	if err != nil {
		respondErr(w, err)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxSiteBytes)

	tmp, err := os.MkdirTemp(s.sitesDir(), ".deploy-")
	if err != nil {
		respondErr(w, err)
		return
	}
	defer os.RemoveAll(tmp)

	files, size, err := extractTarGz(body, tmp)
	if err != nil {
		respondErr(w, err)
		return
	}

	final := s.siteDir(name)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		respondErr(w, err)
		return
	}
	existed, err := replaceDir(tmp, final)
	if err != nil {
		respondErr(w, err)
		return
	}

	info, err := os.Stat(final)
	if err != nil {
		respondErr(w, err)
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	updatedAt := info.ModTime().UTC()
	if !owned {
		if err := s.store.DeleteSite(name); err != nil {
			respondErr(w, err)
			return
		}
	}
	if err := s.setSiteMeta(name, updatedAt); err != nil {
		respondErr(w, err)
		return
	}
	if err := s.identity.claimSite(name, user, updatedAt); err != nil {
		respondErr(w, err)
		return
	}
	// Report the actual owner (an admin redeploy preserves the first owner).
	owner := ownerFromUser(user)
	if stored, found, err := s.identity.siteOwner(name); err == nil && found {
		owner = stored
	}
	site := api.Site{Name: name, OwnerID: owner.ID, OwnerLogin: owner.Login, Files: files, Bytes: size, UpdatedAt: updatedAt}
	s.publish(api.UpdateEvent{
		Pod:  name,
		Type: "site.deployed",
		Site: &site,
	})
	writeJSON(w, status, api.DeployResult{
		Site: site,
		URL:  s.siteURL(r, name),
	})
}

// DELETE /api/sites/{name}
func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteSite(w, r, r.PathValue("name"))
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request, name string) {
	if !siteNameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid site name %q", name)
		return
	}
	if _, ok := s.requireSiteAccess(w, r, name); !ok {
		return
	}
	unlock := s.lockSite(name)
	defer unlock()

	dir := s.siteDir(name)
	_, statErr := os.Stat(dir)
	dirExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		respondErr(w, statErr)
		return
	}
	_, owned, err := s.identity.siteOwner(name)
	if err != nil {
		respondErr(w, err)
		return
	}
	if !dirExists && !owned {
		writeError(w, http.StatusNotFound, "site %q not found", name)
		return
	}
	// Tear down the store and ownership record before the static files, so a
	// partial failure leaves the site still listed and the delete retryable
	// (and idempotent) rather than stranding an orphan store under the name.
	if err := s.store.DeleteSite(name); err != nil {
		respondErr(w, err)
		return
	}
	if err := s.identity.removeSite(name); err != nil {
		respondErr(w, err)
		return
	}
	if err := s.removeSiteMeta(name); err != nil {
		respondErr(w, err)
		return
	}
	if dirExists {
		if err := os.RemoveAll(dir); err != nil {
			respondErr(w, err)
			return
		}
	}
	s.publish(api.UpdateEvent{
		Pod:  name,
		Type: "site.deleted",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// replaceDir atomically swaps tmp into place at final, reporting whether a
// previous version existed.
func replaceDir(tmp, final string) (existed bool, err error) {
	switch _, statErr := os.Stat(final); {
	case statErr == nil:
		existed = true
	case !errors.Is(statErr, fs.ErrNotExist):
		return false, statErr
	}
	if !existed {
		return false, os.Rename(tmp, final)
	}
	old := final + ".old" // site names cannot contain '.', so this never collides
	_ = os.RemoveAll(old)
	if err := os.Rename(final, old); err != nil {
		return true, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Rename(old, final) // best-effort restore
		return true, err
	}
	_ = os.RemoveAll(old)
	return true, nil
}

// extractTarGz extracts a tar.gz stream into dst, enforcing the contract's
// safety rules: no absolute paths, no ".." components, only regular files
// and directories, at most maxSiteFiles files and maxSiteBytes bytes.
func extractTarGz(src io.Reader, dst string) (files int, total int64, err error) {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return 0, 0, classifyReadErr(err, "invalid gzip data")
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, total, classifyReadErr(err, "invalid tar archive")
		}
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir, tar.TypeReg:
			// allowed
		default:
			return files, total, badRequestf("archive entry %q: only regular files and directories are allowed", hdr.Name)
		}
		rel, err := entryPath(hdr.Name)
		if err != nil {
			return files, total, err
		}
		if rel == "" { // "." or "./"
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, total, err
			}
			continue
		}

		files++
		if files > maxSiteFiles {
			return files, total, badRequestf("too many files (max %d)", maxSiteFiles)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return files, total, err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return files, total, err
		}
		n, copyErr := io.Copy(f, io.LimitReader(tr, maxSiteBytes-total+1))
		closeErr := f.Close()
		total += n
		if total > maxSiteBytes {
			return files, total, badRequestf("site too large (max %d bytes uncompressed)", maxSiteBytes)
		}
		if copyErr != nil {
			return files, total, classifyReadErr(copyErr, "invalid tar archive")
		}
		if closeErr != nil {
			return files, total, closeErr
		}
	}
	return files, total, nil
}

// entryPath validates and normalizes a tar entry name. It returns "" for
// the root entry ("." / "./") which should simply be skipped.
func entryPath(name string) (string, error) {
	if name == "" {
		return "", badRequestf("archive entry has an empty name")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", badRequestf("archive entry %q: absolute paths are not allowed", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", badRequestf("archive entry %q escapes the site root", name)
		}
	}
	p := path.Clean(name)
	if p == "." {
		return "", nil
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", badRequestf("archive entry %q escapes the site root", name)
	}
	return p, nil
}

// classifyReadErr turns request-body / archive decode failures into 400s,
// preserving the friendlier message for body-size violations.
func classifyReadErr(err error, msg string) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return badRequestf("request body too large (max %d bytes)", maxSiteBytes)
	}
	return badRequestf("%s: %v", msg, err)
}

// GET /sites/{site} → 301 to /sites/{site}/
func (s *Server) handleSiteRedirect(w http.ResponseWriter, r *http.Request) {
	site := r.PathValue("site")
	http.Redirect(w, r, "/sites/"+url.PathEscape(site)+"/", http.StatusMovedPermanently)
}

// GET /sites/{site}/{path...}
func (s *Server) handleSiteFile(w http.ResponseWriter, r *http.Request) {
	s.serveSitePath(w, r, r.PathValue("site"), r.PathValue("path"))
}

func (s *Server) handleSubdomainSite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.URL.Path == "/pods.js" || r.URL.Path == "/install.sh" || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/sites/") {
		return false
	}
	site, ok := s.siteFromHost(r.Host)
	if !ok {
		return false
	}
	s.serveSitePath(w, r, site, strings.TrimPrefix(r.URL.Path, "/"))
	return true
}

func (s *Server) serveSitePath(w http.ResponseWriter, r *http.Request, site, requestPath string) {
	if !siteNameRe.MatchString(site) {
		s.notFoundPage(w)
		return
	}
	s.serveStaticRoot(w, r, s.siteDir(site), requestPath)
}

// serveDevFile serves a file from the dev server's live DevRoot directory.
func (s *Server) serveDevFile(w http.ResponseWriter, r *http.Request, requestPath string) {
	s.serveStaticRoot(w, r, s.cfg.DevRoot, requestPath)
}

// serveStaticRoot serves requestPath out of root, falling back to index.html
// for directories and rendering the 404 page when nothing matches.
func (s *Server) serveStaticRoot(w http.ResponseWriter, r *http.Request, root, requestPath string) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		s.notFoundPage(w)
		return
	}
	rel := path.Clean("/" + requestPath)
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil {
		s.notFoundPage(w)
		return
	}
	if info.IsDir() {
		full = filepath.Join(full, "index.html")
		if info, err = os.Stat(full); err != nil || info.IsDir() {
			s.notFoundPage(w)
			return
		}
	}
	f, err := os.Open(full)
	if err != nil {
		s.notFoundPage(w)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, filepath.Base(full), info.ModTime(), f)
}

func (s *Server) siteFromHost(host string) (site string, ok bool) {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || host == "localhost" || net.ParseIP(host) != nil {
		return "", false
	}
	baseHost := s.publicBaseHost()
	if baseHost != "" {
		if host == baseHost || !strings.HasSuffix(host, "."+baseHost) {
			return "", false
		}
		prefix := strings.TrimSuffix(host, "."+baseHost)
		if strings.Contains(prefix, ".") || !siteNameRe.MatchString(prefix) {
			return "", false
		}
		return prefix, true
	}
	labels := strings.Split(host, ".")
	if len(labels) != 3 {
		return "", false
	}
	site = labels[0]
	if !siteNameRe.MatchString(site) {
		return "", false
	}
	return site, true
}

func (s *Server) siteURL(r *http.Request, site string) string {
	base, err := url.Parse(s.baseURL(r))
	if err != nil {
		return s.baseURL(r) + "/sites/" + url.PathEscape(site) + "/"
	}
	base.Host = site + "." + base.Host
	base.Path = "/"
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func (s *Server) publicBaseHost() string {
	if strings.TrimSpace(s.cfg.PublicURL) == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(s.cfg.PublicURL))
	if err != nil {
		return ""
	}
	host := u.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func (s *Server) notFoundPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, notFoundHTML)
}

const notFoundHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>404 — Happy Pods</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
         font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
         background: #0b1020; color: #e7ecf5; }
  main { text-align: center; padding: 2rem; }
  h1 { font-size: 4rem; margin: 0 0 0.5rem; }
  p { color: #9aa7bd; margin: 0 0 1.5rem; }
  a { color: #7dd3fc; text-decoration: none; }
  a:hover { text-decoration: underline; }
</style>
</head>
<body>
<main>
  <h1>404</h1>
  <p>I'm sorry, Dave. I'm afraid that page doesn't exist.</p>
  <a href="/">← back to Happy Pods</a>
</main>
</body>
</html>
`

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
