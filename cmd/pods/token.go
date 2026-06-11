package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/slopus/pods/internal/client"
)

// refreshWindow is how close to expiry the CLI starts refreshing the API
// token. The server hands out one JWT; refreshing it before it expires is the
// only way to extend a session without logging in again.
const refreshWindow = 7 * 24 * time.Hour

// maybeRefreshToken transparently refreshes the configured API token when it
// expires soon. Best effort: any failure leaves the current token in place
// and the actual command reports the real error. The refreshed token is only
// persisted when the in-use token came from the config file.
func maybeRefreshToken(c *client.Client, cfg config) {
	exp, ok := jwtExpiry(cfg.Secret)
	if !ok {
		return // not a JWT (static admin token, or no token at all)
	}
	now := time.Now()
	if !exp.After(now) || exp.Sub(now) > refreshWindow {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.Refresh(ctx)
	if err != nil || res.Token == "" {
		return
	}
	path, err := configPath()
	if err != nil {
		return
	}
	file, err := loadConfigFile(path)
	if err != nil || file.Secret != cfg.Secret {
		return // token came from a flag or the environment; nothing to persist
	}
	file.Secret = res.Token
	_ = saveConfigFile(path, file)
}

// jwtExpiry extracts the exp claim from a JWT without verifying it (the
// server is the one that verifies; the CLI only schedules refreshes).
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.ExpiresAt, 0), true
}
