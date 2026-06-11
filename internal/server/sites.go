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

const publicTeam = "public"

type sitesFile struct {
	Sites map[string]siteMeta `json:"sites"`
}

type siteMeta struct {
	Team      string    `json:"team"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) sitesDir() string {
	return filepath.Join(s.cfg.DataDir, "sites")
}

func (s *Server) siteDir(team, name string) string {
	return filepath.Join(s.sitesDir(), team, name)
}

func (s *Server) sitesMetaPath() string {
	return filepath.Join(s.cfg.DataDir, "sites.json")
}

func tenantKey(team, name string) string {
	return team + "/" + name
}

// listSites walks the sites directory and returns api.Site entries sorted
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
	for _, teamEntry := range entries {
		if !teamEntry.IsDir() || !siteNameRe.MatchString(teamEntry.Name()) {
			continue
		}
		siteEntries, err := os.ReadDir(filepath.Join(s.sitesDir(), teamEntry.Name()))
		if err != nil {
			return nil, err
		}
		for _, siteEntry := range siteEntries {
			if !siteEntry.IsDir() || !siteNameRe.MatchString(siteEntry.Name()) {
				continue
			}
			site, err := s.statSite(teamEntry.Name(), siteEntry.Name(), meta[tenantKey(teamEntry.Name(), siteEntry.Name())])
			if err != nil {
				return nil, err
			}
			sites = append(sites, site)
		}
	}
	return sites, nil
}

func (s *Server) statSite(team, name string, meta siteMeta) (api.Site, error) {
	root := s.siteDir(team, name)
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
	metaTeam := meta.Team
	if metaTeam == "" {
		metaTeam = team
	}
	updatedAt := meta.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = info.ModTime().UTC()
	}
	return api.Site{Name: name, Team: metaTeam, Files: files, Bytes: size, UpdatedAt: updatedAt.UTC()}, nil
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

func (s *Server) setSiteTeam(team, name string, updatedAt time.Time) error {
	meta, err := s.loadSiteMeta()
	if err != nil {
		return err
	}
	meta[tenantKey(team, name)] = siteMeta{Team: team, UpdatedAt: updatedAt.UTC()}
	return s.saveSiteMeta(meta)
}

func (s *Server) removeSiteTeam(team, name string) error {
	meta, err := s.loadSiteMeta()
	if err != nil {
		return err
	}
	delete(meta, tenantKey(team, name))
	return s.saveSiteMeta(meta)
}

// GET /api/sites
func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	sites, err := s.listSites()
	if err != nil {
		respondErr(w, err)
		return
	}
	if !user.Admin {
		filtered := sites[:0]
		for _, site := range sites {
			if site.Team == publicTeam || user.hasRole(site.Team, roleReader) {
				filtered = append(filtered, site)
			}
		}
		sites = filtered
	}
	writeJSON(w, http.StatusOK, api.SiteList{Sites: sites})
}

// PUT /api/sites/{name} — legacy public-team deploy.
func (s *Server) handleSiteDeploy(w http.ResponseWriter, r *http.Request) {
	s.handleTeamSiteDeploy(w, r, publicTeam, r.PathValue("name"))
}

// PUT /api/teams/{team}/sites/{name}
func (s *Server) handleTeamSiteDeployRoute(w http.ResponseWriter, r *http.Request) {
	s.handleTeamSiteDeploy(w, r, r.PathValue("team"), r.PathValue("name"))
}

func (s *Server) handleTeamSiteDeploy(w http.ResponseWriter, r *http.Request, team, name string) {
	if !validTeam(w, team) {
		return
	}
	if !siteNameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid site name %q", name)
		return
	}
	if !s.requireTeamPublish(w, r, team) {
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

	final := s.siteDir(team, name)
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
	if err := s.setSiteTeam(team, name, updatedAt); err != nil {
		respondErr(w, err)
		return
	}
	site := api.Site{Name: name, Team: team, Files: files, Bytes: size, UpdatedAt: updatedAt}
	s.publish(api.UpdateEvent{
		Pod:  name,
		Team: team,
		Type: "site.deployed",
		Site: &site,
	})
	writeJSON(w, status, api.DeployResult{
		Site: site,
		URL:  s.siteURL(r, team, name),
	})
}

// DELETE /api/sites/{name} — legacy public-team delete.
func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	s.handleTeamSiteDelete(w, r, publicTeam, r.PathValue("name"))
}

// DELETE /api/teams/{team}/sites/{name}
func (s *Server) handleTeamSiteDeleteRoute(w http.ResponseWriter, r *http.Request) {
	s.handleTeamSiteDelete(w, r, r.PathValue("team"), r.PathValue("name"))
}

func (s *Server) handleTeamSiteDelete(w http.ResponseWriter, r *http.Request, team, name string) {
	if !validTeam(w, team) {
		return
	}
	if !siteNameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid site name %q", name)
		return
	}
	if _, ok := s.requireTeamRole(w, r, team, rolePublisher); !ok {
		return
	}
	dir := s.siteDir(team, name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "site %q/%q not found", team, name)
		return
	} else if err != nil {
		respondErr(w, err)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		respondErr(w, err)
		return
	}
	if err := s.removeSiteTeam(team, name); err != nil {
		respondErr(w, err)
		return
	}
	s.publish(api.UpdateEvent{
		Pod:  name,
		Team: team,
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

// GET /sites/{site}/{path...} — legacy public-team static serving.
func (s *Server) handleSiteFile(w http.ResponseWriter, r *http.Request) {
	s.serveSitePath(w, r, publicTeam, r.PathValue("site"), r.PathValue("path"))
}

// GET /sites/{team}/{site} → 301 to /sites/{team}/{site}/
func (s *Server) handleTeamSiteRedirect(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	site := r.PathValue("site")
	http.Redirect(w, r, "/sites/"+url.PathEscape(team)+"/"+url.PathEscape(site)+"/", http.StatusMovedPermanently)
}

// GET /sites/{team}/{site}/{path...}
func (s *Server) handleTeamSiteFile(w http.ResponseWriter, r *http.Request) {
	s.serveSitePath(w, r, r.PathValue("team"), r.PathValue("site"), r.PathValue("path"))
}

func (s *Server) handleSubdomainSite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.URL.Path == "/pods.js" || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/sites/") {
		return false
	}
	team, site, ok := s.siteFromHost(r.Host)
	if !ok {
		return false
	}
	s.serveSitePath(w, r, team, site, strings.TrimPrefix(r.URL.Path, "/"))
	return true
}

func (s *Server) serveSitePath(w http.ResponseWriter, r *http.Request, team, site, requestPath string) {
	if !siteNameRe.MatchString(team) || !siteNameRe.MatchString(site) {
		s.notFoundPage(w)
		return
	}
	root := s.siteDir(team, site)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		s.notFoundPage(w)
		return
	}
	if !s.requireAppAccess(w, r, team, site) {
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

func (s *Server) siteFromHost(host string) (team, site string, ok bool) {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || host == "localhost" || net.ParseIP(host) != nil {
		return "", "", false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 3 {
		return "", "", false
	}
	site, team = labels[0], labels[1]
	if !siteNameRe.MatchString(site) {
		return "", "", false
	}
	if !siteNameRe.MatchString(team) {
		return "", "", false
	}
	return team, site, true
}

func (s *Server) siteURL(r *http.Request, team, site string) string {
	base, err := url.Parse(s.baseURL(r))
	if err != nil {
		return s.baseURL(r) + "/sites/" + url.PathEscape(team) + "/" + url.PathEscape(site) + "/"
	}
	base.Host = site + "." + team + "." + base.Host
	base.Path = "/"
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func validTeam(w http.ResponseWriter, value string) bool {
	if siteNameRe.MatchString(value) {
		return true
	}
	writeError(w, http.StatusBadRequest, "invalid team %q", value)
	return false
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
