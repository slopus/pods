// Package server implements the podbay HTTP server: bearer-authenticated
// JSON API, tar.gz site deploys, static site serving and embedded web assets.
package server

import (
	"crypto/subtle"
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
	DataDir   string // data directory (sites/ and tenant-scoped db.json live here)
	Secret    string // bearer secret for /api/*
	PublicURL string // optional base URL for printed site URLs
}

// Server is the podbay HTTP handler.
type Server struct {
	cfg     Config
	store   *store.Store
	events  *eventHub
	mux     *http.ServeMux
	landing *template.Template
	podsJS  []byte
}

// New creates the data layout, opens the document store and builds the
// route table.
func New(cfg Config) (*Server, error) {
	if cfg.Secret == "" {
		return nil, errors.New("server: secret must not be empty")
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "sites"), 0o755); err != nil {
		return nil, fmt.Errorf("server: create sites dir: %w", err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "db.json"))
	if err != nil {
		return nil, err
	}
	landing, err := template.ParseFS(webFS, "web/index.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse landing page: %w", err)
	}
	podsJS, err := webFS.ReadFile("web/pods.js")
	if err != nil {
		return nil, fmt.Errorf("server: read pods.js: %w", err)
	}
	s := &Server{cfg: cfg, store: st, events: newEventHub(), landing: landing, podsJS: podsJS}
	s.routes()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.handleSubdomainSite(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	// Unauthenticated by design (Quick-style: sites are open to everyone).
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleLanding)
	mux.HandleFunc("GET /pods.js", s.handlePodsJS)
	mux.HandleFunc("GET /sites/{site}", s.handleSiteRedirect)
	mux.HandleFunc("GET /sites/{site}/{path...}", s.handleSiteFile)
	mux.HandleFunc("GET /sites/{team}/{site}", s.handleTeamSiteRedirect)
	mux.HandleFunc("GET /sites/{team}/{site}/{path...}", s.handleTeamSiteFile)

	// Bearer-authenticated API.
	mux.HandleFunc("/api/", s.auth(s.handleAPINotFound)) // JSON 404 fallback
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /api/sites", s.auth(s.handleSiteList))
	mux.HandleFunc("PUT /api/sites/{name}", s.publicOrAuth(s.handleSiteDeploy))
	mux.HandleFunc("DELETE /api/sites/{name}", s.auth(s.handleSiteDelete))
	mux.HandleFunc("PUT /api/teams/{team}/sites/{name}", s.teamPublishAuth(s.handleTeamSiteDeployRoute))
	mux.HandleFunc("DELETE /api/teams/{team}/sites/{name}", s.auth(s.handleTeamSiteDeleteRoute))
	mux.HandleFunc("GET /api/db", s.auth(s.handleCollections))
	mux.HandleFunc("GET /api/db/{coll}", s.auth(s.handleQuery))
	mux.HandleFunc("POST /api/db/{coll}", s.auth(s.handleDocCreate))
	mux.HandleFunc("DELETE /api/db/{coll}", s.auth(s.handleCollectionDrop))
	mux.HandleFunc("GET /api/db/{coll}/{id}", s.auth(s.handleDocGet))
	mux.HandleFunc("PUT /api/db/{coll}/{id}", s.auth(s.handleDocSet))
	mux.HandleFunc("PATCH /api/db/{coll}/{id}", s.auth(s.handleDocPatch))
	mux.HandleFunc("DELETE /api/db/{coll}/{id}", s.auth(s.handleDocDelete))

	s.mux = mux
}

// auth wraps an API handler with constant-time bearer-secret verification.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hasValidBearer(r, s.cfg.Secret) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (s *Server) publicOrAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hasValidBearer(r, s.cfg.Secret) || r.PathValue("team") == "" {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
}

func (s *Server) teamPublishAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("team") == publicTeam || hasValidBearer(r, s.cfg.Secret) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
}

func hasValidBearer(r *http.Request, secret string) bool {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	return strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
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
