// Package store implements the podbay JSON document store: an in-memory
// map of pod tenants to collections guarded by a RWMutex, persisted
// atomically to db.json on every mutation.
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/slopus/pods/internal/api"
)

// Reserved document fields maintained by the store.
const (
	FieldID        = "id"
	FieldCreatedAt = "created_at"
	FieldUpdatedAt = "updated_at"

	LegacyPod = "default"
)

// fileFormat is the on-disk shape of db.json.
type fileFormat struct {
	Pods        map[string]podData            `json:"pods"`
	Collections map[string]map[string]api.Doc `json:"collections,omitempty"` // legacy single-tenant format
}

type podData struct {
	Collections map[string]map[string]api.Doc `json:"collections"`
}

// Store is an in-memory document store persisted to a JSON file.
// The in-memory state is authoritative; the file is a durable mirror.
type Store struct {
	mu   sync.RWMutex
	path string
	pods map[string]map[string]map[string]api.Doc
	now  func() time.Time // overridable in tests
}

// Open loads the store from path. A missing file yields an empty store.
func Open(path string) (*Store, error) {
	s := &Store{
		path: path,
		pods: make(map[string]map[string]map[string]api.Doc),
		now:  time.Now,
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return s, nil
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("store: parse %s: %w", path, err)
	}
	if len(f.Pods) > 0 {
		for pod, data := range f.Pods {
			if data.Collections != nil {
				s.pods[pod] = data.Collections
			}
		}
		return s, nil
	}
	if f.Collections != nil {
		s.pods[LegacyPod] = f.Collections
	}
	return s, nil
}

// Collections returns all non-empty collections in pod sorted by name.
func (s *Store) Collections(pod string) []api.Collection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	colls := s.pods[pod]
	out := make([]api.Collection, 0, len(colls))
	for name, docs := range colls {
		out = append(out, api.Collection{Name: name, Count: len(docs)})
	}
	slices.SortFunc(out, func(a, b api.Collection) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})
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
	d := copyDoc(doc)
	now := s.timestamp()
	d[FieldID] = id
	d[FieldCreatedAt] = now
	d[FieldUpdatedAt] = now
	s.putLocked(pod, coll, id, d)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return copyDoc(d), nil
}

// Get returns the document with the given id, or false if absent.
func (s *Store) Get(pod, coll, id string) (api.Doc, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.pods[pod][coll][id]
	if !ok {
		return nil, false
	}
	return copyDoc(d), true
}

// Set replaces (upserts) the document with the given id. created_at is
// preserved when the document already existed; updated_at is always reset.
func (s *Store) Set(pod, coll, id string, doc api.Doc) (api.Doc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := copyDoc(doc)
	now := s.timestamp()
	d[FieldID] = id
	if prev, ok := s.pods[pod][coll][id]; ok {
		d[FieldCreatedAt] = prev[FieldCreatedAt]
	} else {
		d[FieldCreatedAt] = now
	}
	d[FieldUpdatedAt] = now
	s.putLocked(pod, coll, id, d)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return copyDoc(d), nil
}

// Patch shallow-merges patch into the existing document. Reserved fields in
// the patch are ignored. Returns false if the document does not exist.
func (s *Store) Patch(pod, coll, id string, patch api.Doc) (api.Doc, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.pods[pod][coll][id]
	if !ok {
		return nil, false, nil
	}
	for k, v := range patch {
		if k == FieldID || k == FieldCreatedAt || k == FieldUpdatedAt {
			continue
		}
		d[k] = copyValue(v)
	}
	d[FieldUpdatedAt] = s.timestamp()
	if err := s.persistLocked(); err != nil {
		return nil, true, err
	}
	return copyDoc(d), true, nil
}

// Delete removes the document with the given id. Returns false if absent.
func (s *Store) Delete(pod, coll, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	colls, ok := s.pods[pod]
	if !ok {
		return false, nil
	}
	docs, ok := colls[coll]
	if !ok {
		return false, nil
	}
	if _, ok := docs[id]; !ok {
		return false, nil
	}
	delete(docs, id)
	if len(docs) == 0 {
		delete(colls, coll)
	}
	if len(colls) == 0 {
		delete(s.pods, pod)
	}
	return true, s.persistLocked()
}

// Drop removes a whole collection. Returns false if absent.
func (s *Store) Drop(pod, coll string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	colls, ok := s.pods[pod]
	if !ok {
		return false, nil
	}
	if _, ok := colls[coll]; !ok {
		return false, nil
	}
	delete(colls, coll)
	if len(colls) == 0 {
		delete(s.pods, pod)
	}
	return true, s.persistLocked()
}

// putLocked stores d, creating the collection map if needed. Caller holds mu.
func (s *Store) putLocked(pod, coll, id string, d api.Doc) {
	colls := s.pods[pod]
	if colls == nil {
		colls = make(map[string]map[string]api.Doc)
		s.pods[pod] = colls
	}
	docs := colls[coll]
	if docs == nil {
		docs = make(map[string]api.Doc)
		colls[coll] = docs
	}
	docs[id] = d
}

// persistLocked writes the store to disk: marshal, temp file, atomic rename.
// Caller holds mu.
func (s *Store) persistLocked() error {
	pods := make(map[string]podData, len(s.pods))
	for pod, colls := range s.pods {
		pods[pod] = podData{Collections: colls}
	}
	data, err := json.MarshalIndent(fileFormat{Pods: pods}, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".db-*.json.tmp")
	if err != nil {
		return fmt.Errorf("store: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("store: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: rename: %w", err)
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
