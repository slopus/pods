package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/slopus/pods/internal/api"
)

type eventHub struct {
	mu          sync.Mutex
	nextID      int64
	subscribers map[chan api.UpdateEvent]string
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: make(map[chan api.UpdateEvent]string)}
}

func (h *eventHub) publish(ev api.UpdateEvent) {
	h.mu.Lock()
	h.nextID++
	ev.ID = h.nextID
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	for ch, tenant := range h.subscribers {
		if tenant != "" && tenant != ev.Pod {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *eventHub) subscribe(tenant string) (<-chan api.UpdateEvent, func()) {
	ch := make(chan api.UpdateEvent, 32)
	h.mu.Lock()
	h.subscribers[ch] = tenant
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

func (s *Server) publish(ev api.UpdateEvent) {
	if ev.Pod == "" {
		return
	}
	s.events.publish(ev)
}

// GET /api/events
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	tenant := ""
	if s.cfg.dev() {
		tenant = s.cfg.DevSite
	} else if site, ok := s.siteFromHost(r.Host); ok {
		tenant = site
	}
	s.streamEvents(w, r, tenant)
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, tenant string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	events, cancel := s.events.subscribe(tenant)
	defer cancel()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "id: %d\nevent: update\ndata: %s\n\n", ev.ID, data)
			flusher.Flush()
		}
	}
}
