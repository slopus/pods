package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/slopus/pods/internal/api"
)

func TestStoreCRUDAndPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }

	created, err := s.Create("demo", "posts", api.Doc{
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
	patched, ok, err := s.Patch("demo", "posts", id, api.Doc{
		"title":      "patched",
		"updated_at": "client-updated",
	})
	if err != nil || !ok {
		t.Fatalf("Patch ok=%v err=%v", ok, err)
	}
	if patched["title"] != "patched" || patched[FieldUpdatedAt] != "2026-06-10T12:01:00Z" {
		t.Fatalf("patched doc = %+v", patched)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("demo", "posts", id)
	if !ok {
		t.Fatal("reopened store did not contain created doc")
	}
	if got["title"] != "patched" {
		t.Fatalf("persisted title = %v", got["title"])
	}
}

func TestStoreQuery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Set("demo", "posts", "a", api.Doc{"status": "draft", "score": 2.0, "live": false}); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if _, err := s.Set("demo", "posts", "b", api.Doc{"status": "draft", "score": 9.0, "live": true}); err != nil {
		t.Fatalf("Set b: %v", err)
	}
	if _, err := s.Set("demo", "posts", "c", api.Doc{"status": "published", "score": 5.0, "live": true}); err != nil {
		t.Fatalf("Set c: %v", err)
	}
	if _, err := s.Set("other", "posts", "z", api.Doc{"status": "draft", "score": 99.0}); err != nil {
		t.Fatalf("Set other: %v", err)
	}

	res := s.Query("demo", "posts", Query{
		Where: []Where{{Field: "status", Value: "draft"}},
		Sort:  "-score",
		Limit: 1,
	})
	if res.Total != 2 || len(res.Docs) != 1 || res.Docs[0][FieldID] != "b" {
		t.Fatalf("draft query = %+v", res)
	}

	res = s.Query("demo", "posts", Query{Where: []Where{{Field: "live", Value: "false"}}})
	if res.Total != 1 || res.Docs[0][FieldID] != "a" {
		t.Fatalf("bool query = %+v", res)
	}

	res = s.Query("demo", "posts", Query{
		Where:  []Where{{Field: "score", Value: "5"}},
		Offset: 0,
	})
	if res.Total != 1 || res.Docs[0][FieldID] != "c" {
		t.Fatalf("numeric query = %+v", res)
	}

	res = s.Query("other", "posts", Query{})
	if res.Total != 1 || res.Docs[0][FieldID] != "z" {
		t.Fatalf("site isolation query = %+v", res)
	}
}

func TestStorePerSiteFilesAndDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Set("alpha", "posts", "a", api.Doc{"n": 1.0}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha.sqlite")); err != nil {
		t.Fatalf("per-site db file missing: %v", err)
	}

	// Reads on unknown sites stay empty and never create database files.
	if _, ok := s.Get("ghost", "posts", "a"); ok {
		t.Fatal("Get on unknown site returned a doc")
	}
	if colls := s.Collections("ghost"); len(colls) != 0 {
		t.Fatalf("Collections on unknown site = %+v", colls)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("read created a db file for unknown site: %v", err)
	}

	if err := s.DeleteSite("alpha"); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("db file still present after DeleteSite: %v", err)
	}
	if _, ok := s.Get("alpha", "posts", "a"); ok {
		t.Fatal("doc still readable after DeleteSite")
	}
}

func TestStoreMigratesLegacyGlobalDB(t *testing.T) {
	dataDir := t.TempDir()
	legacyPath := filepath.Join(dataDir, "db.sqlite")
	legacy, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE docs (
		pod TEXT NOT NULL, collection TEXT NOT NULL, id TEXT NOT NULL,
		doc TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		PRIMARY KEY (pod, collection, id))`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO docs VALUES
		('demo', 'views', 'a', '{"id":"a","visitor":"one","created_at":"2026-06-10T12:00:00Z","updated_at":"2026-06-10T12:00:00Z"}', '2026-06-10T12:00:00Z', '2026-06-10T12:00:00Z'),
		('not/a/site', 'views', 'b', '{"id":"b"}', '2026-06-10T12:00:00Z', '2026-06-10T12:00:00Z')`); err != nil {
		t.Fatalf("insert legacy docs: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	s, err := Open(filepath.Join(dataDir, "db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, ok := s.Get("demo", "views", "a")
	if !ok {
		t.Fatal("migrated doc not found")
	}
	if got["visitor"] != "one" || got[FieldCreatedAt] != "2026-06-10T12:00:00Z" {
		t.Fatalf("migrated doc = %+v", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy db not archived: %v", err)
	}
	if _, err := os.Stat(legacyPath + ".migrated"); err != nil {
		t.Fatalf("archived legacy db missing: %v", err)
	}
}
