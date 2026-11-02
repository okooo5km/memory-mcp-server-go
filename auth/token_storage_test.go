// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The exact DDL written by versions that stored tokens in plaintext.
const legacyOAuthDDL = `
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

type legacyFixture struct {
	clientID string
	// active session: access + refresh
	activeAccess, activeRefresh string
	// rotated chain: oldRefresh was replaced by replAccess/replRefresh
	oldRefresh, replAccess, replRefresh string
}

// writeLegacyStore creates a plaintext-token oauth.db exactly as an older
// binary would have.
func writeLegacyStore(t *testing.T, path string) legacyFixture {
	t.Helper()
	mk := func() string {
		tok, err := generateToken(32)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	f := legacyFixture{
		clientID:      "legacy-client-0123456789abcdef",
		activeAccess:  mk(),
		activeRefresh: mk(),
		oldRefresh:    mk(),
		replAccess:    mk(),
		replRefresh:   mk(),
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacyOAuthDDL); err != nil {
		t.Fatal(err)
	}
	now := timeNow()
	accessExp := now.Add(accessTokenTTL).UnixNano()
	refreshExp := now.Add(refreshTokenTTL).UnixNano()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO oauth_access_tokens VALUES (?, ?, ?, ?)`, f.activeAccess, f.clientID, accessExp, f.activeRefresh)
	exec(`INSERT INTO oauth_refresh_tokens (token, client_id, expires_at) VALUES (?, ?, ?)`, f.activeRefresh, f.clientID, refreshExp)
	exec(`INSERT INTO oauth_access_tokens VALUES (?, ?, ?, ?)`, f.replAccess, f.clientID, accessExp, f.replRefresh)
	exec(`INSERT INTO oauth_refresh_tokens (token, client_id, expires_at) VALUES (?, ?, ?)`, f.replRefresh, f.clientID, refreshExp)
	exec(`INSERT INTO oauth_refresh_tokens VALUES (?, ?, ?, ?, ?, ?, ?)`, f.oldRefresh, f.clientID, refreshExp,
		now.Add(-time.Minute).UnixNano(), f.replAccess, f.replRefresh, accessExp)
	return f
}

// openServerAt opens an OAuth server on a store path. The returned close
// function is idempotent and also registered as a cleanup.
func openServerAt(t *testing.T, path string) (*OAuthServer, http.Handler, func()) {
	t.Helper()
	srv, err := NewOAuthServerWithError(Config{Username: "admin", Password: "secret", StorePath: path})
	if err != nil {
		t.Fatalf("open oauth server: %v", err)
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, func(h http.Handler) http.Handler { return h })
	var once sync.Once
	closeFn := func() { once.Do(srv.Close) }
	t.Cleanup(closeFn)
	return srv, mux, closeFn
}

func refreshForm(refresh, clientID string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// assertNoPlaintext fails if any token appears in the store files, either as
// text or as the raw bytes behind its hex form.
func assertNoPlaintext(t *testing.T, path string, tokens ...string) {
	t.Helper()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range tokens {
			if tok == "" {
				continue
			}
			if bytes.Contains(data, []byte(tok)) {
				t.Errorf("%s contains a plaintext token", filepath.Base(p))
			}
			if raw, err := hex.DecodeString(tok); err == nil && bytes.Contains(data, raw) {
				t.Errorf("%s contains a token in raw binary form", filepath.Base(p))
			}
		}
	}
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLegacyStoreMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	f := writeLegacyStore(t, path)
	all := []string{f.activeAccess, f.activeRefresh, f.oldRefresh, f.replAccess, f.replRefresh}
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("legacy user_version = %d", v)
	}

	logs := captureLog(t)
	srv, mux, closeSrv := openServerAt(t, path)
	if !strings.Contains(logs.String(), "migrated to hashed token storage: 5 tokens") {
		t.Errorf("expected a migration log line with the token count, got %q", logs.String())
	}
	for _, tok := range all {
		if strings.Contains(logs.String(), tok) || strings.Contains(logs.String(), hashToken(tok)) {
			t.Error("migration log leaks token material")
		}
	}
	if v := userVersion(t, path); v != oauthSchemaVersion {
		t.Fatalf("user_version = %d after migration", v)
	}
	// The plaintext must be gone right after migration, even before Close.
	assertNoPlaintext(t, path, all...)

	t.Run("access token validates", func(t *testing.T) {
		if _, err := srv.verifyToken(nil, f.activeAccess, nil); err != nil {
			t.Fatalf("legacy access token rejected: %v", err)
		}
	})
	t.Run("replay returns the identical pre-migration pair", func(t *testing.T) {
		w := postForm(mux, "/token", refreshForm(f.oldRefresh, f.clientID))
		if w.Code != 200 {
			t.Fatalf("replay: %d %s", w.Code, w.Body.String())
		}
		m := jsonBody(t, w)
		if m["access_token"] != f.replAccess || m["refresh_token"] != f.replRefresh {
			t.Fatal("replay did not return the pair issued before migration")
		}
	})
	t.Run("legacy refresh token rotates", func(t *testing.T) {
		w := postForm(mux, "/token", refreshForm(f.activeRefresh, f.clientID))
		if w.Code != 200 {
			t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
		}
		m := jsonBody(t, w)
		if m["refresh_token"] == f.activeRefresh || m["access_token"] == "" {
			t.Fatal("expected a fresh pair")
		}
		all = append(all, m["access_token"].(string), m["refresh_token"].(string))
		if _, err := srv.verifyToken(nil, m["access_token"].(string), nil); err != nil {
			t.Fatalf("new access token rejected: %v", err)
		}
	})
	t.Run("revocation chain works across the migration", func(t *testing.T) {
		if c := postForm(mux, "/revoke", url.Values{"token": {f.replRefresh}, "client_id": {f.clientID}}).Code; c != 200 {
			t.Fatalf("revoke: %d", c)
		}
		if _, err := srv.verifyToken(nil, f.replAccess, nil); err == nil {
			t.Error("access token of the revoked chain must be invalid")
		}
		if c := postForm(mux, "/token", refreshForm(f.oldRefresh, f.clientID)).Code; c != 400 {
			t.Errorf("rotated predecessor must not replay after revocation, got %d", c)
		}
	})

	closeSrv()
	assertNoPlaintext(t, path, all...)

	t.Run("second open is a no-op", func(t *testing.T) {
		logs.Reset()
		srv2, _, _ := openServerAt(t, path)
		if strings.Contains(logs.String(), "migrated") {
			t.Errorf("second open migrated again: %q", logs.String())
		}
		if v := userVersion(t, path); v != oauthSchemaVersion {
			t.Errorf("user_version = %d", v)
		}
		if _, err := srv2.verifyToken(nil, f.activeAccess, nil); err != nil {
			t.Errorf("token lost on second open: %v", err)
		}
	})
}

func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	f := writeLegacyStore(t, path)
	// Sabotage: a table squatting on the temporary name makes the migration fail midway.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE oauth_refresh_tokens_new (x)"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := NewOAuthServerWithError(Config{Username: "a", Password: "b", StorePath: path}); err == nil {
		t.Fatal("expected startup error")
	}
	if v := userVersion(t, path); v != 0 {
		t.Errorf("user_version = %d after failed migration, want 0", v)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM oauth_access_tokens WHERE token = ?", f.activeAccess).Scan(&n); err != nil || n != 1 {
		t.Errorf("legacy data must be untouched after rollback: n=%d err=%v", n, err)
	}
}

func TestRefuseNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = NewOAuthServerWithError(Config{Username: "a", Password: "b", StorePath: path})
	if err == nil || !strings.Contains(err.Error(), "schema version 99") {
		t.Fatalf("expected a clear schema version error, got %v", err)
	}
}

// runFullFlow drives authorize -> token -> refresh -> replay and returns every
// token that was ever handed out.
func runFullFlow(t *testing.T, mux http.Handler) (tokens []string, clientID, oldRefresh string, second map[string]any) {
	t.Helper()
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	code := authorizeCode(t, mux, authzQuery(cid, "https://app.example.com/cb", nil))
	first := jsonBody(t, postForm(mux, "/token", tokenForm(code, cid)))
	w := postForm(mux, "/token", refreshForm(first["refresh_token"].(string), cid))
	if w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	second = jsonBody(t, w)
	for _, m := range []map[string]any{first, second} {
		tokens = append(tokens, m["access_token"].(string), m["refresh_token"].(string))
	}
	return tokens, cid, first["refresh_token"].(string), second
}

func TestFreshStoreHoldsNoPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	srv, mux, closeSrv := openServerAt(t, path)
	tokens, cid, oldRefresh, second := runFullFlow(t, mux)
	if v := userVersion(t, path); v != oauthSchemaVersion {
		t.Fatalf("fresh store user_version = %d", v)
	}

	replay := jsonBody(t, postForm(mux, "/token", refreshForm(oldRefresh, cid)))
	if replay["access_token"] != second["access_token"] || replay["refresh_token"] != second["refresh_token"] {
		t.Fatal("replay must return the identical pair")
	}
	// Revoke and delete rows too: secure_delete must scrub them.
	postForm(mux, "/revoke", url.Values{"token": {second["refresh_token"].(string)}, "client_id": {cid}})
	if _, err := srv.verifyToken(nil, second["access_token"].(string), nil); err == nil {
		t.Error("revoked access token still valid")
	}
	closeSrv()
	assertNoPlaintext(t, path, tokens...)
}

func TestReplayUnsealFailures(t *testing.T) {
	flip := func(b []byte) []byte { b = append([]byte(nil), b...); b[len(b)-1] ^= 0x01; return b }
	tests := []struct {
		name   string
		mutate func(sealed []byte) []byte
	}{
		{"tampered ciphertext", flip},
		{"tampered nonce", func(b []byte) []byte { b = append([]byte(nil), b...); b[0] ^= 0xff; return b }},
		{"truncated", func(b []byte) []byte { return b[:5] }},
		{"sealed under a different token", func([]byte) []byte {
			b, err := sealReplacement("some-other-refresh-token", "a", "r")
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
	}
	for _, mode := range []string{"memory", "sqlite"} {
		for _, tc := range tests {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var path string
				var srv *OAuthServer
				var mux http.Handler
				var closeSrv func()
				if mode == "sqlite" {
					path = filepath.Join(t.TempDir(), "oauth.db")
					srv, mux, closeSrv = openServerAt(t, path)
				} else {
					srv, mux = setupTestServer(t)
				}
				cid := registerTestClient(t, mux.(*http.ServeMux))
				_, refresh := issueTestTokenPair(t, srv, cid)
				if w := postForm(mux, "/token", refreshForm(refresh, cid)); w.Code != 200 {
					t.Fatalf("rotate: %d", w.Code)
				}
				entry, ok := srv.tokens.GetRefreshToken(refresh)
				if !ok || len(entry.ReplacementSealed) == 0 {
					t.Fatal("rotated entry should carry a sealed replacement")
				}
				bad := tc.mutate(entry.ReplacementSealed)
				if mode == "sqlite" {
					// Tamper durably and read it back through a fresh server.
					closeSrv()
					db, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec("UPDATE oauth_refresh_tokens SET replacement_sealed = ? WHERE token_hash = ?", bad, hashToken(refresh)); err != nil {
						t.Fatal(err)
					}
					db.Close()
					srv, mux, _ = openServerAt(t, path)
				} else {
					entry.ReplacementSealed = bad
				}
				_ = srv
				w := postForm(mux, "/token", refreshForm(refresh, cid))
				if w.Code != 400 || jsonBody(t, w)["error"] != "invalid_grant" {
					t.Fatalf("got %d %s, want 400 invalid_grant", w.Code, w.Body.String())
				}
			})
		}
	}

	t.Run("store level error", func(t *testing.T) {
		tokens := newTokenStore()
		now := time.Now()
		tokens.PutRefreshToken("old", "c", now.Add(time.Hour))
		if _, err := tokens.RotateRefreshToken("old", "c", now, time.Minute, func() (*TokenPair, error) {
			return &TokenPair{AccessToken: "a", RefreshToken: "r", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(time.Hour)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		rt, _ := tokens.GetRefreshToken("old")
		rt.ReplacementSealed = flip(rt.ReplacementSealed)
		_, err := tokens.RotateRefreshToken("old", "c", now, time.Minute, nil)
		if !errors.Is(err, errReplayUnseal) {
			t.Fatalf("got %v, want errReplayUnseal", err)
		}
	})
}

func TestSealRoundTrip(t *testing.T) {
	sealed, err := sealReplacement("old-token", "access-x", "refresh-y")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("access-x")) || bytes.Contains(sealed, []byte("refresh-y")) {
		t.Fatal("sealed blob contains plaintext")
	}
	a, r, err := unsealReplacement("old-token", sealed)
	if err != nil || a != "access-x" || r != "refresh-y" {
		t.Fatalf("round trip: %q %q %v", a, r, err)
	}
	if _, _, err := unsealReplacement("other-token", sealed); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestRestartKeepsSessionsAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	_, mux, closeSrv := openServerAt(t, path)
	_, cid, oldRefresh, second := runFullFlow(t, mux)
	closeSrv()

	srv2, mux2, _ := openServerAt(t, path)
	if _, err := srv2.verifyToken(nil, second["access_token"].(string), nil); err != nil {
		t.Fatalf("access token lost across restart: %v", err)
	}
	replay := jsonBody(t, postForm(mux2, "/token", refreshForm(oldRefresh, cid)))
	if replay["access_token"] != second["access_token"] || replay["refresh_token"] != second["refresh_token"] {
		t.Fatal("replay after restart must return the identical pair")
	}
	if w := postForm(mux2, "/token", refreshForm(second["refresh_token"].(string), cid)); w.Code != 200 {
		t.Fatalf("rotate after restart: %d", w.Code)
	}
}

func TestInMemoryReplay(t *testing.T) {
	_, mux := setupTestServer(t)
	_, cid, oldRefresh, second := runFullFlow(t, mux)
	replay := jsonBody(t, postForm(mux, "/token", refreshForm(oldRefresh, cid)))
	if replay["access_token"] != second["access_token"] || replay["refresh_token"] != second["refresh_token"] {
		t.Fatal("in-memory replay must return the identical pair")
	}
}

func TestConcurrentRefreshSamePair(t *testing.T) {
	for _, mode := range []string{"memory", "sqlite"} {
		t.Run(mode, func(t *testing.T) {
			var srv *OAuthServer
			var mux http.Handler
			if mode == "sqlite" {
				srv, mux, _ = openServerAt(t, filepath.Join(t.TempDir(), "oauth.db"))
			} else {
				srv, mux = setupTestServer(t)
			}
			cid := registerTestClient(t, mux.(*http.ServeMux))
			_, refresh := issueTestTokenPair(t, srv, cid)

			const n = 32
			type result struct {
				code          int
				access, refre string
			}
			results := make([]result, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					w := postForm(mux, "/token", refreshForm(refresh, cid))
					r := result{code: w.Code}
					if w.Code == 200 {
						m := jsonBody(t, w)
						r.access, r.refre = m["access_token"].(string), m["refresh_token"].(string)
					}
					results[i] = r
				}()
			}
			close(start)
			wg.Wait()
			for i, r := range results {
				if r.code != 200 {
					t.Fatalf("request %d: status %d", i, r.code)
				}
				if r.access != results[0].access || r.refre != results[0].refre {
					t.Fatalf("request %d received a different pair", i)
				}
			}
		})
	}
}

func TestTokenPersistFailureIsReported(t *testing.T) {
	srv, err := NewOAuthServerWithError(Config{Username: "a", Password: "b", StorePath: filepath.Join(t.TempDir(), "oauth.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	if err := srv.store.close(); err != nil {
		t.Fatal(err)
	}

	if err := srv.tokens.PutAccessToken("tok", "c", time.Now().Add(time.Hour), ""); err == nil {
		t.Error("PutAccessToken should fail on a closed store")
	}
	if err := srv.tokens.PutRefreshToken("tok", "c", time.Now().Add(time.Hour)); err == nil {
		t.Error("PutRefreshToken should fail on a closed store")
	}
	if len(srv.tokens.accessTokens) != 0 || len(srv.tokens.refreshTokens) != 0 {
		t.Error("tokens that failed to persist must not be usable")
	}

	w := httptest.NewRecorder()
	srv.issueTokenPair(w, "c")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "server_error") {
		t.Errorf("got %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "access_token") {
		t.Errorf("tokens handed out after a failed write: %s", w.Body.String())
	}
}

// revokingStore revokes the token right after the first read, which is when a
// concurrent revocation could overtake a cache fill.
type revokingStore struct {
	persistentTokenStore
	revoke func()
	once   sync.Once
}

func (r *revokingStore) getAccessToken(h string) (*TokenEntry, bool) {
	t, ok := r.persistentTokenStore.getAccessToken(h)
	r.once.Do(r.revoke)
	return t, ok
}

func (r *revokingStore) getRefreshToken(h string) (*RefreshTokenEntry, bool) {
	t, ok := r.persistentTokenStore.getRefreshToken(h)
	r.once.Do(r.revoke)
	return t, ok
}

func TestRevocationDuringCacheFillSticks(t *testing.T) {
	newStore := func(t *testing.T) *persistentStore {
		t.Helper()
		store, err := openPersistentStore(filepath.Join(t.TempDir(), "oauth.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		return store
	}
	exp := time.Now().Add(time.Hour)

	t.Run("access", func(t *testing.T) {
		store := newStore(t)
		if err := store.putAccessToken(&TokenEntry{TokenHash: hashToken("tok"), ClientID: "c", ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
		tokens := newTokenStore()
		tokens.persist = &revokingStore{persistentTokenStore: store, revoke: func() { tokens.DeleteAccessToken("tok") }}
		if _, ok := tokens.GetAccessToken("tok"); ok {
			t.Error("revoked token returned")
		}
		if _, ok := tokens.GetAccessToken("tok"); ok {
			t.Error("revoked token came back from the cache")
		}
	})
	t.Run("refresh", func(t *testing.T) {
		store := newStore(t)
		if err := store.putRefreshToken(&RefreshTokenEntry{TokenHash: hashToken("tok"), ClientID: "c", ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
		tokens := newTokenStore()
		tokens.persist = &revokingStore{persistentTokenStore: store, revoke: func() { tokens.DeleteRefreshToken("tok") }}
		if _, ok := tokens.GetRefreshToken("tok"); ok {
			t.Error("revoked token returned")
		}
		if _, ok := tokens.GetRefreshToken("tok"); ok {
			t.Error("revoked token came back from the cache")
		}
	})
}

// flakyRevokeStore fails revocation writes on demand while reads keep working,
// as happens with a full disk or a held write lock.
type flakyRevokeStore struct {
	persistentTokenStore
	fail atomic.Bool
}

func (f *flakyRevokeStore) revokeTokens(accessHashes, refreshHashes []string) error {
	if f.fail.Load() {
		return errors.New("disk full")
	}
	return f.persistentTokenStore.revokeTokens(accessHashes, refreshHashes)
}

func setupFlakyRevoke(t *testing.T) (*OAuthServer, *http.ServeMux, *flakyRevokeStore) {
	t.Helper()
	srv, err := NewOAuthServerWithError(Config{Username: "a", Password: "b", StorePath: filepath.Join(t.TempDir(), "oauth.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	flaky := &flakyRevokeStore{persistentTokenStore: srv.store}
	srv.tokens.persist = flaky
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, func(next http.Handler) http.Handler { return next })
	return srv, mux, flaky
}

func postRevoke(mux *http.ServeMux, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/revoke", strings.NewReader(url.Values{"token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestRevokeRefreshReportsStoreFailure(t *testing.T) {
	srv, mux, flaky := setupFlakyRevoke(t)
	access, refresh := issueTestTokenPair(t, srv, "c")
	flaky.fail.Store(true)

	w := postRevoke(mux, refresh)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d %q, want 503 with Retry-After", w.Code, w.Body.String())
	}

	// The rows are still there, yet nothing may bring the tokens back.
	if _, ok := srv.store.getRefreshToken(hashToken(refresh)); !ok {
		t.Fatal("test needs the refresh row to remain in the store")
	}
	if _, ok := srv.store.getAccessToken(hashToken(access)); !ok {
		t.Fatal("test needs the access row to remain in the store")
	}
	if _, ok := srv.tokens.GetAccessToken(access); ok {
		t.Error("access token came back from the store")
	}
	if _, ok := srv.tokens.GetRefreshToken(refresh); ok {
		t.Error("refresh token came back from the store")
	}
	if _, err := srv.verifyToken(t.Context(), access, nil); err == nil {
		t.Error("revoked access token still authenticates")
	}
	if _, err := srv.tokens.RotateRefreshToken(refresh, "c", time.Now(), time.Hour, newTokenPair); !errors.Is(err, errInvalidRefreshToken) {
		t.Errorf("rotating a revoked token: err = %v", err)
	}

	// A retry must not report success while the store still fails.
	if w := postRevoke(mux, refresh); w.Code != http.StatusServiceUnavailable {
		t.Errorf("retry during the outage: got %d", w.Code)
	}

	flaky.fail.Store(false)
	if w := postRevoke(mux, refresh); w.Code != http.StatusOK {
		t.Fatalf("retry after the outage: got %d %s", w.Code, w.Body.String())
	}
	if _, ok := srv.store.getRefreshToken(hashToken(refresh)); ok {
		t.Error("refresh row left behind")
	}
	if _, ok := srv.store.getAccessToken(hashToken(access)); ok {
		t.Error("access row left behind")
	}
	if len(srv.tokens.pendingAccess)+len(srv.tokens.pendingRefresh) != 0 {
		t.Error("pending revocations not cleared")
	}
}

func TestRevokeAccessReportsStoreFailure(t *testing.T) {
	srv, mux, flaky := setupFlakyRevoke(t)
	access, refresh := issueTestTokenPair(t, srv, "c")
	flaky.fail.Store(true)

	req := httptest.NewRequest(http.MethodPost, "/revoke", strings.NewReader(url.Values{"token": {access}, "token_type_hint": {"access_token"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", w.Code)
	}
	if _, ok := srv.tokens.GetAccessToken(access); ok {
		t.Error("access token came back from the store")
	}
	if _, ok := srv.tokens.GetRefreshToken(refresh); !ok {
		t.Error("revoking an access token must leave its refresh token alone")
	}
}

func TestRevokeOfOtherTokenWithoutPendingSucceeds(t *testing.T) {
	_, mux, flaky := setupFlakyRevoke(t)
	flaky.fail.Store(true)
	if w := postRevoke(mux, "unknown"); w.Code != http.StatusOK {
		t.Errorf("unknown token with nothing pending: got %d, want 200 (RFC 7009)", w.Code)
	}
}

func TestCleanupRetriesPendingRevocations(t *testing.T) {
	srv, mux, flaky := setupFlakyRevoke(t)
	access, refresh := issueTestTokenPair(t, srv, "c")
	flaky.fail.Store(true)
	postRevoke(mux, refresh)

	srv.tokens.cleanup()
	if len(srv.tokens.pendingRefresh) == 0 {
		t.Fatal("pending revocation dropped while the store still fails")
	}
	if _, ok := srv.tokens.GetAccessToken(access); ok {
		t.Error("access token came back after a failed retry")
	}

	flaky.fail.Store(false)
	srv.tokens.cleanup()
	if len(srv.tokens.pendingRefresh) != 0 {
		t.Error("pending revocation not cleared")
	}
	if _, ok := srv.store.getRefreshToken(hashToken(refresh)); ok {
		t.Error("refresh row left behind")
	}
}

func TestRevokeChainIsAllOrNothing(t *testing.T) {
	store, err := openPersistentStore(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	tokens := newTokenStore()
	tokens.persist = store

	exp := time.Now().Add(time.Hour)
	if err := tokens.PutRefreshToken("r0", "c", exp); err != nil {
		t.Fatal(err)
	}
	var pairs []*TokenPair
	cur := "r0"
	for range 3 {
		pair, err := tokens.RotateRefreshToken(cur, "c", time.Now(), time.Hour, newTokenPair)
		if err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, pair)
		cur = pair.RefreshToken
	}

	// Refuse to delete the last member. Whatever order the chain is written in,
	// the rows deleted before that point must be rolled back.
	last := hashToken(cur)
	if _, err := store.db.Exec(`CREATE TRIGGER block_delete BEFORE DELETE ON oauth_refresh_tokens
		WHEN old.token_hash = '` + last + `' BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := tokens.RevokeRefreshToken("r0"); err == nil {
		t.Fatal("revocation should fail")
	}
	for _, h := range []string{hashToken("r0"), hashToken(pairs[0].RefreshToken), hashToken(pairs[1].RefreshToken), last} {
		if _, ok := store.getRefreshToken(h); !ok {
			t.Errorf("refresh row %s deleted by a failed revocation", h[:8])
		}
	}
	for _, p := range pairs {
		if _, ok := store.getAccessToken(hashToken(p.AccessToken)); !ok {
			t.Errorf("access row deleted by a failed revocation")
		}
		if _, ok := tokens.GetAccessToken(p.AccessToken); ok {
			t.Errorf("access token usable after a failed revocation")
		}
		if _, ok := tokens.GetRefreshToken(p.RefreshToken); ok {
			t.Errorf("refresh token usable after a failed revocation")
		}
	}

	if _, err := store.db.Exec(`DROP TRIGGER block_delete`); err != nil {
		t.Fatal(err)
	}
	if err := tokens.RetryPendingRevocations(); err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		if _, ok := store.getAccessToken(hashToken(p.AccessToken)); ok {
			t.Errorf("access row left behind after retry")
		}
		if _, ok := store.getRefreshToken(hashToken(p.RefreshToken)); ok {
			t.Errorf("refresh row left behind after retry")
		}
	}
}
