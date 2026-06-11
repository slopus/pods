package server

import (
	"net/http"

	"github.com/slopus/pods/internal/api"
)

type landingData struct {
	Sites []api.Site
}

func (s *Server) handleLanding(w http.ResponseWriter, _ *http.Request) {
	sites, err := s.listSites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.landing.Execute(w, landingData{Sites: sites}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handlePodsJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(s.podsJS)
}
