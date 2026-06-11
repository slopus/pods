// Package client provides a thin, typed HTTP client for the podbay API.
// It sets the bearer authorization header on every request and converts
// api.Error response bodies into readable Go errors.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/slopus/pods/internal/api"
)

// APIError is a non-2xx response from the server. Message carries the
// server-provided error text (the "error" field of the api.Error body).
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

// Client talks to a single podbay server.
type Client struct {
	endpoint string
	secret   string
	httpc    *http.Client
}

// New returns a Client for the given endpoint, e.g. "http://localhost:7777".
// A trailing slash on the endpoint is ignored.
func New(endpoint, secret string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		secret:   secret,
		httpc:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// Endpoint returns the normalized endpoint the client talks to.
func (c *Client) Endpoint() string { return c.endpoint }

// SiteURL returns the fallback path URL of a deployed site.
func (c *Client) SiteURL(name string) string {
	return c.endpoint + "/sites/" + url.PathEscape(name) + "/"
}

// SubdomainSiteURL returns the subdomain URL for name under the client's endpoint host.
func (c *Client) SubdomainSiteURL(name string) string {
	base, err := url.Parse(c.endpoint)
	if err != nil {
		return c.SiteURL(name)
	}
	base.Host = name + "." + base.Host
	base.Path = "/"
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

// Health calls GET /healthz.
func (c *Client) Health(ctx context.Context) (api.Health, error) {
	var out api.Health
	err := c.do(ctx, http.MethodGet, "/healthz", nil, nil, "", &out)
	return out, err
}

// Me calls GET /api/me and returns the current authenticated user profile.
func (c *Client) Me(ctx context.Context) (api.Me, error) {
	var out api.Me
	err := c.do(ctx, http.MethodGet, "/api/me", nil, nil, "", &out)
	return out, err
}

// GitHubDeviceStart starts GitHub OAuth device authentication.
func (c *Client) GitHubDeviceStart(ctx context.Context) (api.GitHubDeviceStart, error) {
	var out api.GitHubDeviceStart
	err := c.do(ctx, http.MethodPost, "/api/auth/github/device/start", nil, nil, "", &out)
	return out, err
}

// GitHubDevicePoll polls GitHub OAuth device authentication.
func (c *Client) GitHubDevicePoll(ctx context.Context, deviceCode string) (api.GitHubDeviceToken, error) {
	var out api.GitHubDeviceToken
	err := c.doJSON(ctx, http.MethodPost, "/api/auth/github/device/poll", api.GitHubDevicePoll{DeviceCode: deviceCode}, &out)
	return out, err
}

// Refresh calls POST /api/auth/refresh, exchanging the client's current API
// token for a fresh one. The new token replaces the old one on the client.
func (c *Client) Refresh(ctx context.Context) (api.TokenResponse, error) {
	var out api.TokenResponse
	if err := c.do(ctx, http.MethodPost, "/api/auth/refresh", nil, nil, "", &out); err != nil {
		return out, err
	}
	if out.Token != "" {
		c.secret = out.Token
	}
	return out, nil
}

// Token returns the API token the client currently sends.
func (c *Client) Token() string { return c.secret }

// Sites calls GET /api/sites and returns the deployed sites.
func (c *Client) Sites(ctx context.Context) ([]api.Site, error) {
	var out api.SiteList
	if err := c.do(ctx, http.MethodGet, "/api/sites", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return out.Sites, nil
}

// Deploy uploads a tar.gz archive as the new content of name.
func (c *Client) Deploy(ctx context.Context, name string, archive io.Reader) (api.DeployResult, error) {
	var out api.DeployResult
	path := "/api/sites/" + url.PathEscape(name)
	err := c.do(ctx, http.MethodPut, path, nil, archive, "application/gzip", &out)
	return out, err
}

// DeleteSite calls DELETE for name.
func (c *Client) DeleteSite(ctx context.Context, name string) error {
	path := "/api/sites/" + url.PathEscape(name)
	return c.do(ctx, http.MethodDelete, path, nil, nil, "", nil)
}

// Collections calls GET /api/db and returns the endpoint site's document collections.
func (c *Client) Collections(ctx context.Context) ([]api.Collection, error) {
	var out api.CollectionList
	if err := c.do(ctx, http.MethodGet, "/api/db", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return out.Collections, nil
}

// QueryOptions are the parameters of a collection query.
type QueryOptions struct {
	Where  []string // "field=value" filters, ANDed together
	Sort   string   // "field" or "-field"
	Limit  int      // 0 = no limit
	Offset int
}

// Query calls GET /api/db/{coll} with the given options.
func (c *Client) Query(ctx context.Context, coll string, opts QueryOptions) (api.QueryResult, error) {
	q := url.Values{}
	for _, w := range opts.Where {
		q.Add("where", w)
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	var out api.QueryResult
	err := c.do(ctx, http.MethodGet, "/api/db/"+url.PathEscape(coll), q, nil, "", &out)
	return out, err
}

// CreateDoc calls POST /api/db/{coll} and returns the created document.
func (c *Client) CreateDoc(ctx context.Context, coll string, doc api.Doc) (api.Doc, error) {
	var out api.Doc
	err := c.doJSON(ctx, http.MethodPost, "/api/db/"+url.PathEscape(coll), doc, &out)
	return out, err
}

// GetDoc calls GET /api/db/{coll}/{id}.
func (c *Client) GetDoc(ctx context.Context, coll, id string) (api.Doc, error) {
	var out api.Doc
	err := c.do(ctx, http.MethodGet, docPath(coll, id), nil, nil, "", &out)
	return out, err
}

// SetDoc calls PUT /api/db/{coll}/{id} (replace/upsert) and returns the document.
func (c *Client) SetDoc(ctx context.Context, coll, id string, doc api.Doc) (api.Doc, error) {
	var out api.Doc
	err := c.doJSON(ctx, http.MethodPut, docPath(coll, id), doc, &out)
	return out, err
}

// PatchDoc calls PATCH /api/db/{coll}/{id} (shallow merge) and returns the document.
func (c *Client) PatchDoc(ctx context.Context, coll, id string, doc api.Doc) (api.Doc, error) {
	var out api.Doc
	err := c.doJSON(ctx, http.MethodPatch, docPath(coll, id), doc, &out)
	return out, err
}

// DeleteDoc calls DELETE /api/db/{coll}/{id}.
func (c *Client) DeleteDoc(ctx context.Context, coll, id string) error {
	return c.do(ctx, http.MethodDelete, docPath(coll, id), nil, nil, "", nil)
}

// DropCollection calls DELETE /api/db/{coll}, removing the whole collection.
func (c *Client) DropCollection(ctx context.Context, coll string) error {
	return c.do(ctx, http.MethodDelete, "/api/db/"+url.PathEscape(coll), nil, nil, "", nil)
}

func docPath(coll, id string) string {
	return "/api/db/" + url.PathEscape(coll) + "/" + url.PathEscape(id)
}

// doJSON marshals in as the JSON request body and performs the request.
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encoding request body: %w", err)
	}
	return c.do(ctx, method, path, nil, bytes.NewReader(body), "application/json", out)
}

// do performs one HTTP request. On non-2xx responses it decodes the api.Error
// body (when present) and returns an *APIError. When out is non-nil the
// response body is decoded into it.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string, out any) error {
	u := c.endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out != nil {
		dec := json.NewDecoder(resp.Body)
		dec.UseNumber()
		if err := dec.Decode(out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func decodeError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var apiErr api.Error
	if err := json.Unmarshal(data, &apiErr); err == nil && apiErr.Error != "" {
		return &APIError{StatusCode: resp.StatusCode, Message: apiErr.Error}
	}
	return &APIError{StatusCode: resp.StatusCode, Message: "unexpected status " + resp.Status}
}
