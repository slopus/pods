package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/securecookie"
	"github.com/slopus/pods/internal/api"
)

const (
	sessionCookieName = "pods_session"
	oauthStateName    = "pods_oauth_state"
)

var userIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,128}$`)

type authFile struct {
	Users  []authUserConfig `json:"users"`
	OAuth  authOAuthFile    `json:"oauth,omitempty"`
	GitHub authGitHubFile   `json:"github,omitempty"`
}

type authUserConfig struct {
	ID        string   `json:"id"`
	Login     string   `json:"login,omitempty"`
	Name      string   `json:"name,omitempty"`
	Email     string   `json:"email,omitempty"`
	AvatarURL string   `json:"avatar_url,omitempty"`
	Admin     bool     `json:"admin,omitempty"`
	Tokens    []string `json:"tokens,omitempty"`
}

type authOAuthFile struct {
	SessionSecret string `json:"session_secret,omitempty"`
	CookieDomain  string `json:"cookie_domain,omitempty"`
	SessionHours  int    `json:"session_hours,omitempty"`
}

type authenticator struct {
	path          string
	users         map[string]*authUser
	secure        *securecookie.SecureCookie
	tokenKey      []byte // HMAC key for API token JWTs
	cookieDomain  string
	sessionLength time.Duration
	github        authGitHubFile
}

type authUser struct {
	ID        string
	Login     string
	Name      string
	Email     string
	AvatarURL string
	Admin     bool
	Tokens    []string
}

type oauthState struct {
	Provider string `json:"provider"`
	ReturnTo string `json:"return_to"`
	Nonce    string `json:"nonce,omitempty"`
	Created  int64  `json:"created"`
}

type oauthSession struct {
	Provider  string `json:"provider"`
	Subject   string `json:"subject"`
	Login     string `json:"login,omitempty"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Expires   int64  `json:"expires"`
}

func newAuthenticator(path, adminToken string) (*authenticator, error) {
	if path == "" {
		return buildAuthenticator(defaultAuthFile(adminToken), path)
	}
	if err := ensureAuthFile(path, adminToken); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read %s: %w", path, err)
	}
	var file authFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("auth: parse %s: %w", path, err)
	}
	return buildAuthenticator(file, path)
}

func ensureAuthFile(path, adminToken string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("auth: stat %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("auth: create config dir: %w", err)
	}
	data, err := json.MarshalIndent(defaultAuthFile(adminToken), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("auth: write %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

func defaultAuthFile(adminToken string) authFile {
	return authFile{
		Users: []authUserConfig{{
			ID:     "admin",
			Name:   "Admin",
			Admin:  true,
			Tokens: []string{adminToken},
		}},
		OAuth: authOAuthFile{
			SessionSecret: adminToken,
			SessionHours:  168,
		},
	}
}

func buildAuthenticator(file authFile, path string) (*authenticator, error) {
	sessionSecret := file.OAuth.SessionSecret
	if sessionSecret == "" {
		sessionSecret = firstUserToken(file.Users)
	}
	if sessionSecret == "" {
		return nil, errors.New("auth: oauth.session_secret or at least one user token is required")
	}
	sessionHours := file.OAuth.SessionHours
	if sessionHours <= 0 {
		sessionHours = 168
	}
	key := sha256.Sum256([]byte(sessionSecret))
	tokenKey := sha256.Sum256([]byte("podbay-api-token:" + sessionSecret))
	a := &authenticator{
		path:          path,
		users:         make(map[string]*authUser),
		secure:        securecookie.New(key[:], nil),
		tokenKey:      tokenKey[:],
		cookieDomain:  strings.TrimSpace(file.OAuth.CookieDomain),
		sessionLength: time.Duration(sessionHours) * time.Hour,
		github:        normalizeGitHubConfig(file.GitHub),
	}
	for _, cfg := range file.Users {
		user, err := buildAuthUser(cfg)
		if err != nil {
			return nil, err
		}
		if _, exists := a.users[user.ID]; exists {
			return nil, fmt.Errorf("auth: duplicate user %q", user.ID)
		}
		a.users[user.ID] = user
	}
	return a, nil
}

func firstUserToken(users []authUserConfig) string {
	for _, user := range users {
		for _, token := range user.Tokens {
			token = strings.TrimSpace(token)
			if token != "" {
				return token
			}
		}
	}
	return ""
}

func buildAuthUser(cfg authUserConfig) (*authUser, error) {
	cfg.ID = strings.TrimSpace(cfg.ID)
	if !userIDRe.MatchString(cfg.ID) {
		return nil, fmt.Errorf("auth: invalid user id %q", cfg.ID)
	}
	return &authUser{
		ID:        cfg.ID,
		Login:     strings.ToLower(strings.TrimSpace(cfg.Login)),
		Name:      cfg.Name,
		Email:     strings.ToLower(strings.TrimSpace(cfg.Email)),
		AvatarURL: strings.TrimSpace(cfg.AvatarURL),
		Admin:     cfg.Admin,
		Tokens:    compactStrings(cfg.Tokens),
	}, nil
}

func compactStrings(values []string) []string {
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// applyGitHubConfig overlays GitHub settings from the server flags/env onto
// whatever auth.json provided. Only fields the caller actually set override
// existing values; in particular scopes are taken verbatim (not normalized to
// the defaults first) so a custom github.scopes in auth.json is preserved.
func (a *authenticator) applyGitHubConfig(cfg authGitHubFile) {
	if v := strings.TrimSpace(cfg.ClientID); v != "" {
		a.github.ClientID = v
	}
	if v := strings.TrimSpace(cfg.ClientSecret); v != "" {
		a.github.ClientSecret = v
	}
	if v := strings.TrimSpace(cfg.RedirectURL); v != "" {
		a.github.RedirectURL = v
	}
	if len(cfg.Scopes) > 0 {
		a.github.Scopes = cfg.Scopes
	}
	if len(cfg.AllowedUsers) > 0 {
		a.github.AllowedUsers = cfg.AllowedUsers
	}
	a.github = normalizeGitHubConfig(a.github)
}

func (a *authenticator) authenticate(r *http.Request) (*authUser, bool) {
	if a == nil {
		return nil, false
	}
	if token := bearerToken(r); token != "" {
		if user, ok := a.userByToken(token); ok {
			return user, true
		}
	}
	if user, ok := a.userBySession(r); ok {
		return user, true
	}
	return nil, false
}

// allowsUser reports whether a user is still permitted to authenticate.
// Static auth-file users (no provider prefix) are always allowed; GitHub
// users are re-checked against the github.allowed_users list on every
// request, so removing a login revokes its tokens immediately.
func (a *authenticator) allowsUser(user *authUser) bool {
	if user == nil {
		return false
	}
	provider, _, ok := strings.Cut(user.ID, ":")
	if !ok || provider != githubProviderID {
		return true
	}
	return a.github.allows(user.Login)
}

func bearerToken(r *http.Request) string {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func (a *authenticator) userByToken(token string) (*authUser, bool) {
	for _, user := range a.users {
		for _, candidate := range user.Tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(candidate)) == 1 {
				return user, true
			}
		}
	}
	return nil, false
}

func (a *authenticator) userBySession(r *http.Request) (*authUser, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	var session oauthSession
	if err := a.secure.Decode(sessionCookieName, cookie.Value, &session); err != nil {
		return nil, false
	}
	if session.Expires <= time.Now().Unix() || session.Provider != githubProviderID {
		return nil, false
	}
	return &authUser{
		ID:        githubUserIDFromSubject(session.Subject),
		Login:     strings.ToLower(strings.TrimSpace(session.Login)),
		Name:      session.Name,
		Email:     strings.ToLower(strings.TrimSpace(session.Email)),
		AvatarURL: session.AvatarURL,
	}, true
}

func (u *authUser) profile() api.UserProfile {
	return api.UserProfile{
		ID:        u.ID,
		Login:     u.Login,
		Name:      u.Name,
		Email:     u.Email,
		AvatarURL: u.AvatarURL,
		Admin:     u.Admin,
	}
}

// currentUserBearer authenticates a request from its bearer token only: a
// static auth-file token, or a podbay JWT whose subject still exists and is
// still allowed. It deliberately ignores the session cookie, so that
// state-changing endpoints can never be driven by a cookie that is shared
// with untrusted user pods on sibling subdomains.
func (s *Server) currentUserBearer(r *http.Request) (*authUser, bool) {
	token := bearerToken(r)
	if token == "" {
		return nil, false
	}
	if user, ok := s.auth.userByToken(token); ok {
		return user, true
	}
	if s.identity == nil {
		return nil, false
	}
	claims, err := verifyJWT(s.auth.tokenKey, token, time.Now())
	if err != nil {
		return nil, false
	}
	user, ok := s.identity.userByID(claims.Subject)
	if !ok || !s.auth.allowsUser(user) {
		return nil, false
	}
	return user, true
}

// currentUser authenticates a request by bearer token or, failing that, by
// the browser session cookie. Use it only for read-only endpoints; mutating
// endpoints must use currentUserBearer (see requireWriteUser).
func (s *Server) currentUser(r *http.Request) (*authUser, bool) {
	if user, ok := s.currentUserBearer(r); ok {
		return user, true
	}
	if user, ok := s.auth.userBySession(r); ok && s.auth.allowsUser(user) {
		return user, true
	}
	return nil, false
}

// POST /api/auth/refresh — exchange a valid (not yet expired) API token for a
// fresh one. There is intentionally no separate refresh token.
func (s *Server) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		s.writeUnauthorized(w, "a bearer API token is required")
		return
	}
	claims, err := verifyJWT(s.auth.tokenKey, token, time.Now())
	if err != nil {
		s.writeUnauthorized(w, "invalid or expired token, log in again")
		return
	}
	user, ok := s.identity.userByID(claims.Subject)
	if !ok || !s.auth.allowsUser(user) {
		s.writeUnauthorized(w, "unknown or no longer allowed user, log in again")
		return
	}
	fresh, expiresAt, err := s.auth.issueToken(user, time.Now())
	if err != nil {
		respondErr(w, err)
		return
	}
	profile := user.profile()
	writeJSON(w, http.StatusOK, api.TokenResponse{Token: fresh, ExpiresAt: expiresAt, User: &profile})
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (*authUser, bool) {
	user, ok := s.currentUser(r)
	if !ok {
		s.writeUnauthorized(w, "unauthorized")
		return nil, false
	}
	return user, true
}

// requireWriteUser authorizes a mutating request. It accepts only bearer
// tokens (CLI JWTs and static admin tokens), never the session cookie, which
// closes same-site CSRF from attacker-controlled pods.
func (s *Server) requireWriteUser(w http.ResponseWriter, r *http.Request) (*authUser, bool) {
	user, ok := s.currentUserBearer(r)
	if !ok {
		s.writeUnauthorized(w, "a bearer API token is required (run \"pods login\")")
		return nil, false
	}
	return user, true
}

func (s *Server) requireSiteAccess(w http.ResponseWriter, r *http.Request, site string) (*authUser, bool) {
	user, ok := s.requireWriteUser(w, r)
	if !ok {
		return nil, false
	}
	if user.Admin {
		return user, true
	}
	owner, found, err := s.identity.siteOwner(site)
	if err != nil {
		respondErr(w, err)
		return nil, false
	}
	if !found || owner.ID == user.ID {
		return user, true
	}
	writeError(w, http.StatusForbidden, "site %q is owned by %s", site, owner.Login)
	return nil, false
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusUnauthorized, "%s", msg)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	site, _ := s.siteFromHost(r.Host)
	user, ok := s.currentUser(r)
	if !ok && r.URL.Query().Get("required") == "1" {
		writeJSON(w, http.StatusUnauthorized, api.Me{Authenticated: false, Site: site, LoginURL: s.loginURL(r)})
		return
	}
	res := api.Me{
		Authenticated: ok,
		Site:          site,
		LoginURL:      s.loginURL(r),
	}
	if ok {
		profile := user.profile()
		res.User = &profile
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	returnTo := s.safeReturnTo(r)
	providers := []api.AuthProvider{}
	if s.auth.github.enabled() {
		providers = append(providers, api.AuthProvider{
			ID:       githubProviderID,
			Name:     "GitHub",
			LoginURL: "/api/auth/login/" + githubProviderID + "?return_to=" + url.QueryEscape(returnTo),
		})
	}
	writeJSON(w, http.StatusOK, api.AuthProviders{Providers: providers})
}

func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("provider") != githubProviderID {
		writeError(w, http.StatusNotFound, "oauth provider %q not found", r.PathValue("provider"))
		return
	}
	s.handleGitHubLogin(w, r)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("provider") != githubProviderID {
		writeError(w, http.StatusNotFound, "oauth provider %q not found", r.PathValue("provider"))
		return
	}
	s.handleGitHubCallback(w, r)
}

func (s *Server) handleOAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) loginURL(r *http.Request) string {
	if !s.auth.github.enabled() {
		return ""
	}
	return "/api/auth/login/" + githubProviderID + "?return_to=" + url.QueryEscape(s.safeReturnTo(r))
}

func (s *Server) safeReturnTo(r *http.Request) string {
	raw := strings.TrimSpace(r.URL.Query().Get("return_to"))
	if raw == "" {
		return currentRequestOrigin(r) + "/"
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return currentRequestOrigin(r) + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return currentRequestOrigin(r) + "/"
	}
	// Only redirect back to the host the login was initiated from or to the
	// public base host. Sibling user pods under the cookie domain are
	// attacker-controlled, so they are not valid post-login targets.
	if sameHost(u.Host, r.Host) || (s.publicBaseHost() != "" && hostOnly(u.Host) == s.publicBaseHost()) {
		return u.String()
	}
	return currentRequestOrigin(r) + "/"
}

func sameHost(a, b string) bool {
	return hostOnly(a) == hostOnly(b)
}

func hostOnly(host string) string {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		return h
	}
	return strings.TrimSuffix(host, ".")
}

func currentRequestOrigin(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + r.Host
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, session oauthSession) bool {
	encoded, err := s.auth.secure.Encode(sessionCookieName, session)
	if err != nil {
		respondErr(w, err)
		return false
	}
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    encoded,
		Path:     "/",
		Expires:  time.Unix(session.Expires, 0),
		MaxAge:   int(time.Until(time.Unix(session.Expires, 0)).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	}
	if s.auth.cookieDomain != "" {
		cookie.Domain = s.auth.cookieDomain
	}
	http.SetCookie(w, cookie)
	return true
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	}
	if s.auth.cookieDomain != "" {
		cookie.Domain = s.auth.cookieDomain
	}
	http.SetCookie(w, cookie)
}

func requestIsSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
