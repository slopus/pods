package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// identityStore is the dedicated authentication database: users who signed in
// through an identity provider, and which user owns which site. Identity is
// provider-agnostic — user IDs are "<provider>:<subject>" — though GitHub is
// the only provider wired up today.
type identityStore struct {
	db  *sql.DB
	now func() time.Time
}

// providerIdentity is a user as reported by an identity provider.
type providerIdentity struct {
	Provider  string // e.g. "github"
	Subject   string // stable user id at the provider
	Login     string
	Name      string
	Email     string
	AvatarURL string
}

type siteOwner struct {
	ID    string
	Login string
}

func openIdentityStore(path string) (*identityStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("identity: create dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("identity: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &identityStore{db: db, now: time.Now}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *identityStore) init() error {
	stmts := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			subject TEXT NOT NULL,
			login TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE (provider, subject)
		)`,
		`CREATE TABLE IF NOT EXISTS site_owners (
			site TEXT PRIMARY KEY,
			owner_id TEXT NOT NULL,
			owner_login TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("identity: init sqlite: %w", err)
		}
	}
	return s.migrateLegacyTables()
}

// migrateLegacyTables folds the GitHub-only schema (github_users plus opaque
// api_tokens) into the provider-agnostic users table. Opaque tokens are gone
// in favor of stateless JWTs, so api_tokens is simply dropped.
func (s *identityStore) migrateLegacyTables() error {
	var name string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'github_users'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("identity: inspect legacy tables: %w", err)
	}
	stmts := []string{
		`INSERT OR IGNORE INTO users (id, provider, subject, login, name, email, avatar_url, created_at, updated_at)
			SELECT id, 'github', CAST(github_id AS TEXT), login, name, email, avatar_url, created_at, updated_at
			FROM github_users`,
		`DROP TABLE IF EXISTS api_tokens`,
		`DROP TABLE github_users`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("identity: migrate legacy tables: %w", err)
		}
	}
	return nil
}

func (s *identityStore) upsertUser(p providerIdentity) (*authUser, error) {
	provider := strings.ToLower(strings.TrimSpace(p.Provider))
	subject := strings.TrimSpace(p.Subject)
	login := strings.ToLower(strings.TrimSpace(p.Login))
	if provider == "" || subject == "" || login == "" {
		return nil, errors.New("identity: provider, subject and login are required")
	}
	user := &authUser{
		ID:        identityUserID(provider, subject),
		Login:     login,
		Name:      strings.TrimSpace(p.Name),
		Email:     strings.ToLower(strings.TrimSpace(p.Email)),
		AvatarURL: strings.TrimSpace(p.AvatarURL),
	}
	now := s.now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`INSERT INTO users (id, provider, subject, login, name, email, avatar_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, subject) DO UPDATE SET
			login = excluded.login,
			name = excluded.name,
			email = excluded.email,
			avatar_url = excluded.avatar_url,
			updated_at = excluded.updated_at`,
		user.ID, provider, subject, user.Login, user.Name, user.Email, user.AvatarURL, now, now)
	if err != nil {
		return nil, fmt.Errorf("identity: upsert user: %w", err)
	}
	return user, nil
}

func (s *identityStore) userByID(id string) (*authUser, bool) {
	var user authUser
	err := s.db.QueryRow(`SELECT id, login, name, email, avatar_url FROM users WHERE id = ?`, id).
		Scan(&user.ID, &user.Login, &user.Name, &user.Email, &user.AvatarURL)
	if err != nil {
		return nil, false
	}
	return &user, true
}

func (s *identityStore) siteOwner(site string) (siteOwner, bool, error) {
	var owner siteOwner
	err := s.db.QueryRow(`SELECT owner_id, owner_login FROM site_owners WHERE site = ?`, site).Scan(&owner.ID, &owner.Login)
	if errors.Is(err, sql.ErrNoRows) {
		return siteOwner{}, false, nil
	}
	if err != nil {
		return siteOwner{}, false, fmt.Errorf("identity: get site owner: %w", err)
	}
	return owner, true, nil
}

// claimSite records the owner of a site the FIRST time it is deployed and
// never reassigns it afterward: a redeploy (by the owner, or by an admin
// hotfixing someone else's site) only bumps updated_at, so ownership can't be
// silently stolen.
func (s *identityStore) claimSite(site string, user *authUser, updatedAt time.Time) error {
	owner := ownerFromUser(user)
	now := s.now().UTC().Format(time.RFC3339)
	if !updatedAt.IsZero() {
		now = updatedAt.UTC().Format(time.RFC3339)
	}
	_, err := s.db.Exec(`INSERT INTO site_owners (site, owner_id, owner_login, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(site) DO UPDATE SET
			updated_at = excluded.updated_at`,
		site, owner.ID, owner.Login, now, now)
	if err != nil {
		return fmt.Errorf("identity: claim site: %w", err)
	}
	return nil
}

func (s *identityStore) removeSite(site string) error {
	if _, err := s.db.Exec(`DELETE FROM site_owners WHERE site = ?`, site); err != nil {
		return fmt.Errorf("identity: remove site owner: %w", err)
	}
	return nil
}

func ownerFromUser(user *authUser) siteOwner {
	owner := siteOwner{ID: user.ID, Login: user.Login}
	if owner.Login == "" {
		owner.Login = user.ID
	}
	return owner
}

func identityUserID(provider, subject string) string {
	return provider + ":" + subject
}
