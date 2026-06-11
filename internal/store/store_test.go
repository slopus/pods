package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/slopus/pods/internal/api"
)

func TestStoreCRUDAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }

	created, err := s.Create("public/demo", "posts", api.Doc{
		"id":         "client-id",
		"created_at": "client-created",
		"title":      "hello",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := created[FieldID].(string)
	if id == "" || id == "client-id" {
		t.Fatalf("id = %q, want generated id", id)
	}
	if created[FieldCreatedAt] != "2026-06-10T12:00:00Z" {
		t.Fatalf("created_at = %v", created[FieldCreatedAt])
	}

	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 1, 0, 0, time.UTC) }
	patched, ok, err := s.Patch("public/demo", "posts", id, api.Doc{
		"title":      "patched",
		"updated_at": "client-updated",
	})
	if err != nil || !ok {
		t.Fatalf("Patch ok=%v err=%v", ok, err)
	}
	if patched["title"] != "patched" || patched[FieldUpdatedAt] != "2026-06-10T12:01:00Z" {
		t.Fatalf("patched doc = %+v", patched)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("public/demo", "posts", id)
	if !ok {
		t.Fatal("reopened store did not contain created doc")
	}
	if got["title"] != "patched" {
		t.Fatalf("persisted title = %v", got["title"])
	}
}

func TestStoreQuery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Set("public/demo", "posts", "a", api.Doc{"status": "draft", "score": 2.0, "live": false}); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if _, err := s.Set("public/demo", "posts", "b", api.Doc{"status": "draft", "score": 9.0, "live": true}); err != nil {
		t.Fatalf("Set b: %v", err)
	}
	if _, err := s.Set("public/demo", "posts", "c", api.Doc{"status": "published", "score": 5.0, "live": true}); err != nil {
		t.Fatalf("Set c: %v", err)
	}
	if _, err := s.Set("ops/demo", "posts", "z", api.Doc{"status": "draft", "score": 99.0}); err != nil {
		t.Fatalf("Set other: %v", err)
	}

	res := s.Query("public/demo", "posts", Query{
		Where: []Where{{Field: "status", Value: "draft"}},
		Sort:  "-score",
		Limit: 1,
	})
	if res.Total != 2 || len(res.Docs) != 1 || res.Docs[0][FieldID] != "b" {
		t.Fatalf("draft query = %+v", res)
	}

	res = s.Query("public/demo", "posts", Query{Where: []Where{{Field: "live", Value: "false"}}})
	if res.Total != 1 || res.Docs[0][FieldID] != "a" {
		t.Fatalf("bool query = %+v", res)
	}

	res = s.Query("public/demo", "posts", Query{
		Where:  []Where{{Field: "score", Value: "5"}},
		Offset: 0,
	})
	if res.Total != 1 || res.Docs[0][FieldID] != "c" {
		t.Fatalf("numeric query = %+v", res)
	}

	res = s.Query("ops/demo", "posts", Query{})
	if res.Total != 1 || res.Docs[0][FieldID] != "z" {
		t.Fatalf("tenant isolation query = %+v", res)
	}
}
