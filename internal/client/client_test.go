package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/slopus/pods/internal/api"
	"github.com/slopus/pods/internal/client"
)

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding stub response: %v", err)
	}
}

func TestSitesSendsBearerAndDecodes(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sites", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(t, w, http.StatusOK, api.SiteList{Sites: []api.Site{
			{Name: "blog", Files: 2, Bytes: 1024},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Trailing slash on the endpoint must be tolerated.
	c := client.New(srv.URL+"/", "s3cret")
	sites, err := c.Sites(context.Background())
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer s3cret")
	}
	if len(sites) != 1 || sites[0].Name != "blog" || sites[0].Files != 2 || sites[0].Bytes != 1024 {
		t.Errorf("unexpected sites: %+v", sites)
	}
}

func TestHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, api.Health{OK: true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h, err := client.New(srv.URL, "s").Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.OK {
		t.Error("Health.OK = false, want true")
	}
}

func TestDeploy(t *testing.T) {
	var gotName, gotContentType, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/sites/{name}", func(w http.ResponseWriter, r *http.Request) {
		gotName = r.PathValue("name")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(t, w, http.StatusCreated, api.DeployResult{
			Site: api.Site{Name: gotName, Files: 3, Bytes: int64(len(b))},
			URL:  "http://example.test/sites/" + gotName + "/",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := client.New(srv.URL, "s").Deploy(context.Background(), "public", "blog", strings.NewReader("fake-tarball"))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if gotName != "blog" {
		t.Errorf("name = %q, want %q", gotName, "blog")
	}
	if gotContentType != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", gotContentType)
	}
	if gotBody != "fake-tarball" {
		t.Errorf("body = %q, want %q", gotBody, "fake-tarball")
	}
	if res.Site.Files != 3 || res.URL != "http://example.test/sites/blog/" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestAPIErrorDecoding(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/sites/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, api.Error{Error: `site "ghost" not found`})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := client.New(srv.URL, "s").DeleteSite(context.Background(), "public", "ghost")
	if err == nil {
		t.Fatal("DeleteSite: want error, got nil")
	}
	if got, want := err.Error(), `site "ghost" not found`; got != want {
		t.Errorf("err = %q, want %q", got, want)
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *client.APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", apiErr.StatusCode)
	}
}

func TestAPIErrorNonJSONBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sites", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := client.New(srv.URL, "s").Sites(context.Background())
	if err == nil {
		t.Fatal("Sites: want error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %q, want it to mention the status", err)
	}
}

func TestQueryParams(t *testing.T) {
	var gotQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/db/{coll}", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		writeJSON(t, w, http.StatusOK, api.QueryResult{
			Docs:  []api.Doc{{"id": "a1", "status": "draft"}},
			Total: 7,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := client.New(srv.URL, "s").Query(context.Background(), "posts", client.QueryOptions{
		Where:  []string{"status=draft", "author=bob"},
		Sort:   "-created_at",
		Limit:  10,
		Offset: 5,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if want := []string{"status=draft", "author=bob"}; !equalSlices(gotQuery["where"], want) {
		t.Errorf("where = %v, want %v", gotQuery["where"], want)
	}
	if got := gotQuery.Get("sort"); got != "-created_at" {
		t.Errorf("sort = %q, want -created_at", got)
	}
	if got := gotQuery.Get("limit"); got != "10" {
		t.Errorf("limit = %q, want 10", got)
	}
	if got := gotQuery.Get("offset"); got != "5" {
		t.Errorf("offset = %q, want 5", got)
	}
	if res.Total != 7 || len(res.Docs) != 1 || res.Docs[0]["id"] != "a1" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestQueryOmitsDefaultParams(t *testing.T) {
	var gotRawQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/db/{coll}", func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		writeJSON(t, w, http.StatusOK, api.QueryResult{Docs: []api.Doc{}, Total: 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := client.New(srv.URL, "s").Query(context.Background(), "posts", client.QueryOptions{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotRawQuery != "" {
		t.Errorf("raw query = %q, want empty", gotRawQuery)
	}
}

func TestDocCRUD(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/db/{coll}", func(w http.ResponseWriter, r *http.Request) {
		var doc api.Doc
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			t.Errorf("decoding create body: %v", err)
		}
		doc["id"] = "abc123"
		writeJSON(t, w, http.StatusCreated, doc)
	})
	mux.HandleFunc("GET /api/db/{coll}/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, api.Doc{"id": r.PathValue("id"), "title": "hi"})
	})
	mux.HandleFunc("PUT /api/db/{coll}/{id}", func(w http.ResponseWriter, r *http.Request) {
		var doc api.Doc
		_ = json.NewDecoder(r.Body).Decode(&doc)
		doc["id"] = r.PathValue("id")
		writeJSON(t, w, http.StatusOK, doc)
	})
	mux.HandleFunc("PATCH /api/db/{coll}/{id}", func(w http.ResponseWriter, r *http.Request) {
		var doc api.Doc
		_ = json.NewDecoder(r.Body).Decode(&doc)
		doc["id"] = r.PathValue("id")
		doc["patched"] = true
		writeJSON(t, w, http.StatusOK, doc)
	})
	mux.HandleFunc("DELETE /api/db/{coll}/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/db/{coll}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/db", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, api.CollectionList{Collections: []api.Collection{
			{Name: "posts", Count: 4},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	c := client.New(srv.URL, "s")

	created, err := c.CreateDoc(ctx, "posts", api.Doc{"title": "hello"})
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	if created["id"] != "abc123" || created["title"] != "hello" {
		t.Errorf("created = %v", created)
	}

	got, err := c.GetDoc(ctx, "posts", "abc123")
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	if got["id"] != "abc123" || got["title"] != "hi" {
		t.Errorf("got = %v", got)
	}

	set, err := c.SetDoc(ctx, "posts", "abc123", api.Doc{"title": "replaced"})
	if err != nil {
		t.Fatalf("SetDoc: %v", err)
	}
	if set["title"] != "replaced" || set["id"] != "abc123" {
		t.Errorf("set = %v", set)
	}

	patched, err := c.PatchDoc(ctx, "posts", "abc123", api.Doc{"extra": "yes"})
	if err != nil {
		t.Fatalf("PatchDoc: %v", err)
	}
	if patched["patched"] != true || patched["extra"] != "yes" {
		t.Errorf("patched = %v", patched)
	}

	if err := c.DeleteDoc(ctx, "posts", "abc123"); err != nil {
		t.Fatalf("DeleteDoc: %v", err)
	}
	if err := c.DropCollection(ctx, "posts"); err != nil {
		t.Fatalf("DropCollection: %v", err)
	}

	colls, err := c.Collections(ctx)
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if len(colls) != 1 || colls[0].Name != "posts" || colls[0].Count != 4 {
		t.Errorf("collections = %+v", colls)
	}
}

func TestSiteURL(t *testing.T) {
	c := client.New("http://example.test:7777/", "s")
	if got, want := c.SiteURL("blog"), "http://example.test:7777/sites/blog/"; got != want {
		t.Errorf("SiteURL = %q, want %q", got, want)
	}
	if got, want := c.TeamSiteURL("ops", "blog"), "http://blog.ops.example.test:7777/"; got != want {
		t.Errorf("TeamSiteURL = %q, want %q", got, want)
	}
	if got, want := c.Endpoint(), "http://example.test:7777"; got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
