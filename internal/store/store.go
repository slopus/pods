// Package store implements the podbay document store: one SQLite database
// per site, plus in-memory query evaluation.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/slopus/pods/internal/api"
)

// Reserved document fields maintained by the store.
const (
	FieldID        = "id"
	FieldCreatedAt = "created_at"
	FieldUpdatedAt = "updated_at"
)

var siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Store manages one SQLite database per site, stored as <dir>/<site>.sqlite.
type Store struct {
	dir string
	mu  sync.Mutex // guards dbs
	dbs map[string]*siteDB
	now func() time.Time // overridable in tests
}

// siteDB serializes multi-statement operations (get-then-put) on one site's
// database; the database/sql pool alone does not make those atomic.
type siteDB struct {
	mu sync.RWMutex
	db *sql.DB
}

// Open prepares the per-site database directory. If a legacy single-database
// store exists at <dir>/../db.sqlite (the pre-per-site layout), its documents
// are split into per-site databases once and the legacy file is renamed to
// db.sqlite.migrated.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	s := &Store{dir: dir, dbs: make(map[string]*siteDB), now: time.Now}
	if err := s.migrateLegacyGlobal(filepath.Join(filepath.Dir(dir), "db.sqlite")); err != nil {
		return nil, err
	}
	return s, nil
}

// Close closes all open site databases.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for site, sdb := range s.dbs {
		if err := sdb.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.dbs, site)
	}
	return firstErr
}

func (s *Store) sitePath(site string) string {
	return filepath.Join(s.dir, site+".sqlite")
}

// openSite returns the cached handle for site, opening (and optionally
// creating) its database. With create=false a site without a database file
// yields (nil, nil), so reads on unknown sites stay empty and never create
// files.
func (s *Store) openSite(site string, create bool) (*siteDB, error) {
	if !siteNameRe.MatchString(site) {
		return nil, fmt.Errorf("store: invalid site name %q", site)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sdb, ok := s.dbs[site]; ok {
		return sdb, nil
	}
	path := s.sitePath(site)
	if !create {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		} else if err != nil {
			return nil, fmt.Errorf("store: stat %s: %w", path, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := initSiteDB(db); err != nil {
		db.Close()
		return nil, err
	}
	sdb := &siteDB{db: db}
	s.dbs[site] = sdb
	return sdb, nil
}

func initSiteDB(db *sql.DB) error {
	stmts := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS docs (
			collection TEXT NOT NULL,
			id TEXT NOT NULL,
			doc TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (collection, id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("store: init sqlite: %w", err)
		}
	}
	return nil
}

// DeleteSite closes and removes a site's database file (with its WAL
// sidecars). Removing a site that has no database is not an error.
func (s *Store) DeleteSite(site string) error {
	if !siteNameRe.MatchString(site) {
		return fmt.Errorf("store: invalid site name %q", site)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sdb, ok := s.dbs[site]; ok {
		sdb.mu.Lock() // wait out in-flight operations
		_ = sdb.db.Close()
		sdb.mu.Unlock()
		delete(s.dbs, site)
	}
	path := s.sitePath(site)
	// Remove the WAL/SHM sidecars before the main database file: if removal is
	// interrupted, an orphan -wal without its -shm/main file is harmless,
	// whereas an orphan -wal left beside a future same-named db is not.
	for _, p := range []string{path + "-wal", path + "-shm", path} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: remove %s: %w", p, err)
		}
	}
	return nil
}

// migrateLegacyGlobal splits the old single-database layout (one db.sqlite
// with a pod column) into per-site databases.
func (s *Store) migrateLegacyGlobal(path string) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("store: stat legacy %s: %w", path, err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("store: open legacy %s: %w", path, err)
	}
	defer legacy.Close()
	// A missing docs table means there is nothing to migrate; any OTHER error
	// (locked/corrupt/unreadable file) must fail startup rather than be
	// mistaken for "already migrated" — silently skipping would let a later
	// successful re-run clobber data written in the meantime.
	var hasDocs int
	if err := legacy.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='docs'`).Scan(&hasDocs); err != nil {
		return fmt.Errorf("store: inspect legacy %s: %w", path, err)
	}
	if hasDocs == 0 {
		legacy.Close()
		return os.Rename(path, path+".migrated")
	}
	rows, err := legacy.Query(`SELECT pod, collection, id, doc, created_at, updated_at FROM docs`)
	if err != nil {
		return fmt.Errorf("store: read legacy %s: %w", path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var pod, coll, id, doc, createdAt, updatedAt string
		if err := rows.Scan(&pod, &coll, &id, &doc, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("store: scan legacy doc: %w", err)
		}
		if !siteNameRe.MatchString(pod) {
			continue
		}
		sdb, err := s.openSite(pod, true)
		if err != nil {
			return err
		}
		// INSERT OR IGNORE so a re-run after a partial migration never
		// overwrites a document already updated through the API.
		if _, err := sdb.db.Exec(`INSERT OR IGNORE INTO docs (collection, id, doc, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, coll, id, doc, createdAt, updatedAt); err != nil {
			return fmt.Errorf("store: migrate doc: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read legacy docs: %w", err)
	}
	rows.Close()
	legacy.Close()
	if err := os.Rename(path, path+".migrated"); err != nil {
		return fmt.Errorf("store: archive legacy db: %w", err)
	}
	return nil
}

// Collections returns all non-empty collections in a site's store sorted by name.
func (s *Store) Collections(site string) []api.Collection {
	sdb, err := s.openSite(site, false)
	if err != nil || sdb == nil {
		return nil
	}
	sdb.mu.RLock()
	defer sdb.mu.RUnlock()
	rows, err := sdb.db.Query(`SELECT collection, count(*) FROM docs GROUP BY collection ORDER BY collection`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []api.Collection
	for rows.Next() {
		var coll api.Collection
		if err := rows.Scan(&coll.Name, &coll.Count); err == nil {
			out = append(out, coll)
		}
	}
	return out
}

// Create inserts doc into coll with a freshly generated id and server-set
// created_at/updated_at, returning the stored document.
func (s *Store) Create(site, coll string, doc api.Doc) (api.Doc, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	sdb, err := s.openSite(site, true)
	if err != nil {
		return nil, err
	}
	sdb.mu.Lock()
	defer sdb.mu.Unlock()
	d := normalizeDoc(doc)
	now := s.timestamp()
	d[FieldID] = id
	d[FieldCreatedAt] = now
	d[FieldUpdatedAt] = now
	if err := put(sdb.db, coll, id, d, now, now); err != nil {
		return nil, err
	}
	return copyDoc(d), nil
}

// Get returns the document with the given id, or false if absent.
func (s *Store) Get(site, coll, id string) (api.Doc, bool) {
	sdb, err := s.openSite(site, false)
	if err != nil || sdb == nil {
		return nil, false
	}
	sdb.mu.RLock()
	defer sdb.mu.RUnlock()
	doc, ok, err := get(sdb.db, coll, id)
	if err != nil || !ok {
		return nil, false
	}
	return doc, true
}

// Set replaces (upserts) the document with the given id. created_at is
// preserved when the document already existed; updated_at is always reset.
func (s *Store) Set(site, coll, id string, doc api.Doc) (api.Doc, error) {
	sdb, err := s.openSite(site, true)
	if err != nil {
		return nil, err
	}
	sdb.mu.Lock()
	defer sdb.mu.Unlock()
	d := normalizeDoc(doc)
	now := s.timestamp()
	d[FieldID] = id
	createdAt := now
	if prev, ok, err := get(sdb.db, coll, id); err != nil {
		return nil, err
	} else if ok {
		createdAt = stringField(prev, FieldCreatedAt)
		if createdAt == "" {
			createdAt = now
		}
	}
	d[FieldCreatedAt] = createdAt
	d[FieldUpdatedAt] = now
	if err := put(sdb.db, coll, id, d, createdAt, now); err != nil {
		return nil, err
	}
	return copyDoc(d), nil
}

// Patch shallow-merges patch into the existing document. Reserved fields in
// the patch are ignored. Returns false if the document does not exist.
func (s *Store) Patch(site, coll, id string, patch api.Doc) (api.Doc, bool, error) {
	sdb, err := s.openSite(site, false)
	if err != nil {
		return nil, false, err
	}
	if sdb == nil {
		return nil, false, nil
	}
	sdb.mu.Lock()
	defer sdb.mu.Unlock()
	d, ok, err := get(sdb.db, coll, id)
	if err != nil || !ok {
		return nil, ok, err
	}
	for k, v := range patch {
		if k == FieldID || k == FieldCreatedAt || k == FieldUpdatedAt {
			continue
		}
		d[k] = copyValue(v)
	}
	updatedAt := s.timestamp()
	d[FieldUpdatedAt] = updatedAt
	createdAt := stringField(d, FieldCreatedAt)
	if createdAt == "" {
		createdAt = updatedAt
		d[FieldCreatedAt] = createdAt
	}
	if err := put(sdb.db, coll, id, normalizeDoc(d), createdAt, updatedAt); err != nil {
		return nil, true, err
	}
	return copyDoc(d), true, nil
}

// Delete removes the document with the given id. Returns false if absent.
func (s *Store) Delete(site, coll, id string) (bool, error) {
	sdb, err := s.openSite(site, false)
	if err != nil || sdb == nil {
		return false, err
	}
	sdb.mu.Lock()
	defer sdb.mu.Unlock()
	res, err := sdb.db.Exec(`DELETE FROM docs WHERE collection = ? AND id = ?`, coll, id)
	if err != nil {
		return false, fmt.Errorf("store: delete doc: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Drop removes a whole collection. Returns false if absent.
func (s *Store) Drop(site, coll string) (bool, error) {
	sdb, err := s.openSite(site, false)
	if err != nil || sdb == nil {
		return false, err
	}
	sdb.mu.Lock()
	defer sdb.mu.Unlock()
	res, err := sdb.db.Exec(`DELETE FROM docs WHERE collection = ?`, coll)
	if err != nil {
		return false, fmt.Errorf("store: drop collection: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) collectionDocs(site, coll string) ([]api.Doc, error) {
	sdb, err := s.openSite(site, false)
	if err != nil || sdb == nil {
		return nil, err
	}
	sdb.mu.RLock()
	defer sdb.mu.RUnlock()
	rows, err := sdb.db.Query(`SELECT doc FROM docs WHERE collection = ?`, coll)
	if err != nil {
		return nil, fmt.Errorf("store: query docs: %w", err)
	}
	defer rows.Close()
	var docs []api.Doc
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("store: scan doc: %w", err)
		}
		doc, err := decodeDoc([]byte(raw))
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func get(db *sql.DB, coll, id string) (api.Doc, bool, error) {
	var raw string
	err := db.QueryRow(`SELECT doc FROM docs WHERE collection = ? AND id = ?`, coll, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get doc: %w", err)
	}
	doc, err := decodeDoc([]byte(raw))
	if err != nil {
		return nil, false, err
	}
	return doc, true, nil
}

func put(db *sql.DB, coll, id string, doc api.Doc, createdAt, updatedAt string) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: marshal doc: %w", err)
	}
	_, err = db.Exec(`INSERT INTO docs (collection, id, doc, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(collection, id) DO UPDATE SET
			doc = excluded.doc,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`, coll, id, string(data), createdAt, updatedAt)
	if err != nil {
		return fmt.Errorf("store: put doc: %w", err)
	}
	return nil
}

func (s *Store) timestamp() string {
	return s.now().UTC().Format(time.RFC3339)
}

// newID returns 16 hex characters from crypto/rand.
func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func normalizeDoc(d api.Doc) api.Doc {
	data, err := json.Marshal(d)
	if err != nil {
		return copyDoc(d)
	}
	out, err := decodeDoc(data)
	if err != nil {
		return copyDoc(d)
	}
	return out
}

func decodeDoc(data []byte) (api.Doc, error) {
	var doc api.Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("store: decode doc: %w", err)
	}
	if doc == nil {
		doc = api.Doc{}
	}
	return doc, nil
}

func stringField(d api.Doc, field string) string {
	v, _ := d[field].(string)
	return v
}

// copyDoc deep-copies a document so callers can never alias store state.
func copyDoc(d api.Doc) api.Doc {
	out := make(api.Doc, len(d))
	for k, v := range d {
		out[k] = copyValue(v)
	}
	return out
}

func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = copyValue(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = copyValue(vv)
		}
		return s
	default:
		return v
	}
}
