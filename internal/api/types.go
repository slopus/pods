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

// UserProfile is the public shape of an authenticated user.
type UserProfile struct {
	ID        string `json:"id"`
	Login     string `json:"login,omitempty"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Admin     bool   `json:"admin,omitempty"`
}

// Me is the response of GET /api/me.
type Me struct {
	Authenticated bool         `json:"authenticated"`
	User          *UserProfile `json:"user,omitempty"`
	Site          string       `json:"site,omitempty"`
	LoginURL      string       `json:"login_url,omitempty"`
}

// AuthProvider describes an OAuth/OIDC login option.
type AuthProvider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LoginURL string `json:"login_url"`
}

// AuthProviders is the response of GET /api/auth/providers.
type AuthProviders struct {
	Providers []AuthProvider `json:"providers"`
}

// GitHubDeviceStart is returned when the CLI starts GitHub device auth.
type GitHubDeviceStart struct {
	DeviceCode     string `json:"device_code"`
	UserCode       string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn      int    `json:"expires_in"`
	Interval       int    `json:"interval"`
}

// GitHubDevicePoll is sent by the CLI while waiting for GitHub device auth.
type GitHubDevicePoll struct {
	DeviceCode string `json:"device_code"`
}

// GitHubDeviceToken is returned after GitHub device auth completes. Token is
// the single refreshable JWT API token.
type GitHubDeviceToken struct {
	Pending   bool         `json:"pending,omitempty"`
	Interval  int          `json:"interval,omitempty"` // suggested seconds before the next poll
	Token     string       `json:"token,omitempty"`
	ExpiresAt time.Time    `json:"expires_at,omitzero"`
	User      *UserProfile `json:"user,omitempty"`
}

// TokenResponse is the response of POST /api/auth/refresh: a fresh API token
// replacing the one the request was authenticated with.
type TokenResponse struct {
	Token     string       `json:"token"`
	ExpiresAt time.Time    `json:"expires_at"`
	User      *UserProfile `json:"user,omitempty"`
}

// Site describes one deployed static site.
type Site struct {
	Name       string    `json:"name"`
	OwnerID    string    `json:"owner_id,omitempty"`
	OwnerLogin string    `json:"owner_login,omitempty"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	UpdatedAt  time.Time `json:"updated_at"`
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
	Type       string    `json:"type"`
	Collection string    `json:"collection,omitempty"`
	DocumentID string    `json:"document_id,omitempty"`
	Doc        Doc       `json:"doc,omitempty"`
	Site       *Site     `json:"site,omitempty"`
	At         time.Time `json:"at"`
}
