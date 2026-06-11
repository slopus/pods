// Package api defines the wire types shared by the podbay server and the pods CLI.
package api

import "time"

// Health is the response of GET /healthz.
type Health struct {
	OK bool `json:"ok"`
}

// Error is the JSON body of every non-2xx API response.
type Error struct {
	Error string `json:"error"`
}

// Site describes one deployed static site.
type Site struct {
	Name      string    `json:"name"`
	Team      string    `json:"team"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SiteList is the response of GET /api/sites.
type SiteList struct {
	Sites []Site `json:"sites"`
}

// DeployResult is the response of PUT /api/sites/{name}.
type DeployResult struct {
	Site Site   `json:"site"`
	URL  string `json:"url"`
}

// Collection describes one document collection in the store.
type Collection struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// CollectionList is the response of GET /api/db on a site-scoped host.
type CollectionList struct {
	Collections []Collection `json:"collections"`
}

// Doc is a stored JSON document. The server maintains the reserved fields
// "id", "created_at" and "updated_at".
type Doc = map[string]any

// QueryResult is the response of GET /api/db/{collection} on a site-scoped host.
// Total counts matching documents before limit/offset are applied.
type QueryResult struct {
	Docs  []Doc `json:"docs"`
	Total int   `json:"total"`
}

// UpdateEvent is emitted on the authenticated SSE update stream whenever a
// pod's site or JSON store changes.
type UpdateEvent struct {
	ID         int64     `json:"id"`
	Pod        string    `json:"pod"`
	Team       string    `json:"team,omitempty"`
	Type       string    `json:"type"`
	Collection string    `json:"collection,omitempty"`
	DocumentID string    `json:"document_id,omitempty"`
	Doc        Doc       `json:"doc,omitempty"`
	Site       *Site     `json:"site,omitempty"`
	At         time.Time `json:"at"`
}
