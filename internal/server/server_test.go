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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slopus/pods/internal/api"
)

func TestPublicAPIAndHealth(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/sites without auth = %d, want 200", rr.Code)
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

	req := authedRequest(http.MethodPut, "/api/sites/demo", bytes.NewReader(archive))
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
	if deploy.Site.Name != "demo" || deploy.Site.OwnerID != "admin" || deploy.Site.Files != 3 {
		t.Fatalf("deploy result = %+v", deploy)
	}
	if deploy.URL != "http://demo.pods.test/" {
		t.Fatalf("deploy URL = %q", deploy.URL)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "demo.pods.test"
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
	deployTestSite(t, app, "demo")

	req := authedRequest(http.MethodPost, "/api/db/posts", strings.NewReader(`{"status":"draft","score":2}`))
	req.Host = "demo.example.test"
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
	req.Host = "demo.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d body=%s", rr.Code, rr.Body.String())
	}

	req = authedRequest(http.MethodGet, "/api/db/posts?where=status=published&sort=-score&limit=1", nil)
	req.Host = "demo.example.test"
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
	req.Host = "demo.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDBRequiresDeployedSite(t *testing.T) {
	app := newTestApp(t)

	req := authedRequest(http.MethodPost, "/api/db/posts", strings.NewReader(`{"n":1}`))
	req.Host = "ghost.example.test"
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("db write on undeployed site = %d, want 404 body=%s", rr.Code, rr.Body.String())
	}
}

func TestPodEventStream(t *testing.T) {
	app := newTestApp(t)
	deployTestSite(t, app, "demo")
	srv := httptest.NewServer(app)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "demo.example.test"
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
	mutateReq.Host = "demo.example.test"
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
		if ev.Pod != "demo" || ev.Type != "doc.created" || ev.Collection != "posts" || ev.Doc["title"] != "streamed" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSiteOwnerControlsDeployButContentAndDBArePublic(t *testing.T) {
	app := newTestAppWithAuth(t, `{
	  "users": [
	    {"id": "alice", "login": "alice", "tokens": ["alice-token"]},
	    {"id": "riley", "login": "riley", "tokens": ["riley-token"]}
	  ]
	}`)
	archive := makeTarGz(t, map[string]string{"index.html": "<h1>Private name, public content</h1>"})

	req := httptest.NewRequest(http.MethodPut, "/api/sites/demo", bytes.NewReader(archive))
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("deploy without auth status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/sites/demo", bytes.NewReader(archive))
	req.Header.Set("Authorization", "Bearer alice-token")
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("deploy as owner status = %d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/sites/demo", bytes.NewReader(archive))
	req.Header.Set("Authorization", "Bearer riley-token")
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("deploy as non-owner status = %d, want 403 body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "demo.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Private name, public content") {
		t.Fatalf("site without auth status=%d body=%q", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/db/views", strings.NewReader(`{"count":1}`))
	req.Host = "demo.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("public DB write status = %d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Host = "demo.example.test"
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/me status = %d body=%s", rr.Code, rr.Body.String())
	}
	var me api.Me
	if err := json.Unmarshal(rr.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if !me.Authenticated || me.User == nil || me.User.ID != "alice" || me.Site != "demo" {
		t.Fatalf("me = %+v", me)
	}
}

func TestGitHubSessionAuthenticates(t *testing.T) {
	auth, err := buildAuthenticator(authFile{
		OAuth: authOAuthFile{SessionSecret: "session-secret"},
	}, "")
	if err != nil {
		t.Fatalf("buildAuthenticator: %v", err)
	}
	value, err := auth.secure.Encode(sessionCookieName, oauthSession{
		Provider: githubProviderID,
		Subject:  "123",
		Login:    "alice",
		Email:    "alice@example.test",
		Name:     "Alice",
		Expires:  time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})

	user, ok := auth.authenticate(req)
	if !ok {
		t.Fatal("GitHub session did not authenticate")
	}
	if user.ID != "github:123" || user.Login != "alice" {
		t.Fatalf("user = %+v", user)
	}
}

func TestJWTLoginRefreshAndRevocation(t *testing.T) {
	app := newTestApp(t)
	user, err := app.identity.upsertUser(providerIdentity{
		Provider: "github", Subject: "123", Login: "alice", Name: "Alice",
	})
	if err != nil {
		t.Fatalf("upsertUser: %v", err)
	}
	token, expiresAt, err := app.auth.issueToken(user, time.Now())
	if err != nil {
		t.Fatalf("issueToken: %v", err)
	}
	if time.Until(expiresAt) < 29*24*time.Hour {
		t.Fatalf("expiresAt = %v, want ~30 days out", expiresAt)
	}

	// The JWT authenticates as the identity user.
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	var me api.Me
	if err := json.Unmarshal(rr.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if !me.Authenticated || me.User == nil || me.User.ID != "github:123" || me.User.Login != "alice" {
		t.Fatalf("me = %+v", me)
	}

	// Refresh exchanges the token for a fresh, working one.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", rr.Code, rr.Body.String())
	}
	var refreshed api.TokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if refreshed.Token == "" || refreshed.User == nil || refreshed.User.ID != "github:123" {
		t.Fatalf("refresh = %+v", refreshed)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+refreshed.Token)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	me = api.Me{}
	if err := json.Unmarshal(rr.Body.Bytes(), &me); err != nil || !me.Authenticated {
		t.Fatalf("refreshed token rejected: err=%v me=%+v", err, me)
	}

	// Tampered tokens are rejected.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token+"x")
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered refresh status = %d", rr.Code)
	}

	// Expired tokens are rejected.
	expired, _, err := app.auth.issueToken(user, time.Now().Add(-31*24*time.Hour))
	if err != nil {
		t.Fatalf("issueToken expired: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+expired)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired refresh status = %d", rr.Code)
	}
}

func TestSiteDeleteRemovesDocumentDB(t *testing.T) {
	app := newTestApp(t)
	deployTestSite(t, app, "demo")

	req := authedRequest(http.MethodPost, "/api/db/notes", strings.NewReader(`{"v":1}`))
	req.Host = "demo.example.test"
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	dbPath := filepath.Join(app.cfg.DataDir, "db", "demo.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("per-site db missing after write: %v", err)
	}

	req = authedRequest(http.MethodDelete, "/api/sites/demo", nil)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete site status = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("per-site db still present after site delete: %v", err)
	}
}

func TestLandingPageRenders(t *testing.T) {
	app := newTestApp(t)
	deployTestSite(t, app, "demo")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("landing status = %d body=%s", rr.Code, rr.Body.String()[:min(len(rr.Body.String()), 400)])
	}
	body := rr.Body.String()
	for _, want := range []string{"demo", "#docs", "github.com/slopus/pods"} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing page missing %q", want)
		}
	}
}

func deployTestSite(t *testing.T, app *Server, name string) {
	t.Helper()
	archive := makeTarGz(t, map[string]string{"index.html": "<h1>" + name + "</h1>"})
	req := authedRequest(http.MethodPut, "/api/sites/"+name, bytes.NewReader(archive))
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("deploy %s status = %d body=%s", name, rr.Code, rr.Body.String())
	}
}

func TestSessionCookieCannotDeployOrDelete(t *testing.T) {
	app := newTestApp(t)
	// A valid browser session for a GitHub user.
	cookieVal, err := app.auth.secure.Encode(sessionCookieName, oauthSession{
		Provider: githubProviderID, Subject: "999", Login: "mallory",
		Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: cookieVal}

	// The cookie identifies the user on read endpoints...
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	var me api.Me
	_ = json.Unmarshal(rr.Body.Bytes(), &me)
	if !me.Authenticated || me.User == nil || me.User.Login != "mallory" {
		t.Fatalf("cookie did not authenticate /api/me: %+v", me)
	}

	// ...but must NOT authorize a deploy (CSRF defense: bearer-only mutations).
	archive := makeTarGz(t, map[string]string{"index.html": "x"})
	req = httptest.NewRequest(http.MethodPut, "/api/sites/victim", bytes.NewReader(archive))
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("cookie-authorized deploy = %d, want 401 body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/sites/victim", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("cookie-authorized delete = %d, want 401", rr.Code)
	}
}

func TestAdminRedeployPreservesOwner(t *testing.T) {
	app := newTestAppWithAuth(t, `{
	  "users": [
	    {"id": "alice", "login": "alice", "tokens": ["alice-token"]},
	    {"id": "admin", "login": "admin", "admin": true, "tokens": ["admin-token"]}
	  ]
	}`)
	archive := makeTarGz(t, map[string]string{"index.html": "<h1>alice</h1>"})

	deploy := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/sites/blog", bytes.NewReader(archive))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		app.ServeHTTP(rr, req)
		return rr
	}

	if rr := deploy("alice-token"); rr.Code != http.StatusCreated {
		t.Fatalf("alice deploy = %d body=%s", rr.Code, rr.Body.String())
	}
	// Admin redeploys (e.g. a hotfix); ownership must stay with alice.
	if rr := deploy("admin-token"); rr.Code != http.StatusOK {
		t.Fatalf("admin redeploy = %d body=%s", rr.Code, rr.Body.String())
	}
	owner, found, err := app.identity.siteOwner("blog")
	if err != nil || !found || owner.ID != "alice" {
		t.Fatalf("owner after admin redeploy = %+v found=%v err=%v", owner, found, err)
	}
	// Alice can still redeploy her own site.
	if rr := deploy("alice-token"); rr.Code != http.StatusOK {
		t.Fatalf("alice re-redeploy = %d body=%s", rr.Code, rr.Body.String())
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

func newTestAppWithAuth(t *testing.T, authJSON string) *Server {
	t.Helper()
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(authJSON), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	app, err := New(Config{DataDir: dir, Secret: "secret", AuthFile: authPath})
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
