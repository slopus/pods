package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/slopus/pods/internal/api"
	"github.com/slopus/pods/internal/store"
)

// GET /api/db
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	site, ok := s.requestSite(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.CollectionList{Collections: s.store.Collections(site)})
}

// GET /api/db/{coll}
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	site, ok := s.requestSite(w, r)
	if !ok {
		return
	}
	coll := r.PathValue("coll")
	if !validName(w, "collection", coll) {
		return
	}
	q, err := parseStoreQuery(r)
	if err != nil {
		respondErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.store.Query(site, coll, q))
}

// POST /api/db/{coll}
func (s *Server) handleDocCreate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.requestSite(w, r)
	if !ok {
		return
	}
	coll := r.PathValue("coll")
	if !validName(w, "collection", coll) {
		return
	}
	doc, err := readDoc(w, r)
	if err != nil {
		respondErr(w, err)
		return
	}
	created, err := s.store.Create(site, coll, doc)
	if err != nil {
		respondErr(w, err)
		return
	}
	id, _ := created["id"].(string)
	s.publish(api.UpdateEvent{
		Pod:        site,
		Type:       "doc.created",
		Collection: coll,
		DocumentID: id,
		Doc:        created,
	})
	writeJSON(w, http.StatusCreated, created)
}

// GET /api/db/{coll}/{id}
func (s *Server) handleDocGet(w http.ResponseWriter, r *http.Request) {
	site, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	doc, found := s.store.Get(site, coll, id)
	if !found {
		writeError(w, http.StatusNotFound, "document %q not found", id)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// PUT /api/db/{coll}/{id}
func (s *Server) handleDocSet(w http.ResponseWriter, r *http.Request) {
	site, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	doc, err := readDoc(w, r)
	if err != nil {
		respondErr(w, err)
		return
	}
	set, err := s.store.Set(site, coll, id, doc)
	if err != nil {
		respondErr(w, err)
		return
	}
	s.publish(api.UpdateEvent{
		Pod:        site,
		Type:       "doc.set",
		Collection: coll,
		DocumentID: id,
		Doc:        set,
	})
	writeJSON(w, http.StatusOK, set)
}

// PATCH /api/db/{coll}/{id}
func (s *Server) handleDocPatch(w http.ResponseWriter, r *http.Request) {
	site, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	doc, err := readDoc(w, r)
	if err != nil {
		respondErr(w, err)
		return
	}
	patched, found, err := s.store.Patch(site, coll, id, doc)
	if err != nil {
		respondErr(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "document %q not found", id)
		return
	}
	s.publish(api.UpdateEvent{
		Pod:        site,
		Type:       "doc.patched",
		Collection: coll,
		DocumentID: id,
		Doc:        patched,
	})
	writeJSON(w, http.StatusOK, patched)
}

// DELETE /api/db/{coll}/{id}
func (s *Server) handleDocDelete(w http.ResponseWriter, r *http.Request) {
	site, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	deleted, err := s.store.Delete(site, coll, id)
	if err != nil {
		respondErr(w, err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "document %q not found", id)
		return
	}
	s.publish(api.UpdateEvent{
		Pod:        site,
		Type:       "doc.deleted",
		Collection: coll,
		DocumentID: id,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DELETE /api/db/{coll}
func (s *Server) handleCollectionDrop(w http.ResponseWriter, r *http.Request) {
	site, ok := s.requestSite(w, r)
	if !ok {
		return
	}
	coll := r.PathValue("coll")
	if !validName(w, "collection", coll) {
		return
	}
	dropped, err := s.store.Drop(site, coll)
	if err != nil {
		respondErr(w, err)
		return
	}
	if !dropped {
		writeError(w, http.StatusNotFound, "collection %q not found", coll)
		return
	}
	s.publish(api.UpdateEvent{
		Pod:        site,
		Type:       "collection.dropped",
		Collection: coll,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) validDocPath(w http.ResponseWriter, r *http.Request) (site, coll, id string, ok bool) {
	site, ok = s.requestSite(w, r)
	if !ok {
		return "", "", "", false
	}
	coll = r.PathValue("coll")
	id = r.PathValue("id")
	if !validName(w, "collection", coll) || !validName(w, "document id", id) {
		return "", "", "", false
	}
	return site, coll, id, true
}

// requestSite resolves the site database API tenant from the request host
// and requires that site to actually be deployed, so stray subdomains can
// never create database files.
func (s *Server) requestSite(w http.ResponseWriter, r *http.Request) (site string, ok bool) {
	// The dev server is single-site: the store API is always the dev site,
	// regardless of host (so it works on localhost without a subdomain).
	if s.cfg.dev() {
		return s.cfg.DevSite, true
	}
	site, ok = s.siteFromHost(r.Host)
	if !ok {
		writeError(w, http.StatusBadRequest, "site API requires a <site> subdomain host")
		return "", false
	}
	if info, err := os.Stat(s.siteDir(site)); err != nil || !info.IsDir() {
		writeError(w, http.StatusNotFound, "site %q not found", site)
		return "", false
	}
	return site, true
}

func validName(w http.ResponseWriter, kind, value string) bool {
	if nameRe.MatchString(value) {
		return true
	}
	writeError(w, http.StatusBadRequest, "invalid %s %q", kind, value)
	return false
}

func parseStoreQuery(r *http.Request) (store.Query, error) {
	values := r.URL.Query()
	q := store.Query{Sort: values.Get("sort")}
	for _, raw := range values["where"] {
		field, value, ok := strings.Cut(raw, "=")
		if !ok || field == "" {
			return q, badRequestf("invalid where filter %q, expected field=value", raw)
		}
		q.Where = append(q.Where, store.Where{Field: field, Value: value})
	}
	var err error
	if q.Limit, err = parseNonNegativeInt(values.Get("limit"), "limit"); err != nil {
		return q, err
	}
	if q.Offset, err = parseNonNegativeInt(values.Get("offset"), "offset"); err != nil {
		return q, err
	}
	return q, nil
}

func parseNonNegativeInt(raw, name string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, badRequestf("%s must be a non-negative integer", name)
	}
	return n, nil
}

func readDoc(w http.ResponseWriter, r *http.Request) (api.Doc, error) {
	body := http.MaxBytesReader(w, r.Body, maxDocBytes)
	dec := json.NewDecoder(body)
	var doc api.Doc
	if err := dec.Decode(&doc); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, badRequestf("request body too large (max %d bytes)", maxDocBytes)
		}
		return nil, badRequestf("invalid JSON object: %v", err)
	}
	if doc == nil {
		return nil, badRequestf("document must be a JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, badRequestf("request body must contain a single JSON object")
		}
		return nil, badRequestf("invalid JSON object: %v", err)
	}
	return doc, nil
}
