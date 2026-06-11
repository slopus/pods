package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushRecorder is an http.ResponseWriter that records Flush calls.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *flushRecorder) Flush() { f.flushed++ }

func TestResponseRecorderForwardsFlush(t *testing.T) {
	// The SSE handler does w.(http.Flusher); the logging wrapper must keep
	// that working, otherwise GET /api/events returns "streaming unsupported".
	underlying := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec := &responseRecorder{ResponseWriter: underlying}

	f, ok := any(rec).(http.Flusher)
	if !ok {
		t.Fatal("responseRecorder does not implement http.Flusher")
	}
	f.Flush()
	if underlying.flushed != 1 {
		t.Fatalf("flush forwarded %d times, want 1", underlying.flushed)
	}
}
