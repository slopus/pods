package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slopus/pods/internal/api"
)

func TestAPIRequiresAuthButHealthDoesNot(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/sites without auth = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rr.Code)
	}
}

func TestDeployAndServeSite(t *testing.T) {
	app := newTestApp(t)
	archive := makeTarGz(t, map[string]string{
		"index.html":     "<h1>Hello pod</h1>",
		"assets/app.js":  "console.log('ok')",
		"nested/page.js": "export default 1",
	})

	req := authedRequest(http.MethodPut, "/api/teams/ops/sites/demo", bytes.NewReader(archive))
	req.Host = "pods.test"
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("deploy status = %d body=%s", rr.Code, rr.Body.String())
	}

	var deploy api.DeployResult
	if err := json.Unmarshal(rr.Body.Bytes(), &deploy); err != nil {
		t.Fatalf("decode deploy: %v", err)
	}
	if deploy.Site.Name != "demo" || deploy.Site.Team != "ops" || deploy.Site.Files != 3 {
		t.Fatalf("deploy result = %+v", deploy)
	}
	if deploy.URL != "http://demo.ops.pods.test/" {
		t.Fatalf("deploy URL = %q", deploy.URL)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "demo.ops.pods.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Hello pod") {
		t.Fatalf("site response status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestDeployRejectsZipSlip(t *testing.T) {
	app := newTestApp(t)
	archive := makeTarGz(t, map[string]string{"../escape.txt": "nope"})

	req := authedRequest(http.MethodPut, "/api/sites/demo", bytes.NewReader(archive))
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("zip-slip deploy status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
}

func TestDBCRUDOverHTTP(t *testing.T) {
	app := newTestApp(t)

	req := authedRequest(http.MethodPost, "/api/db/posts", strings.NewReader(`{"status":"draft","score":2}`))
	req.Host = "demo.public.example.test"
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created api.Doc
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" || created["status"] != "draft" {
		t.Fatalf("created = %+v", created)
	}

	req = authedRequest(http.MethodPatch, "/api/db/posts/"+id, strings.NewReader(`{"status":"published"}`))
	req.Host = "demo.public.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d body=%s", rr.Code, rr.Body.String())
	}

	req = authedRequest(http.MethodGet, "/api/db/posts?where=status=published&sort=-score&limit=1", nil)
	req.Host = "demo.public.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("query status = %d body=%s", rr.Code, rr.Body.String())
	}
	var query api.QueryResult
	if err := json.Unmarshal(rr.Body.Bytes(), &query); err != nil {
		t.Fatalf("decode query: %v", err)
	}
	if query.Total != 1 || len(query.Docs) != 1 || query.Docs[0]["id"] != id {
		t.Fatalf("query = %+v", query)
	}

	req = authedRequest(http.MethodDelete, "/api/db/posts/"+id, nil)
	req.Host = "demo.public.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPodEventStream(t *testing.T) {
	app := newTestApp(t)
	srv := httptest.NewServer(app)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Host = "demo.public.example.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("event stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d", resp.StatusCode)
	}

	eventCh := make(chan api.UpdateEvent, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev api.UpdateEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				eventCh <- ev
				return
			}
		}
	}()

	mutateReq, err := http.NewRequest(http.MethodPost, srv.URL+"/api/db/posts", strings.NewReader(`{"title":"streamed"}`))
	if err != nil {
		t.Fatalf("NewRequest mutate: %v", err)
	}
	mutateReq.Header.Set("Authorization", "Bearer secret")
	mutateReq.Host = "demo.public.example.test"
	mutateResp, err := http.DefaultClient.Do(mutateReq)
	if err != nil {
		t.Fatalf("mutate request: %v", err)
	}
	_ = mutateResp.Body.Close()
	if mutateResp.StatusCode != http.StatusCreated {
		t.Fatalf("mutate status = %d", mutateResp.StatusCode)
	}

	select {
	case ev := <-eventCh:
		if ev.Pod != "demo" || ev.Team != "public" || ev.Type != "doc.created" || ev.Collection != "posts" || ev.Doc["title"] != "streamed" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func newTestApp(t *testing.T) *Server {
	t.Helper()
	app, err := New(Config{DataDir: t.TempDir(), Secret: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return app
}

func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
