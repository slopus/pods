package server

import (
	"net/http"

	"github.com/slopus/pods/internal/api"
)

// repoURL is the public home of the project, shown on the landing page.
const repoURL = "https://github.com/slopus/pods"

type landingData struct {
	Sites    []api.Site
	RepoURL  string
	BaseHost string // host that site subdomains hang off, e.g. "podbay.dev"
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	sites, err := s.listSites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := landingData{Sites: sites, RepoURL: repoURL, BaseHost: s.landingBaseHost(r)}
	if err := s.landing.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// landingBaseHost is the host that site subdomains hang off: the configured
// public base host, or the request host when self-hosted without one.
func (s *Server) landingBaseHost(r *http.Request) string {
	if h := s.publicBaseHost(); h != "" {
		return h
	}
	return hostOnly(r.Host)
}

func (s *Server) handlePodsJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(s.podsJS)
}

// GET /install.sh serves the CLI installer so users can run
// `curl -fsSL https://podbay.dev/install.sh | sh`.
func (s *Server) handleInstallSH(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write(s.installSH)
}

// staticAsset returns a handler that serves an embedded web/ file with a fixed
// content type. The bytes are read once at route-registration time.
func (s *Server) staticAsset(name, contentType string) http.HandlerFunc {
	data, _ := webFS.ReadFile("web/" + name)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	}
}
