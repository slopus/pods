// Package store implements the podbay document store backed by SQLite.
package store

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

	LegacyPod = "default"
)

// fileFormat is the legacy on-disk shape of db.json.
type fileFormat struct {
	Pods        map[string]podData            `json:"pods"`
	Collections map[string]map[string]api.Doc `json:"collections,omitempty"` // legacy single-tenant format
}

type podData struct {
	Collections map[string]map[string]api.Doc `json:"collections"`
}

// Store is a SQLite-backed document store.
type Store struct {
	mu   sync.RWMutex
	path string
	db   *sql.DB
	now  func() time.Time // overridable in tests
}

// Open creates or opens a SQLite store at path. If the database is empty and a
// sibling db.json exists, Open imports that legacy JSON store once.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{path: path, db: db, now: time.Now}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateLegacyJSON(filepath.Join(filepath.Dir(path), "db.json")); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	stmts := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS docs (
			pod TEXT NOT NULL,
			collection TEXT NOT NULL,
			id TEXT NOT NULL,
			doc TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (pod, collection, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_docs_collection ON docs (pod, collection)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: init sqlite: %w", err)
		}
	}
	return nil
}

func (s *Store) migrateLegacyJSON(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read legacy %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM docs`).Scan(&count); err != nil {
		return fmt.Errorf("store: count sqlite docs: %w", err)
	}
	if count > 0 {
		return nil
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("store: parse legacy %s: %w", path, err)
	}
	pods := f.Pods
	if len(pods) == 0 && f.Collections != nil {
		pods = map[string]podData{LegacyPod: {Collections: f.Collections}}
	}
	if len(pods) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for pod, data := range pods {
		for coll, docs := range data.Collections {
			for id, doc := range docs {
				if id == "" {
					if docID, _ := doc[FieldID].(string); docID != "" {
						id = docID
					}
				}
				if id == "" {
					continue
				}
				d := normalizeDoc(doc)
				d[FieldID] = id
				createdAt := stringField(d, FieldCreatedAt)
				updatedAt := stringField(d, FieldUpdatedAt)
				if createdAt == "" {
					createdAt = updatedAt
				}
				if updatedAt == "" {
					updatedAt = createdAt
				}
				if createdAt == "" {
					createdAt = time.Now().UTC().Format(time.RFC3339)
				}
				if updatedAt == "" {
					updatedAt = createdAt
				}
				d[FieldCreatedAt] = createdAt
				d[FieldUpdatedAt] = updatedAt
				if err := putTx(tx, pod, coll, id, d, createdAt, updatedAt); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// Collections returns all non-empty collections in pod sorted by name.
func (s *Store) Collections(pod string) []api.Collection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT collection, count(*) FROM docs WHERE pod = ? GROUP BY collection ORDER BY collection`, pod)
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
func (s *Store) Create(pod, coll string, doc api.Doc) (api.Doc, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := normalizeDoc(doc)
	now := s.timestamp()
	d[FieldID] = id
	d[FieldCreatedAt] = now
	d[FieldUpdatedAt] = now
	if err := s.put(pod, coll, id, d, now, now); err != nil {
		return nil, err
	}
	return copyDoc(d), nil
}

// Get returns the document with the given id, or false if absent.
func (s *Store) Get(pod, coll, id string) (api.Doc, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok, err := s.get(pod, coll, id)
	if err != nil || !ok {
		return nil, false
	}
	return doc, true
}

// Set replaces (upserts) the document with the given id. created_at is
// preserved when the document already existed; updated_at is always reset.
func (s *Store) Set(pod, coll, id string, doc api.Doc) (api.Doc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := normalizeDoc(doc)
	now := s.timestamp()
	d[FieldID] = id
	createdAt := now
	if prev, ok, err := s.get(pod, coll, id); err != nil {
		return nil, err
	} else if ok {
		createdAt = stringField(prev, FieldCreatedAt)
		if createdAt == "" {
			createdAt = now
		}
	}
	d[FieldCreatedAt] = createdAt
	d[FieldUpdatedAt] = now
	if err := s.put(pod, coll, id, d, createdAt, now); err != nil {
		return nil, err
	}
	return copyDoc(d), nil
}

// Patch shallow-merges patch into the existing document. Reserved fields in
// the patch are ignored. Returns false if the document does not exist.
func (s *Store) Patch(pod, coll, id string, patch api.Doc) (api.Doc, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok, err := s.get(pod, coll, id)
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
	if err := s.put(pod, coll, id, normalizeDoc(d), createdAt, updatedAt); err != nil {
		return nil, true, err
	}
	return copyDoc(d), true, nil
}

// Delete removes the document with the given id. Returns false if absent.
func (s *Store) Delete(pod, coll, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM docs WHERE pod = ? AND collection = ? AND id = ?`, pod, coll, id)
	if err != nil {
		return false, fmt.Errorf("store: delete doc: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Drop removes a whole collection. Returns false if absent.
func (s *Store) Drop(pod, coll string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM docs WHERE pod = ? AND collection = ?`, pod, coll)
	if err != nil {
		return false, fmt.Errorf("store: drop collection: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) collectionDocs(pod, coll string) ([]api.Doc, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT doc FROM docs WHERE pod = ? AND collection = ?`, pod, coll)
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

func (s *Store) get(pod, coll, id string) (api.Doc, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT doc FROM docs WHERE pod = ? AND collection = ? AND id = ?`, pod, coll, id).Scan(&raw)
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

func (s *Store) put(pod, coll, id string, doc api.Doc, createdAt, updatedAt string) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: marshal doc: %w", err)
	}
	_, err = s.db.Exec(`INSERT INTO docs (pod, collection, id, doc, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(pod, collection, id) DO UPDATE SET
			doc = excluded.doc,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`, pod, coll, id, string(data), createdAt, updatedAt)
	if err != nil {
		return fmt.Errorf("store: put doc: %w", err)
	}
	return nil
}

func putTx(tx *sql.Tx, pod, coll, id string, doc api.Doc, createdAt, updatedAt string) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: marshal migrated doc: %w", err)
	}
	_, err = tx.Exec(`INSERT INTO docs (pod, collection, id, doc, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, pod, coll, id, string(data), createdAt, updatedAt)
	if err != nil {
		return fmt.Errorf("store: migrate doc: %w", err)
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
