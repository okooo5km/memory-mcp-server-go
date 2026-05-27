// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type persistentStore struct {
	db *sql.DB
}

func openPersistentStore(path string) (*persistentStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create oauth store directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open oauth store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable oauth store WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set oauth store busy timeout: %w", err)
	}
	if err := createPersistentSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := chmodOAuthStoreFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	return &persistentStore{db: db}, nil
}

func chmodOAuthStoreFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("secure oauth store permissions for %s: %w", p, err)
		}
	}
	return nil
}

func createPersistentSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS oauth_clients (
		client_id TEXT PRIMARY KEY,
		client_secret TEXT NOT NULL DEFAULT '',
		client_name TEXT NOT NULL DEFAULT '',
		redirect_uris TEXT NOT NULL,
		grant_types TEXT NOT NULL,
		response_types TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS oauth_access_tokens (
		token TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		refresh_token TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_oauth_access_expires ON oauth_access_tokens(expires_at);

	CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
		token TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		replaced_at INTEGER NOT NULL DEFAULT 0,
		replacement_access_token TEXT NOT NULL DEFAULT '',
		replacement_refresh_token TEXT NOT NULL DEFAULT '',
		replacement_access_expires_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_oauth_refresh_expires ON oauth_refresh_tokens(expires_at);
	`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create oauth store schema: %w", err)
	}
	return nil
}

func (s *persistentStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *persistentStore) putClient(c *OAuthClient) error {
	redirectURIs, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	grantTypes, err := json.Marshal(c.GrantTypes)
	if err != nil {
		return err
	}
	responseTypes, err := json.Marshal(c.ResponseTypes)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO oauth_clients (client_id, client_secret, client_name, redirect_uris, grant_types, response_types, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET
			client_secret = excluded.client_secret,
			client_name = excluded.client_name,
			redirect_uris = excluded.redirect_uris,
			grant_types = excluded.grant_types,
			response_types = excluded.response_types
	`, c.ClientID, c.ClientSecret, c.ClientName, string(redirectURIs), string(grantTypes), string(responseTypes), c.CreatedAt.UnixNano())
	return err
}

func (s *persistentStore) getClient(clientID string) (*OAuthClient, bool) {
	var c OAuthClient
	var redirectURIs, grantTypes, responseTypes string
	var createdAt int64
	err := s.db.QueryRow(`
		SELECT client_id, client_secret, client_name, redirect_uris, grant_types, response_types, created_at
		FROM oauth_clients WHERE client_id = ?
	`, clientID).Scan(&c.ClientID, &c.ClientSecret, &c.ClientName, &redirectURIs, &grantTypes, &responseTypes, &createdAt)
	if err != nil {
		return nil, false
	}
	if err := json.Unmarshal([]byte(redirectURIs), &c.RedirectURIs); err != nil {
		return nil, false
	}
	if err := json.Unmarshal([]byte(grantTypes), &c.GrantTypes); err != nil {
		return nil, false
	}
	if err := json.Unmarshal([]byte(responseTypes), &c.ResponseTypes); err != nil {
		return nil, false
	}
	c.CreatedAt = timeFromUnixNano(createdAt)
	return &c, true
}

func (s *persistentStore) putAccessToken(t *TokenEntry) error {
	_, err := s.db.Exec(`
		INSERT INTO oauth_access_tokens (token, client_id, expires_at, refresh_token)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET
			client_id = excluded.client_id,
			expires_at = excluded.expires_at,
			refresh_token = excluded.refresh_token
	`, t.Token, t.ClientID, t.ExpiresAt.UnixNano(), t.RefreshToken)
	return err
}

func (s *persistentStore) getAccessToken(token string) (*TokenEntry, bool) {
	var t TokenEntry
	var expiresAt int64
	err := s.db.QueryRow(`
		SELECT token, client_id, expires_at, refresh_token
		FROM oauth_access_tokens WHERE token = ?
	`, token).Scan(&t.Token, &t.ClientID, &expiresAt, &t.RefreshToken)
	if err != nil {
		return nil, false
	}
	t.ExpiresAt = timeFromUnixNano(expiresAt)
	return &t, true
}

func (s *persistentStore) deleteAccessToken(token string) error {
	_, err := s.db.Exec("DELETE FROM oauth_access_tokens WHERE token = ?", token)
	return err
}

func (s *persistentStore) putRefreshToken(t *RefreshTokenEntry) error {
	_, err := s.db.Exec(`
		INSERT INTO oauth_refresh_tokens (
			token, client_id, expires_at, replaced_at,
			replacement_access_token, replacement_refresh_token, replacement_access_expires_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET
			client_id = excluded.client_id,
			expires_at = excluded.expires_at,
			replaced_at = excluded.replaced_at,
			replacement_access_token = excluded.replacement_access_token,
			replacement_refresh_token = excluded.replacement_refresh_token,
			replacement_access_expires_at = excluded.replacement_access_expires_at
	`, t.Token, t.ClientID, t.ExpiresAt.UnixNano(), unixNanoOrZero(t.ReplacedAt),
		t.ReplacementAccessToken, t.ReplacementRefreshToken, unixNanoOrZero(t.ReplacementAccessExpiresAt))
	return err
}

func (s *persistentStore) getRefreshToken(token string) (*RefreshTokenEntry, bool) {
	var t RefreshTokenEntry
	var expiresAt, replacedAt, replacementAccessExpiresAt int64
	err := s.db.QueryRow(`
		SELECT token, client_id, expires_at, replaced_at,
			replacement_access_token, replacement_refresh_token, replacement_access_expires_at
		FROM oauth_refresh_tokens WHERE token = ?
	`, token).Scan(&t.Token, &t.ClientID, &expiresAt, &replacedAt,
		&t.ReplacementAccessToken, &t.ReplacementRefreshToken, &replacementAccessExpiresAt)
	if err != nil {
		return nil, false
	}
	t.ExpiresAt = timeFromUnixNano(expiresAt)
	t.ReplacedAt = timeFromUnixNano(replacedAt)
	t.ReplacementAccessExpiresAt = timeFromUnixNano(replacementAccessExpiresAt)
	return &t, true
}

func (s *persistentStore) deleteRefreshToken(token string) error {
	_, err := s.db.Exec("DELETE FROM oauth_refresh_tokens WHERE token = ?", token)
	return err
}

func (s *persistentStore) rotateRefreshToken(oldRefreshToken string, access *TokenEntry, replacementRefresh *RefreshTokenEntry, oldRefresh *RefreshTokenEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO oauth_access_tokens (token, client_id, expires_at, refresh_token)
		VALUES (?, ?, ?, ?)
	`, access.Token, access.ClientID, access.ExpiresAt.UnixNano(), access.RefreshToken); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO oauth_refresh_tokens (token, client_id, expires_at)
		VALUES (?, ?, ?)
	`, replacementRefresh.Token, replacementRefresh.ClientID, replacementRefresh.ExpiresAt.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE oauth_refresh_tokens SET
			replaced_at = ?,
			replacement_access_token = ?,
			replacement_refresh_token = ?,
			replacement_access_expires_at = ?
		WHERE token = ?
	`, oldRefresh.ReplacedAt.UnixNano(), oldRefresh.ReplacementAccessToken, oldRefresh.ReplacementRefreshToken,
		oldRefresh.ReplacementAccessExpiresAt.UnixNano(), oldRefreshToken); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *persistentStore) cleanup(now time.Time) {
	_, _ = s.db.Exec("DELETE FROM oauth_access_tokens WHERE expires_at < ?", now.UnixNano())
	_, _ = s.db.Exec("DELETE FROM oauth_refresh_tokens WHERE expires_at < ?", now.UnixNano())
}

func unixNanoOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeFromUnixNano(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v)
}
