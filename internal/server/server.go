// Package server implements the podbay HTTP server: bearer-authenticated
// JSON API, tar.gz site deploys, static site serving and embedded web assets.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/slopus/pods/internal/api"
	"github.com/slopus/pods/internal/store"
)

const (
	maxSiteFiles = 10_000    // max files per deployed site
	maxSiteBytes = 256 << 20 // max uncompressed bytes per site; also the body cap
	maxDocBytes  = 1 << 20   // max DB request body
)

var (
	siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

//go:embed web
var webFS embed.FS

// Config configures a Server.
type Config struct {
	DataDir            string // data directory (sites/, db/ and identity.sqlite live here)
	Secret             string // bootstrap admin bearer token
	AuthFile           string // optional auth.json path; defaults to <DataDir>/auth.json
	PublicURL          string // optional base URL for printed site URLs
	CookieDomain       string // optional session cookie domain, e.g. ".podbay.dev"
	GitHubClientID     string // GitHub OAuth app client id
	GitHubClientSecret string // GitHub OAuth app client secret, required for web login
	GitHubRedirectURL  string // optional fixed GitHub OAuth callback URL

	// StaticCacheSeconds caps how long a CDN/browser may cache a pod's static
	// assets before revalidating, so a redeploy shows through quickly. HTML
	// always revalidates regardless. 0 uses the default (60s).
	StaticCacheSeconds int

	// Dev mode (set DevSite): single-site local server with an in-memory
	// store, no auth, and DevRoot served live as the site's static files.
	DevSite string // site name; when non-empty the server runs in dev mode
	DevRoot string // local directory served live as DevSite's static files
}

const defaultStaticCacheSeconds = 60

// assetCacheControl is the Cache-Control sent for non-HTML static assets: a
// short shared-and-browser max-age so Cloudflare (and browsers) re-fetch
// within the window instead of serving a stale deploy for hours. HTML always
// uses noStoreRevalidate so the entry document is never stale.
func (s *Server) assetCacheControl() string {
	secs := s.cfg.StaticCacheSeconds
	if secs <= 0 {
		secs = defaultStaticCacheSeconds
	}
	return fmt.Sprintf("public, max-age=%d, s-maxage=%d, must-revalidate", secs, secs)
}

// htmlCacheControl tells caches to always revalidate HTML (cheap via
// Last-Modified / 304), so a redeploy is visible immediately.
const htmlCacheControl = "no-cache"

// dev reports whether the server runs as a local single-site dev server.
func (c Config) dev() bool { return c.DevSite != "" }

// Server is the podbay HTTP handler.
type Server struct {
	cfg      Config
	auth     *authenticator
	identity *identityStore
	store    *store.Store
	events   *eventHub
	mux       *http.ServeMux
	landing   *template.Template
	podsJS    []byte
	installSH []byte

	metaMu    sync.Mutex             // guards sites.json load-modify-save
	siteMu    sync.Mutex             // guards siteLocks
	siteLocks map[string]*sync.Mutex // per-site deploy/delete serialization
}

// lockSite serializes lifecycle operations (deploy, delete) on one site so
// the owner check, file swap, store, and ownership record stay consistent
// under concurrent requests. Returns the unlock function.
func (s *Server) lockSite(name string) func() {
	s.siteMu.Lock()
	if s.siteLocks == nil {
		s.siteLocks = make(map[string]*sync.Mutex)
	}
	m := s.siteLocks[name]
	if m == nil {
		m = &sync.Mutex{}
		s.siteLocks[name] = m
	}
	s.siteMu.Unlock()
	m.Lock()
	return m.Unlock
}

// New creates the data layout, opens the document store and builds the
// route table.
func New(cfg Config) (*Server, error) {
	if cfg.Secret == "" {
		return nil, errors.New("server: secret must not be empty")
	}
	// In dev mode everything is in memory and nothing touches disk except the
	// (read-only) DevRoot being served.
	authPath := cfg.AuthFile
	if !cfg.dev() {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, "sites"), 0o755); err != nil {
			return nil, fmt.Errorf("server: create sites dir: %w", err)
		}
		if authPath == "" {
			authPath = filepath.Join(cfg.DataDir, "auth.json")
		}
	} else {
		authPath = "" // build an in-memory authenticator, write no auth.json
	}
	auth, err := newAuthenticator(authPath, cfg.Secret)
	if err != nil {
		return nil, err
	}
	auth.applyGitHubConfig(authGitHubFile{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		RedirectURL:  cfg.GitHubRedirectURL,
	})
	if domain := strings.TrimSpace(cfg.CookieDomain); domain != "" {
		auth.cookieDomain = domain
	}
	identity, st, err := openStores(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, auth: auth, identity: identity, store: st, events: newEventHub()}
	landing, err := template.ParseFS(webFS, "web/index.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse landing page: %w", err)
	}
	podsJS, err := webFS.ReadFile("web/pods.js")
	if err != nil {
		return nil, fmt.Errorf("server: read pods.js: %w", err)
	}
	installSH, err := webFS.ReadFile("web/install.sh")
	if err != nil {
		return nil, fmt.Errorf("server: read install.sh: %w", err)
	}
	s.landing = landing
	s.podsJS = podsJS
	s.installSH = installSH
	s.routes()
	return s, nil
}

// NewDev builds a single-site, fully in-memory dev server that serves devRoot
// live as devSite, with no auth and nothing written to disk. It backs the
// `pods dev` command.
func NewDev(devSite, devRoot string) (*Server, error) {
	secret, err := randomString(24) // ephemeral; never printed
	if err != nil {
		return nil, err
	}
	return New(Config{Secret: secret, DevSite: devSite, DevRoot: devRoot})
}

// openStores opens the identity and document stores: in memory for the dev
// server, on disk otherwise.
func openStores(cfg Config) (*identityStore, *store.Store, error) {
	if cfg.dev() {
		identity, err := openIdentityStoreMemory()
		if err != nil {
			return nil, nil, err
		}
		return identity, store.OpenMemory(), nil
	}
	identity, err := openIdentityStore(filepath.Join(cfg.DataDir, "identity.sqlite"))
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "db"))
	if err != nil {
		identity.db.Close()
		return nil, nil, err
	}
	return identity, st, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.dev() {
		if s.handleDevStatic(w, r) {
			return
		}
	} else if s.handleSubdomainSite(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// handleDevStatic serves the live DevRoot directory for ordinary GET/HEAD
// requests, leaving API and built-in asset routes to the mux. It is the dev
// equivalent of subdomain site serving.
func (s *Server) handleDevStatic(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	if p == "/pods.js" || p == "/install.sh" || p == "/healthz" || p == "/favicon.ico" ||
		strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/sites/") {
		return false
	}
	s.serveDevFile(w, r, strings.TrimPrefix(p, "/"))
	return true
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	// Unauthenticated by design (Quick-style: sites are open to everyone).
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleLanding)
	mux.HandleFunc("GET /pods.js", s.handlePodsJS)
	mux.HandleFunc("GET /install.sh", s.handleInstallSH)
	mux.HandleFunc("GET /favicon.ico", s.staticAsset("favicon.ico", "image/x-icon"))
	mux.HandleFunc("GET /favicon-32.png", s.staticAsset("favicon-32.png", "image/png"))
	mux.HandleFunc("GET /favicon-16.png", s.staticAsset("favicon-16.png", "image/png"))
	mux.HandleFunc("GET /apple-touch-icon.png", s.staticAsset("apple-touch-icon.png", "image/png"))
	mux.HandleFunc("GET /icon-192.png", s.staticAsset("icon-192.png", "image/png"))
	mux.HandleFunc("GET /icon-512.png", s.staticAsset("icon-512.png", "image/png"))
	mux.HandleFunc("GET /sites/{site}", s.handleSiteRedirect)
	mux.HandleFunc("GET /sites/{site}/{path...}", s.handleSiteFile)

	// Site APIs are open and scoped by the single site subdomain. Publishing and
	// management endpoints authorize against GitHub OAuth users and API tokens.
	mux.HandleFunc("/api/", s.handleAPINotFound) // JSON 404 fallback
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/auth/providers", s.handleAuthProviders)
	mux.HandleFunc("GET /api/auth/login/{provider}", s.handleOAuthLogin)
	mux.HandleFunc("GET /api/auth/callback/{provider}", s.handleOAuthCallback)
	mux.HandleFunc("POST /api/auth/github/device/start", s.handleGitHubDeviceStart)
	mux.HandleFunc("POST /api/auth/github/device/poll", s.handleGitHubDevicePoll)
	mux.HandleFunc("POST /api/auth/refresh", s.handleAuthRefresh)
	mux.HandleFunc("POST /api/auth/logout", s.handleOAuthLogout)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/sites", s.handleSiteList)
	mux.HandleFunc("PUT /api/sites/{name}", s.handleSiteDeploy)
	mux.HandleFunc("DELETE /api/sites/{name}", s.handleSiteDelete)
	mux.HandleFunc("GET /api/db", s.handleCollections)
	mux.HandleFunc("GET /api/db/{coll}", s.handleQuery)
	mux.HandleFunc("POST /api/db/{coll}", s.handleDocCreate)
	mux.HandleFunc("DELETE /api/db/{coll}", s.handleCollectionDrop)
	mux.HandleFunc("GET /api/db/{coll}/{id}", s.handleDocGet)
	mux.HandleFunc("PUT /api/db/{coll}/{id}", s.handleDocSet)
	mux.HandleFunc("PATCH /api/db/{coll}/{id}", s.handleDocPatch)
	mux.HandleFunc("DELETE /api/db/{coll}/{id}", s.handleDocDelete)

	s.mux = mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{OK: true})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// baseURL returns the configured public URL, or one derived from the request.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// badRequest marks an error as a client error (HTTP 400).
type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

func badRequestf(format string, args ...any) error {
	return &badRequest{msg: fmt.Sprintf(format, args...)}
}

// respondErr maps an error to a JSON error response: 400 for badRequest,
// 500 otherwise.
func respondErr(w http.ResponseWriter, err error) {
	var br *badRequest
	if errors.As(err, &br) {
		writeError(w, http.StatusBadRequest, "%s", br.msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal error: %v", err)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, api.Error{Error: fmt.Sprintf(format, args...)})
}
