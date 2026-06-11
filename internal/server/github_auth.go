package server

import (
	"crypto/subtle"
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

const (
	githubProviderID   = "github"
	githubDeviceURL    = "https://github.com/login/device/code"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubUserURL      = "https://api.github.com/user"
)

type authGitHubFile struct {
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	RedirectURL  string   `json:"redirect_url,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	AllowedUsers []string `json:"allowed_users,omitempty"`
}

type githubDeviceResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type githubTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

type githubAPIUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

func normalizeGitHubConfig(cfg authGitHubFile) authGitHubFile {
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.ClientSecret = strings.TrimSpace(cfg.ClientSecret)
	cfg.RedirectURL = strings.TrimSpace(cfg.RedirectURL)
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"read:user", "user:email"}
	} else {
		cfg.Scopes = compactStrings(cfg.Scopes)
	}
	cfg.AllowedUsers = compactStrings(cfg.AllowedUsers)
	for i := range cfg.AllowedUsers {
		cfg.AllowedUsers[i] = strings.ToLower(cfg.AllowedUsers[i])
	}
	return cfg
}

func (cfg authGitHubFile) enabled() bool {
	return strings.TrimSpace(cfg.ClientID) != ""
}

func (cfg authGitHubFile) scope() string {
	return strings.Join(cfg.Scopes, " ")
}

func (cfg authGitHubFile) allows(login string) bool {
	if len(cfg.AllowedUsers) == 0 {
		return true
	}
	login = strings.ToLower(strings.TrimSpace(login))
	for _, allowed := range cfg.AllowedUsers {
		if allowed == login {
			return true
		}
	}
	return false
}

func (s *Server) handleGitHubDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !s.auth.github.enabled() {
		writeError(w, http.StatusServiceUnavailable, "GitHub OAuth is not configured")
		return
	}
	form := url.Values{}
	form.Set("client_id", s.auth.github.ClientID)
	form.Set("scope", s.auth.github.scope())
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, githubDeviceURL, strings.NewReader(form.Encode()))
	if err != nil {
		respondErr(w, err)
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Happy-Pods")

	var out githubDeviceResponse
	if err := doGitHubJSON(req, &out); err != nil {
		respondErr(w, err)
		return
	}
	if out.Error != "" {
		writeError(w, http.StatusBadGateway, "GitHub device auth failed: %s", githubErrorMessage(out.Error, out.ErrorDescription))
		return
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	if out.VerificationURI == "" {
		out.VerificationURI = "https://github.com/login/device"
	}
	writeJSON(w, http.StatusOK, api.GitHubDeviceStart{
		DeviceCode:      out.DeviceCode,
		UserCode:        out.UserCode,
		VerificationURI: out.VerificationURI,
		ExpiresIn:       out.ExpiresIn,
		Interval:        out.Interval,
	})
}

func (s *Server) handleGitHubDevicePoll(w http.ResponseWriter, r *http.Request) {
	if !s.auth.github.enabled() {
		writeError(w, http.StatusServiceUnavailable, "GitHub OAuth is not configured")
		return
	}
	var in api.GitHubDevicePoll
	if err := json.NewDecoder(io.LimitReader(r.Body, maxDocBytes)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	deviceCode := strings.TrimSpace(in.DeviceCode)
	if deviceCode == "" {
		writeError(w, http.StatusBadRequest, "device_code is required")
		return
	}
	token, pending, denied, err := s.pollGitHubDevice(r, deviceCode)
	if err != nil {
		// Definitive denials (expired/declined) are 401 so the CLI stops;
		// transient upstream failures are 502 so the CLI keeps polling.
		status := http.StatusBadGateway
		if denied {
			status = http.StatusUnauthorized
		}
		writeError(w, status, "%s", err)
		return
	}
	if pending {
		writeJSON(w, http.StatusAccepted, api.GitHubDeviceToken{Pending: true, Interval: token.Interval})
		return
	}
	user, err := s.githubUser(r, token.AccessToken)
	if err != nil {
		respondErr(w, err)
		return
	}
	if !s.auth.github.allows(user.Login) {
		writeError(w, http.StatusForbidden, "GitHub user %q is not allowed", user.Login)
		return
	}
	authUser, err := s.identity.upsertUser(githubIdentity(user))
	if err != nil {
		respondErr(w, err)
		return
	}
	apiToken, expiresAt, err := s.auth.issueToken(authUser, time.Now())
	if err != nil {
		respondErr(w, err)
		return
	}
	profile := authUser.profile()
	writeJSON(w, http.StatusOK, api.GitHubDeviceToken{Token: apiToken, ExpiresAt: expiresAt, User: &profile})
}

// pollGitHubDevice polls GitHub once. pending is true while the user has not
// finished authorizing; denied distinguishes a definitive failure (the CLI
// should stop) from a transient upstream error (the CLI should keep polling).
func (s *Server) pollGitHubDevice(r *http.Request, deviceCode string) (out githubTokenResponse, pending, denied bool, err error) {
	form := url.Values{}
	form.Set("client_id", s.auth.github.ClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, githubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return out, false, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Happy-Pods")
	if err := doGitHubJSON(req, &out); err != nil {
		return out, false, false, err // transient upstream/network failure
	}
	switch out.Error {
	case "":
	case "authorization_pending", "slow_down":
		return out, true, false, nil
	default:
		return out, false, true, fmt.Errorf("GitHub device auth failed: %s", githubErrorMessage(out.Error, out.ErrorDescription))
	}
	if out.AccessToken == "" {
		return out, false, true, fmt.Errorf("GitHub device auth did not return an access token")
	}
	return out, false, false, nil
}

func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if !s.auth.github.enabled() {
		writeError(w, http.StatusServiceUnavailable, "GitHub OAuth is not configured")
		return
	}
	if s.auth.github.ClientSecret == "" {
		writeError(w, http.StatusServiceUnavailable, "GitHub OAuth client secret is not configured")
		return
	}
	nonce, err := randomString(18)
	if err != nil {
		respondErr(w, err)
		return
	}
	state := oauthState{
		Provider: githubProviderID,
		ReturnTo: s.safeReturnTo(r),
		Nonce:    nonce,
		Created:  time.Now().Unix(),
	}
	encoded, err := s.auth.secure.Encode(oauthStateName, state)
	if err != nil {
		respondErr(w, err)
		return
	}
	s.setOAuthStateCookie(w, r, nonce)
	values := url.Values{}
	values.Set("client_id", s.auth.github.ClientID)
	values.Set("redirect_uri", s.githubCallbackURL(r))
	values.Set("scope", s.auth.github.scope())
	values.Set("state", encoded)
	http.Redirect(w, r, githubAuthorizeURL+"?"+values.Encode(), http.StatusFound)
}

func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if !s.auth.github.enabled() || s.auth.github.ClientSecret == "" {
		writeError(w, http.StatusServiceUnavailable, "GitHub OAuth is not configured")
		return
	}
	var state oauthState
	if err := s.auth.secure.Decode(oauthStateName, r.URL.Query().Get("state"), &state); err != nil {
		writeError(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	if state.Provider != githubProviderID || time.Since(time.Unix(state.Created, 0)) > 10*time.Minute {
		writeError(w, http.StatusBadRequest, "expired oauth state")
		return
	}
	// Bind the callback to the browser that started the login: the nonce in
	// the signed state must match the one stored in the state cookie. This
	// prevents login CSRF / session fixation by state replay.
	cookie, err := r.Cookie(oauthStateName)
	s.clearOAuthStateCookie(w, r)
	if err != nil || cookie.Value == "" || state.Nonce == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state.Nonce)) != 1 {
		writeError(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeError(w, http.StatusBadRequest, "missing oauth code")
		return
	}
	token, err := s.exchangeGitHubCode(r, code)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "%s", err)
		return
	}
	user, err := s.githubUser(r, token.AccessToken)
	if err != nil {
		respondErr(w, err)
		return
	}
	if !s.auth.github.allows(user.Login) {
		writeError(w, http.StatusForbidden, "GitHub user %q is not allowed", user.Login)
		return
	}
	if _, err := s.identity.upsertUser(githubIdentity(user)); err != nil {
		respondErr(w, err)
		return
	}
	if !s.setSessionCookie(w, r, oauthSession{
		Provider:  githubProviderID,
		Subject:   strconv.FormatInt(user.ID, 10),
		Login:     strings.ToLower(user.Login),
		Email:     strings.ToLower(strings.TrimSpace(user.Email)),
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		Expires:   time.Now().Add(s.auth.sessionLength).Unix(),
	}) {
		return
	}
	http.Redirect(w, r, state.ReturnTo, http.StatusFound)
}

func (s *Server) exchangeGitHubCode(r *http.Request, code string) (githubTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", s.auth.github.ClientID)
	form.Set("client_secret", s.auth.github.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", s.githubCallbackURL(r))
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, githubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return githubTokenResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Happy-Pods")
	var out githubTokenResponse
	if err := doGitHubJSON(req, &out); err != nil {
		return out, err
	}
	if out.Error != "" {
		return out, fmt.Errorf("GitHub OAuth exchange failed: %s", githubErrorMessage(out.Error, out.ErrorDescription))
	}
	if out.AccessToken == "" {
		return out, fmt.Errorf("GitHub OAuth exchange did not return an access token")
	}
	return out, nil
}

func (s *Server) githubUser(r *http.Request, accessToken string) (githubAPIUser, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, githubUserURL, nil)
	if err != nil {
		return githubAPIUser{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Happy-Pods")
	var out githubAPIUser
	if err := doGitHubJSON(req, &out); err != nil {
		return out, err
	}
	if out.ID == 0 || strings.TrimSpace(out.Login) == "" {
		return out, fmt.Errorf("GitHub API did not return a user")
	}
	return out, nil
}

// setOAuthStateCookie stores the login nonce so the callback can confirm it
// is talking to the same browser. Scoped to the cookie domain so it survives
// the login-host → callback-host (apex) hop.
func (s *Server) setOAuthStateCookie(w http.ResponseWriter, r *http.Request, nonce string) {
	cookie := &http.Cookie{
		Name:     oauthStateName,
		Value:    nonce,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	}
	if s.auth.cookieDomain != "" {
		cookie.Domain = s.auth.cookieDomain
	}
	http.SetCookie(w, cookie)
}

func (s *Server) clearOAuthStateCookie(w http.ResponseWriter, r *http.Request) {
	cookie := &http.Cookie{
		Name:     oauthStateName,
		Value:    "",
		Path:     "/",
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

func (s *Server) githubCallbackURL(r *http.Request) string {
	if s.auth.github.RedirectURL != "" {
		return s.auth.github.RedirectURL
	}
	return currentRequestOrigin(r) + "/api/auth/callback/" + githubProviderID
}

// githubIdentity converts a GitHub API user into the provider-agnostic shape
// the identity store works with.
func githubIdentity(user githubAPIUser) providerIdentity {
	return providerIdentity{
		Provider:  githubProviderID,
		Subject:   strconv.FormatInt(user.ID, 10),
		Login:     user.Login,
		Name:      user.Name,
		Email:     user.Email,
		AvatarURL: user.AvatarURL,
	}
}

func githubUserIDFromSubject(subject string) string {
	return identityUserID(githubProviderID, strings.TrimSpace(subject))
}

func doGitHubJSON(req *http.Request, out any) error {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GitHub returned %s", resp.Status)
	}
	return nil
}

func githubErrorMessage(code, description string) string {
	if description != "" {
		return code + ": " + description
	}
	return code
}
