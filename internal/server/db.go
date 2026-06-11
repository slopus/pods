package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/slopus/pods/internal/api"
	"github.com/slopus/pods/internal/store"
)

// GET /api/db
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	team, site, key, ok := s.requestSiteTenant(w, r)
	if !ok {
		return
	}
	_ = team
	_ = site
	writeJSON(w, http.StatusOK, api.CollectionList{Collections: s.store.Collections(key)})
}

// GET /api/db/{coll}
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	_, _, key, ok := s.requestSiteTenant(w, r)
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
	writeJSON(w, http.StatusOK, s.store.Query(key, coll, q))
}

// POST /api/db/{coll}
func (s *Server) handleDocCreate(w http.ResponseWriter, r *http.Request) {
	team, site, key, ok := s.requestSiteTenant(w, r)
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
	created, err := s.store.Create(key, coll, doc)
	if err != nil {
		respondErr(w, err)
		return
	}
	id, _ := created["id"].(string)
	s.publish(api.UpdateEvent{
		Pod:        site,
		Team:       team,
		Type:       "doc.created",
		Collection: coll,
		DocumentID: id,
		Doc:        created,
	})
	writeJSON(w, http.StatusCreated, created)
}

// GET /api/db/{coll}/{id}
func (s *Server) handleDocGet(w http.ResponseWriter, r *http.Request) {
	_, _, key, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	doc, found := s.store.Get(key, coll, id)
	if !found {
		writeError(w, http.StatusNotFound, "document %q not found", id)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// PUT /api/db/{coll}/{id}
func (s *Server) handleDocSet(w http.ResponseWriter, r *http.Request) {
	team, site, key, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	doc, err := readDoc(w, r)
	if err != nil {
		respondErr(w, err)
		return
	}
	set, err := s.store.Set(key, coll, id, doc)
	if err != nil {
		respondErr(w, err)
		return
	}
	s.publish(api.UpdateEvent{
		Pod:        site,
		Team:       team,
		Type:       "doc.set",
		Collection: coll,
		DocumentID: id,
		Doc:        set,
	})
	writeJSON(w, http.StatusOK, set)
}

// PATCH /api/db/{coll}/{id}
func (s *Server) handleDocPatch(w http.ResponseWriter, r *http.Request) {
	team, site, key, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	doc, err := readDoc(w, r)
	if err != nil {
		respondErr(w, err)
		return
	}
	patched, found, err := s.store.Patch(key, coll, id, doc)
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
		Team:       team,
		Type:       "doc.patched",
		Collection: coll,
		DocumentID: id,
		Doc:        patched,
	})
	writeJSON(w, http.StatusOK, patched)
}

// DELETE /api/db/{coll}/{id}
func (s *Server) handleDocDelete(w http.ResponseWriter, r *http.Request) {
	team, site, key, coll, id, ok := s.validDocPath(w, r)
	if !ok {
		return
	}
	deleted, err := s.store.Delete(key, coll, id)
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
		Team:       team,
		Type:       "doc.deleted",
		Collection: coll,
		DocumentID: id,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DELETE /api/db/{coll}
func (s *Server) handleCollectionDrop(w http.ResponseWriter, r *http.Request) {
	team, site, key, ok := s.requestSiteTenant(w, r)
	if !ok {
		return
	}
	coll := r.PathValue("coll")
	if !validName(w, "collection", coll) {
		return
	}
	dropped, err := s.store.Drop(key, coll)
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
		Team:       team,
		Type:       "collection.dropped",
		Collection: coll,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) validDocPath(w http.ResponseWriter, r *http.Request) (team, site, key, coll, id string, ok bool) {
	team, site, key, ok = s.requestSiteTenant(w, r)
	if !ok {
		return "", "", "", "", "", false
	}
	coll = r.PathValue("coll")
	id = r.PathValue("id")
	if !validName(w, "collection", coll) || !validName(w, "document id", id) {
		return "", "", "", "", "", false
	}
	return team, site, key, coll, id, true
}

func (s *Server) requestSiteTenant(w http.ResponseWriter, r *http.Request) (team, site, key string, ok bool) {
	team, site, ok = s.siteFromHost(r.Host)
	if !ok {
		writeError(w, http.StatusBadRequest, "site API requires a <site>.<team> host")
		return "", "", "", false
	}
	return team, site, tenantKey(team, site), true
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
