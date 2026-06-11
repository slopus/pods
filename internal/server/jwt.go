package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// apiTokenTTL is how long an issued API token lives. Clients refresh it
// through POST /api/auth/refresh any time before it expires; an expired
// token requires a fresh login.
const apiTokenTTL = 30 * 24 * time.Hour

// jwtClaims is the payload of a podbay API token: a single HS256 JWT,
// refreshable before expiration. No separate refresh token exists.
type jwtClaims struct {
	Subject   string `json:"sub"`
	Login     string `json:"login,omitempty"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func signJWT(key []byte, claims jwtClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("jwt: encode claims: %w", err)
	}
	signingInput := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signingInput + "." + jwtSignature(key, signingInput), nil
}

func verifyJWT(key []byte, token string, now time.Time) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errors.New("jwt: malformed token")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtClaims{}, errors.New("jwt: malformed header")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "HS256" {
		return jwtClaims{}, errors.New("jwt: unsupported algorithm")
	}
	signingInput := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(jwtSignature(key, signingInput)), []byte(parts[2])) {
		return jwtClaims{}, errors.New("jwt: invalid signature")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, errors.New("jwt: malformed payload")
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return jwtClaims{}, errors.New("jwt: malformed claims")
	}
	if claims.Subject == "" {
		return jwtClaims{}, errors.New("jwt: missing subject")
	}
	if claims.ExpiresAt <= now.Unix() {
		return jwtClaims{}, errors.New("jwt: token expired")
	}
	return claims, nil
}

func jwtSignature(key []byte, signingInput string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// issueToken mints the single refreshable API token for an identity user.
func (a *authenticator) issueToken(user *authUser, now time.Time) (token string, expiresAt time.Time, err error) {
	expiresAt = now.Add(apiTokenTTL)
	token, err = signJWT(a.tokenKey, jwtClaims{
		Subject:   user.ID,
		Login:     user.Login,
		IssuedAt:  now.Unix(),
		ExpiresAt: expiresAt.Unix(),
	})
	return token, expiresAt, err
}
