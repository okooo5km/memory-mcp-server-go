// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type persistentStore struct {
	db *sql.DB
}

// oauthSchemaVersion is the PRAGMA user_version of the current schema.
// 0 is the legacy layout that stored tokens in plaintext.
const oauthSchemaVersion = 1

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
	// Overwrite deleted content with zeros so removed rows (including legacy
	// plaintext) do not linger in free pages. Per connection, and the pool
	// holds exactly one.
	if _, err := db.Exec("PRAGMA secure_delete=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable oauth store secure delete: %w", err)
	}
	migrated, err := migratePersistentSchema(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if migrated >= 0 {
		// The legacy plaintext may still sit in the WAL and in free pages of
		// the main file; flush the WAL and rewrite the file without them.
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			db.Close()
			return nil, fmt.Errorf("checkpoint oauth store after migration: %w", err)
		}
		if _, err := db.Exec("VACUUM"); err != nil {
			db.Close()
			return nil, fmt.Errorf("vacuum oauth store after migration: %w", err)
		}
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			db.Close()
			return nil, fmt.Errorf("checkpoint oauth store after migration: %w", err)
		}
		log.Printf("OAuth store migrated to hashed token storage: %d tokens converted", migrated)
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

const oauthClientsDDL = `
	CREATE TABLE IF NOT EXISTS oauth_clients (
		client_id TEXT PRIMARY KEY,
		client_secret TEXT NOT NULL DEFAULT '',
		client_name TEXT NOT NULL DEFAULT '',
		redirect_uris TEXT NOT NULL,
		grant_types TEXT NOT NULL,
		response_types TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);`

// tokenTablesDDL creates the hashed token tables under the given name suffix.
func tokenTablesDDL(suffix string) string {
	return `
	CREATE TABLE oauth_access_tokens` + suffix + ` (
		token_hash TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		refresh_hash TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE oauth_refresh_tokens` + suffix + ` (
		token_hash TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		replaced_at INTEGER NOT NULL DEFAULT 0,
		replacement_access_hash TEXT NOT NULL DEFAULT '',
		replacement_refresh_hash TEXT NOT NULL DEFAULT '',
		replacement_sealed BLOB,
		replacement_access_expires_at INTEGER NOT NULL DEFAULT 0
	);`
}

const tokenIndexesDDL = `
	CREATE INDEX IF NOT EXISTS idx_oauth_access_expires ON oauth_access_tokens(expires_at);
	CREATE INDEX IF NOT EXISTS idx_oauth_access_refresh ON oauth_access_tokens(refresh_hash);
	CREATE INDEX IF NOT EXISTS idx_oauth_refresh_expires ON oauth_refresh_tokens(expires_at);
	CREATE INDEX IF NOT EXISTS idx_oauth_refresh_replacement ON oauth_refresh_tokens(replacement_refresh_hash);`

// migratePersistentSchema brings the database to oauthSchemaVersion inside a
// single transaction, so a failure leaves the file untouched. It returns the
// number of legacy tokens converted, or -1 when nothing was migrated (fresh
// database or already current).
func migratePersistentSchema(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return -1, fmt.Errorf("read oauth store schema version: %w", err)
	}
	if version > oauthSchemaVersion {
		return -1, fmt.Errorf("oauth store schema version %d is newer than the supported version %d; upgrade mms or use another --oauth-store path", version, oauthSchemaVersion)
	}
	if version == oauthSchemaVersion {
		return -1, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return -1, fmt.Errorf("begin oauth store migration: %w", err)
	}
	defer tx.Rollback()

	var legacy int
	if err := tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='oauth_access_tokens'").Scan(&legacy); err != nil {
		return -1, fmt.Errorf("inspect oauth store schema: %w", err)
	}
	if _, err := tx.Exec(oauthClientsDDL); err != nil {
		return -1, fmt.Errorf("create oauth store schema: %w", err)
	}
	if legacy == 0 {
		// Fresh database: create the current schema directly.
		if _, err := tx.Exec(tokenTablesDDL("") + tokenIndexesDDL); err != nil {
			return -1, fmt.Errorf("create oauth store schema: %w", err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", oauthSchemaVersion)); err != nil {
			return -1, fmt.Errorf("set oauth store schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return -1, fmt.Errorf("commit oauth store schema: %w", err)
		}
		return -1, nil
	}

	count, err := migrateLegacyTokens(tx)
	if err != nil {
		return -1, fmt.Errorf("migrate oauth store to hashed tokens: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", oauthSchemaVersion)); err != nil {
		return -1, fmt.Errorf("set oauth store schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return -1, fmt.Errorf("commit oauth store migration: %w", err)
	}
	return count, nil
}

// migrateLegacyTokens rewrites the plaintext legacy token tables as hashed
// tables. Legacy replacement pairs are sealed with the plaintext of the
// refresh token they replaced, which is still available in the legacy row.
func migrateLegacyTokens(tx *sql.Tx) (int, error) {
	type legacyAccess struct {
		token, clientID, refresh string
		expiresAt                int64
	}
	type legacyRefresh struct {
		token, clientID, replAccess, replRefresh string
		expiresAt, replacedAt, replAccessExpires int64
	}
	var accesses []legacyAccess
	var refreshes []legacyRefresh

	rows, err := tx.Query("SELECT token, client_id, expires_at, refresh_token FROM oauth_access_tokens")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var a legacyAccess
		if err := rows.Scan(&a.token, &a.clientID, &a.expiresAt, &a.refresh); err != nil {
			rows.Close()
			return 0, err
		}
		accesses = append(accesses, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	rows, err = tx.Query(`SELECT token, client_id, expires_at, replaced_at,
		replacement_access_token, replacement_refresh_token, replacement_access_expires_at
		FROM oauth_refresh_tokens`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var r legacyRefresh
		if err := rows.Scan(&r.token, &r.clientID, &r.expiresAt, &r.replacedAt, &r.replAccess, &r.replRefresh, &r.replAccessExpires); err != nil {
			rows.Close()
			return 0, err
		}
		refreshes = append(refreshes, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	if _, err := tx.Exec(tokenTablesDDL("_new")); err != nil {
		return 0, err
	}
	for _, a := range accesses {
		if _, err := tx.Exec(`INSERT INTO oauth_access_tokens_new (token_hash, client_id, expires_at, refresh_hash)
			VALUES (?, ?, ?, ?)`, hashToken(a.token), a.clientID, a.expiresAt, hashTokenOrEmpty(a.refresh)); err != nil {
			return 0, err
		}
	}
	for _, r := range refreshes {
		var sealed []byte
		replAccessHash, replRefreshHash := "", ""
		if r.replAccess != "" && r.replRefresh != "" {
			if sealed, err = sealReplacement(r.token, r.replAccess, r.replRefresh); err != nil {
				return 0, err
			}
			replAccessHash, replRefreshHash = hashToken(r.replAccess), hashToken(r.replRefresh)
		}
		if _, err := tx.Exec(`INSERT INTO oauth_refresh_tokens_new (
				token_hash, client_id, expires_at, replaced_at,
				replacement_access_hash, replacement_refresh_hash, replacement_sealed, replacement_access_expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			hashToken(r.token), r.clientID, r.expiresAt, r.replacedAt,
			replAccessHash, replRefreshHash, sealed, r.replAccessExpires); err != nil {
			return 0, err
		}
	}
	for _, stmt := range []string{
		"DROP TABLE oauth_access_tokens",
		"DROP TABLE oauth_refresh_tokens",
		"ALTER TABLE oauth_access_tokens_new RENAME TO oauth_access_tokens",
		"ALTER TABLE oauth_refresh_tokens_new RENAME TO oauth_refresh_tokens",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(tokenIndexesDDL); err != nil {
		return 0, err
	}
	return len(accesses) + len(refreshes), nil
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
		INSERT INTO oauth_access_tokens (token_hash, client_id, expires_at, refresh_hash)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(token_hash) DO UPDATE SET
			client_id = excluded.client_id,
			expires_at = excluded.expires_at,
			refresh_hash = excluded.refresh_hash
	`, t.TokenHash, t.ClientID, t.ExpiresAt.UnixNano(), t.RefreshHash)
	return err
}

func (s *persistentStore) getAccessToken(tokenHash string) (*TokenEntry, bool) {
	var t TokenEntry
	var expiresAt int64
	err := s.db.QueryRow(`
		SELECT token_hash, client_id, expires_at, refresh_hash
		FROM oauth_access_tokens WHERE token_hash = ?
	`, tokenHash).Scan(&t.TokenHash, &t.ClientID, &expiresAt, &t.RefreshHash)
	if err != nil {
		return nil, false
	}
	t.ExpiresAt = timeFromUnixNano(expiresAt)
	return &t, true
}

func (s *persistentStore) deleteAccessToken(tokenHash string) error {
	_, err := s.db.Exec("DELETE FROM oauth_access_tokens WHERE token_hash = ?", tokenHash)
	return err
}

func (s *persistentStore) putRefreshToken(t *RefreshTokenEntry) error {
	_, err := s.db.Exec(`
		INSERT INTO oauth_refresh_tokens (
			token_hash, client_id, expires_at, replaced_at,
			replacement_access_hash, replacement_refresh_hash, replacement_sealed, replacement_access_expires_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(token_hash) DO UPDATE SET
			client_id = excluded.client_id,
			expires_at = excluded.expires_at,
			replaced_at = excluded.replaced_at,
			replacement_access_hash = excluded.replacement_access_hash,
			replacement_refresh_hash = excluded.replacement_refresh_hash,
			replacement_sealed = excluded.replacement_sealed,
			replacement_access_expires_at = excluded.replacement_access_expires_at
	`, t.TokenHash, t.ClientID, t.ExpiresAt.UnixNano(), unixNanoOrZero(t.ReplacedAt),
		t.ReplacementAccessHash, t.ReplacementRefreshHash, t.ReplacementSealed, unixNanoOrZero(t.ReplacementAccessExpiresAt))
	return err
}

func (s *persistentStore) getRefreshToken(tokenHash string) (*RefreshTokenEntry, bool) {
	var t RefreshTokenEntry
	var expiresAt, replacedAt, replacementAccessExpiresAt int64
	err := s.db.QueryRow(`
		SELECT token_hash, client_id, expires_at, replaced_at,
			replacement_access_hash, replacement_refresh_hash, replacement_sealed, replacement_access_expires_at
		FROM oauth_refresh_tokens WHERE token_hash = ?
	`, tokenHash).Scan(&t.TokenHash, &t.ClientID, &expiresAt, &replacedAt,
		&t.ReplacementAccessHash, &t.ReplacementRefreshHash, &t.ReplacementSealed, &replacementAccessExpiresAt)
	if err != nil {
		return nil, false
	}
	t.ExpiresAt = timeFromUnixNano(expiresAt)
	t.ReplacedAt = timeFromUnixNano(replacedAt)
	t.ReplacementAccessExpiresAt = timeFromUnixNano(replacementAccessExpiresAt)
	return &t, true
}

func (s *persistentStore) deleteRefreshToken(tokenHash string) error {
	_, err := s.db.Exec("DELETE FROM oauth_refresh_tokens WHERE token_hash = ?", tokenHash)
	return err
}

func (s *persistentStore) deleteAccessTokensByRefresh(refreshHash string) error {
	_, err := s.db.Exec("DELETE FROM oauth_access_tokens WHERE refresh_hash = ?", refreshHash)
	return err
}

func (s *persistentStore) findReplacedRefresh(replacementRefreshHash string) (string, bool) {
	var h string
	err := s.db.QueryRow("SELECT token_hash FROM oauth_refresh_tokens WHERE replacement_refresh_hash = ? LIMIT 1", replacementRefreshHash).Scan(&h)
	if err != nil {
		return "", false
	}
	return h, true
}

func (s *persistentStore) rotateRefreshToken(oldRefreshHash string, access *TokenEntry, replacementRefresh *RefreshTokenEntry, oldRefresh *RefreshTokenEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO oauth_access_tokens (token_hash, client_id, expires_at, refresh_hash)
		VALUES (?, ?, ?, ?)
	`, access.TokenHash, access.ClientID, access.ExpiresAt.UnixNano(), access.RefreshHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO oauth_refresh_tokens (token_hash, client_id, expires_at)
		VALUES (?, ?, ?)
	`, replacementRefresh.TokenHash, replacementRefresh.ClientID, replacementRefresh.ExpiresAt.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE oauth_refresh_tokens SET
			replaced_at = ?,
			replacement_access_hash = ?,
			replacement_refresh_hash = ?,
			replacement_sealed = ?,
			replacement_access_expires_at = ?
		WHERE token_hash = ?
	`, oldRefresh.ReplacedAt.UnixNano(), oldRefresh.ReplacementAccessHash, oldRefresh.ReplacementRefreshHash,
		oldRefresh.ReplacementSealed, oldRefresh.ReplacementAccessExpiresAt.UnixNano(), oldRefreshHash); err != nil {
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
