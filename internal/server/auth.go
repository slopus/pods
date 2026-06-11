package server

import (
	"context"
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

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/securecookie"
	"github.com/slopus/pods/internal/api"
	"golang.org/x/oauth2"
)

const (
	roleNone      = 0
	roleReader    = 1
	rolePublisher = 2
	roleAdmin     = 3

	appAuthPublic   = "public"
	appAuthOptional = "optional"
	appAuthRequired = "required"

	sessionCookieName = "pods_session"
	oauthStateName    = "pods_oauth_state"
)

var userIDRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,128}$`)

type authFile struct {
	Users []authUserConfig        `json:"users"`
	Teams map[string]authTeamFile `json:"teams,omitempty"`
	Apps  []authAppFile           `json:"apps,omitempty"`
	OAuth authOAuthFile           `json:"oauth,omitempty"`
}

type authUserConfig struct {
	ID     string            `json:"id"`
	Name   string            `json:"name,omitempty"`
	Email  string            `json:"email,omitempty"`
	Admin  bool              `json:"admin,omitempty"`
	Tokens []string          `json:"tokens,omitempty"`
	OAuth  []string          `json:"oauth,omitempty"`
	Teams  map[string]string `json:"teams,omitempty"`
}

type authTeamFile struct {
	Name          string `json:"name,omitempty"`
	PublicPublish bool   `json:"public_publish,omitempty"`
}

type authAppFile struct {
	Team string `json:"team"`
	Site string `json:"site"`
	Auth string `json:"auth,omitempty"`
}

type authOAuthFile struct {
	SessionSecret string              `json:"session_secret,omitempty"`
	CookieDomain  string              `json:"cookie_domain,omitempty"`
	SessionHours  int                 `json:"session_hours,omitempty"`
	Providers     []oauthProviderFile `json:"providers,omitempty"`
}

type oauthProviderFile struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"client_id"`
	ClientSecret   string   `json:"client_secret,omitempty"`
	RedirectURL    string   `json:"redirect_url,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	AllowedEmails  []string `json:"allowed_emails,omitempty"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
}

type authenticator struct {
	path          string
	file          authFile
	users         map[string]*authUser
	apps          []authAppPolicy
	oauth         map[string]*oauthProvider
	oauthOrder    []string
	secure        *securecookie.SecureCookie
	cookieDomain  string
	sessionLength time.Duration
}

type authUser struct {
	ID     string
	Name   string
	Email  string
	Admin  bool
	Tokens []string
	OAuth  []string
	Teams  map[string]string
}

type authAppPolicy struct {
	Team string
	Site string
	Auth string
}

type oauthProvider struct {
	id             string
	name           string
	redirectURL    string
	config         oauth2.Config
	verifier       *oidc.IDTokenVerifier
	allowedEmails  map[string]struct{}
	allowedDomains map[string]struct{}
}

type oauthClaims struct {
	Subject       string `json:"sub"`
	Nonce         string `json:"nonce"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

type oauthState struct {
	Provider string `json:"provider"`
	ReturnTo string `json:"return_to"`
	Nonce    string `json:"nonce"`
	Created  int64  `json:"created"`
}

type oauthSession struct {
	Provider string `json:"provider"`
	Subject  string `json:"subject"`
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	Expires  int64  `json:"expires"`
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
			Teams:  map[string]string{"*": "admin"},
		}},
		Teams: map[string]authTeamFile{
			publicTeam: {Name: "Public", PublicPublish: true},
		},
		OAuth: authOAuthFile{
			SessionSecret: adminToken,
			SessionHours:  168,
		},
	}
}

func buildAuthenticator(file authFile, path string) (*authenticator, error) {
	if file.Teams == nil {
		file.Teams = map[string]authTeamFile{}
	}
	if _, ok := file.Teams[publicTeam]; !ok {
		file.Teams[publicTeam] = authTeamFile{Name: "Public", PublicPublish: true}
	}
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
	a := &authenticator{
		path:          path,
		file:          file,
		users:         make(map[string]*authUser),
		oauth:         make(map[string]*oauthProvider),
		secure:        securecookie.New(key[:], nil),
		cookieDomain:  strings.TrimSpace(file.OAuth.CookieDomain),
		sessionLength: time.Duration(sessionHours) * time.Hour,
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
	for _, app := range file.Apps {
		policy, err := buildAppPolicy(app)
		if err != nil {
			return nil, err
		}
		a.apps = append(a.apps, policy)
	}
	for _, cfg := range file.OAuth.Providers {
		provider, err := buildOAuthProvider(cfg)
		if err != nil {
			return nil, err
		}
		if _, exists := a.oauth[provider.id]; exists {
			return nil, fmt.Errorf("auth: duplicate oauth provider %q", provider.id)
		}
		a.oauth[provider.id] = provider
		a.oauthOrder = append(a.oauthOrder, provider.id)
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
	teams := map[string]string{}
	for team, role := range cfg.Teams {
		if team != "*" && !siteNameRe.MatchString(team) {
			return nil, fmt.Errorf("auth: invalid team %q for user %q", team, cfg.ID)
		}
		canon, ok := canonicalRole(role)
		if !ok {
			return nil, fmt.Errorf("auth: invalid role %q for user %q team %q", role, cfg.ID, team)
		}
		teams[team] = canon
	}
	if cfg.Admin && teams["*"] == "" {
		teams["*"] = "admin"
	}
	return &authUser{
		ID:     cfg.ID,
		Name:   cfg.Name,
		Email:  strings.ToLower(strings.TrimSpace(cfg.Email)),
		Admin:  cfg.Admin,
		Tokens: compactStrings(cfg.Tokens),
		OAuth:  compactStrings(cfg.OAuth),
		Teams:  teams,
	}, nil
}

func buildAppPolicy(cfg authAppFile) (authAppPolicy, error) {
	if !siteNameRe.MatchString(cfg.Team) {
		return authAppPolicy{}, fmt.Errorf("auth: invalid app team %q", cfg.Team)
	}
	if cfg.Site != "*" && !siteNameRe.MatchString(cfg.Site) {
		return authAppPolicy{}, fmt.Errorf("auth: invalid app site %q", cfg.Site)
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Auth))
	if mode == "" {
		mode = appAuthRequired
	}
	switch mode {
	case appAuthPublic, appAuthOptional, appAuthRequired:
	default:
		return authAppPolicy{}, fmt.Errorf("auth: invalid app auth mode %q", cfg.Auth)
	}
	return authAppPolicy{Team: cfg.Team, Site: cfg.Site, Auth: mode}, nil
}

func buildOAuthProvider(cfg oauthProviderFile) (*oauthProvider, error) {
	cfg.ID = strings.ToLower(strings.TrimSpace(cfg.ID))
	if !siteNameRe.MatchString(cfg.ID) {
		return nil, fmt.Errorf("auth: invalid oauth provider id %q", cfg.ID)
	}
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, fmt.Errorf("auth: oauth provider %q missing issuer", cfg.ID)
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, fmt.Errorf("auth: oauth provider %q missing client_id", cfg.ID)
	}
	oidcProvider, err := oidc.NewProvider(context.Background(), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: discover oauth provider %q: %w", cfg.ID, err)
	}
	scopes := compactStrings(cfg.Scopes)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	} else if !containsString(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = cfg.ID
	}
	p := &oauthProvider{
		id:          cfg.ID,
		name:        name,
		redirectURL: strings.TrimSpace(cfg.RedirectURL),
		config: oauth2.Config{
			ClientID:     strings.TrimSpace(cfg.ClientID),
			ClientSecret: cfg.ClientSecret,
			Endpoint:     oidcProvider.Endpoint(),
			Scopes:       scopes,
		},
		verifier:       oidcProvider.Verifier(&oidc.Config{ClientID: strings.TrimSpace(cfg.ClientID)}),
		allowedEmails:  stringSetLower(cfg.AllowedEmails),
		allowedDomains: stringSetLower(cfg.AllowedDomains),
	}
	return p, nil
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

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func stringSetLower(values []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" {
			out[v] = struct{}{}
		}
	}
	return out
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
	if session.Expires <= time.Now().Unix() {
		return nil, false
	}
	provider, ok := a.oauth[session.Provider]
	if !ok {
		return nil, false
	}
	claims := oauthClaims{
		Subject: session.Subject,
		Email:   session.Email,
		Name:    session.Name,
	}
	if !provider.allows(claims) {
		return nil, false
	}
	return a.userByOAuth(session.Provider, session.Subject, claims), true
}

func (a *authenticator) userByOAuth(providerID, subject string, claims oauthClaims) *authUser {
	ref := providerID + ":" + subject
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	for _, user := range a.users {
		if matchesOAuthUser(user, ref, email) {
			return user.withOAuthClaims(claims)
		}
	}
	return &authUser{
		ID:    oauthFallbackID(providerID, subject, email),
		Name:  claims.Name,
		Email: email,
		Teams: map[string]string{},
	}
}

func matchesOAuthUser(user *authUser, ref, email string) bool {
	for _, candidate := range user.OAuth {
		if strings.EqualFold(candidate, ref) {
			return true
		}
	}
	if email != "" && strings.EqualFold(user.Email, email) {
		return true
	}
	if email != "" && strings.EqualFold(user.ID, email) {
		return true
	}
	return false
}

func (u *authUser) withOAuthClaims(claims oauthClaims) *authUser {
	copy := *u
	copy.Teams = map[string]string{}
	for team, role := range u.Teams {
		copy.Teams[team] = role
	}
	if copy.Email == "" {
		copy.Email = strings.ToLower(strings.TrimSpace(claims.Email))
	}
	if copy.Name == "" {
		copy.Name = claims.Name
	}
	return &copy
}

func oauthFallbackID(providerID, subject, email string) string {
	if email != "" {
		return email
	}
	sum := sha256.Sum256([]byte(providerID + ":" + subject))
	return providerID + "-" + base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

func (a *authenticator) appAuth(team, site string) string {
	mode := appAuthPublic
	for _, app := range a.apps {
		if app.Team == team && app.Site == site {
			return app.Auth
		}
		if app.Team == team && app.Site == "*" {
			mode = app.Auth
		}
	}
	return mode
}

func (a *authenticator) allowsAnonymousPublish(team string) bool {
	if team != publicTeam {
		return false
	}
	cfg, ok := a.file.Teams[team]
	if !ok {
		return true
	}
	return cfg.PublicPublish
}

func (a *authenticator) defaultOAuthProvider() string {
	if len(a.oauthOrder) == 0 {
		return ""
	}
	return a.oauthOrder[0]
}

func (u *authUser) roleRank(team string) int {
	if u == nil {
		return roleNone
	}
	if u.Admin {
		return roleAdmin
	}
	if role := u.Teams[team]; role != "" {
		return rankRole(role)
	}
	if role := u.Teams["*"]; role != "" {
		return rankRole(role)
	}
	return roleNone
}

func (u *authUser) hasRole(team string, required int) bool {
	return u.roleRank(team) >= required
}

func (u *authUser) profile() api.UserProfile {
	teams := map[string]string{}
	for team, role := range u.Teams {
		teams[team] = role
	}
	return api.UserProfile{
		ID:    u.ID,
		Name:  u.Name,
		Email: u.Email,
		Admin: u.Admin,
		Teams: teams,
	}
}

func canonicalRole(role string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "read", "reader", "viewer", "member":
		return "reader", true
	case "write", "writer", "publish", "publisher", "editor":
		return "publisher", true
	case "admin", "owner":
		return "admin", true
	default:
		return "", false
	}
}

func rankRole(role string) int {
	switch role {
	case "reader":
		return roleReader
	case "publisher":
		return rolePublisher
	case "admin":
		return roleAdmin
	default:
		return roleNone
	}
}

func (s *Server) currentUser(r *http.Request) (*authUser, bool) {
	return s.auth.authenticate(r)
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (*authUser, bool) {
	user, ok := s.currentUser(r)
	if !ok {
		s.writeUnauthorized(w, "unauthorized")
		return nil, false
	}
	return user, true
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (*authUser, bool) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return nil, false
	}
	if !user.Admin {
		writeError(w, http.StatusForbidden, "admin access required")
		return nil, false
	}
	return user, true
}

func (s *Server) requireTeamRole(w http.ResponseWriter, r *http.Request, team string, role int) (*authUser, bool) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return nil, false
	}
	if !user.hasRole(team, role) {
		writeError(w, http.StatusForbidden, "forbidden for team %q", team)
		return nil, false
	}
	return user, true
}

func (s *Server) requireTeamPublish(w http.ResponseWriter, r *http.Request, team string) bool {
	if s.auth.allowsAnonymousPublish(team) {
		return true
	}
	_, ok := s.requireTeamRole(w, r, team, rolePublisher)
	return ok
}

func (s *Server) requireAppAccess(w http.ResponseWriter, r *http.Request, team, site string) bool {
	if s.auth.appAuth(team, site) != appAuthRequired {
		return true
	}
	if _, ok := s.currentUser(r); ok {
		return true
	}
	if providerID := s.auth.defaultOAuthProvider(); providerID != "" {
		loginURL := "/api/auth/login/" + url.PathEscape(providerID) + "?return_to=" + url.QueryEscape(currentRequestURL(r))
		http.Redirect(w, r, loginURL, http.StatusFound)
		return false
	}
	s.writeUnauthorized(w, "unauthorized")
	return false
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusUnauthorized, "%s", msg)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	team, site, _ := s.siteFromHost(r.Host)
	mode := appAuthPublic
	if team != "" && site != "" {
		mode = s.auth.appAuth(team, site)
	}
	user, ok := s.currentUser(r)
	if !ok && (mode == appAuthRequired || r.URL.Query().Get("required") == "1") {
		s.writeUnauthorized(w, "unauthorized")
		return
	}
	res := api.Me{
		Authenticated: ok,
		Team:          team,
		Site:          site,
		AppAuth:       mode,
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
	providers := make([]api.AuthProvider, 0, len(s.auth.oauthOrder))
	for _, id := range s.auth.oauthOrder {
		provider := s.auth.oauth[id]
		providers = append(providers, api.AuthProvider{
			ID:       provider.id,
			Name:     provider.name,
			LoginURL: "/api/auth/login/" + url.PathEscape(provider.id) + "?return_to=" + url.QueryEscape(returnTo),
		})
	}
	writeJSON(w, http.StatusOK, api.AuthProviders{Providers: providers})
}

func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("provider")
	provider, ok := s.auth.oauth[providerID]
	if !ok {
		writeError(w, http.StatusNotFound, "oauth provider %q not found", providerID)
		return
	}
	nonce, err := randomString(18)
	if err != nil {
		respondErr(w, err)
		return
	}
	state := oauthState{
		Provider: providerID,
		ReturnTo: s.safeReturnTo(r),
		Nonce:    nonce,
		Created:  time.Now().Unix(),
	}
	encoded, err := s.auth.secure.Encode(oauthStateName, state)
	if err != nil {
		respondErr(w, err)
		return
	}
	cfg := provider.oauthConfig(s.oauthCallbackURL(r, provider))
	http.Redirect(w, r, cfg.AuthCodeURL(encoded, oidc.Nonce(nonce)), http.StatusFound)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("provider")
	provider, ok := s.auth.oauth[providerID]
	if !ok {
		writeError(w, http.StatusNotFound, "oauth provider %q not found", providerID)
		return
	}
	var state oauthState
	if err := s.auth.secure.Decode(oauthStateName, r.URL.Query().Get("state"), &state); err != nil {
		writeError(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	if state.Provider != providerID || time.Since(time.Unix(state.Created, 0)) > 10*time.Minute {
		writeError(w, http.StatusBadRequest, "expired oauth state")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "missing oauth code")
		return
	}
	cfg := provider.oauthConfig(s.oauthCallbackURL(r, provider))
	token, err := cfg.Exchange(r.Context(), code)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "oauth exchange failed")
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		writeError(w, http.StatusUnauthorized, "oauth provider did not return an id_token")
		return
	}
	idToken, err := provider.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "oauth id_token verification failed")
		return
	}
	var claims oauthClaims
	if err := idToken.Claims(&claims); err != nil {
		writeError(w, http.StatusUnauthorized, "oauth id_token claims failed")
		return
	}
	if claims.Nonce != state.Nonce {
		writeError(w, http.StatusUnauthorized, "oauth nonce verification failed")
		return
	}
	claims.Subject = idToken.Subject
	if !provider.allows(claims) {
		writeError(w, http.StatusForbidden, "oauth user is not allowed")
		return
	}
	if !s.setSessionCookie(w, r, oauthSession{
		Provider: providerID,
		Subject:  claims.Subject,
		Email:    strings.ToLower(strings.TrimSpace(claims.Email)),
		Name:     claims.Name,
		Expires:  time.Now().Add(s.auth.sessionLength).Unix(),
	}) {
		return
	}
	http.Redirect(w, r, state.ReturnTo, http.StatusFound)
}

func (s *Server) handleOAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *oauthProvider) oauthConfig(redirectURL string) oauth2.Config {
	cfg := p.config
	if p.redirectURL != "" {
		cfg.RedirectURL = p.redirectURL
	} else {
		cfg.RedirectURL = redirectURL
	}
	return cfg
}

func (p *oauthProvider) allows(claims oauthClaims) bool {
	if len(p.allowedEmails) == 0 && len(p.allowedDomains) == 0 {
		return true
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if _, ok := p.allowedEmails[email]; ok {
		return true
	}
	if at := strings.LastIndex(email, "@"); at >= 0 {
		_, ok := p.allowedDomains[email[at+1:]]
		return ok
	}
	return false
}

func (s *Server) loginURL(r *http.Request) string {
	providerID := s.auth.defaultOAuthProvider()
	if providerID == "" {
		return ""
	}
	return "/api/auth/login/" + url.PathEscape(providerID) + "?return_to=" + url.QueryEscape(s.safeReturnTo(r))
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
	if sameHost(u.Host, r.Host) || s.auth.hostMatchesCookieDomain(u.Host) {
		return u.String()
	}
	return currentRequestOrigin(r) + "/"
}

func (a *authenticator) hostMatchesCookieDomain(host string) bool {
	domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(a.cookieDomain)), ".")
	if domain == "" {
		return false
	}
	h := hostOnly(host)
	return h == domain || strings.HasSuffix(h, "."+domain)
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

func (s *Server) oauthCallbackURL(r *http.Request, provider *oauthProvider) string {
	if provider.redirectURL != "" {
		return provider.redirectURL
	}
	return currentRequestOrigin(r) + "/api/auth/callback/" + url.PathEscape(provider.id)
}

func currentRequestURL(r *http.Request) string {
	return currentRequestOrigin(r) + r.URL.RequestURI()
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
