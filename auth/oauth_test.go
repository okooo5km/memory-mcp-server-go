// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setupTestServer(t *testing.T) (*OAuthServer, *http.ServeMux) {
	t.Helper()
	srv := NewOAuthServer(Config{
		Username: "admin",
		Password: "secret",
	})
	t.Cleanup(srv.Close)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, func(h http.Handler) http.Handler { return h })
	return srv, mux
}

func TestGenerateToken(t *testing.T) {
	tok, err := generateToken(32)
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if len(tok) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("expected 64 chars, got %d", len(tok))
	}
	// Uniqueness
	tok2, _ := generateToken(32)
	if tok == tok2 {
		t.Error("two tokens should not be equal")
	}
}

func TestVerifyPKCE(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	if !verifyPKCE(verifier, challenge) {
		t.Error("PKCE should verify with correct verifier")
	}
	if verifyPKCE("wrong-verifier", challenge) {
		t.Error("PKCE should fail with wrong verifier")
	}
}

func TestProtectedResourceMetadata(t *testing.T) {
	_, mux := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	req.Host = "mcp.example.com"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	if body["resource"] != "http://mcp.example.com/mcp" {
		t.Errorf("unexpected resource: %v", body["resource"])
	}
}

func TestAuthorizationServerMetadata(t *testing.T) {
	_, mux := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	req.Host = "mcp.example.com"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	if body["issuer"] != "http://mcp.example.com" {
		t.Errorf("unexpected issuer: %v", body["issuer"])
	}
	methods, ok := body["code_challenge_methods_supported"].([]any)
	if !ok || len(methods) == 0 || methods[0] != "S256" {
		t.Error("S256 must be in code_challenge_methods_supported")
	}
}

func TestDynamicClientRegistration(t *testing.T) {
	_, mux := setupTestServer(t)
	body := `{"client_name":"test","redirect_uris":["http://localhost/cb"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["client_id"] == nil || resp["client_id"] == "" {
		t.Error("client_id should be set")
	}
}

func TestRegisterMissingRedirectURIs(t *testing.T) {
	_, mux := setupTestServer(t)
	body := `{"client_name":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestMiddleware401(t *testing.T) {
	srv, _ := setupTestServer(t)
	handler := srv.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No token
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Host = "mcp.example.com"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	authHeader := w.Header().Get("WWW-Authenticate")
	if !strings.Contains(authHeader, "resource_metadata=") {
		t.Errorf("WWW-Authenticate should contain resource_metadata, got: %s", authHeader)
	}
}

func TestFullOAuthFlow(t *testing.T) {
	srv, mux := setupTestServer(t)

	// Step 1: Register client
	regBody := `{"client_name":"Claude","redirect_uris":["https://claude.ai/cb"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(regBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d", w.Code)
	}
	var regResp map[string]any
	json.NewDecoder(w.Body).Decode(&regResp)
	clientID := regResp["client_id"].(string)

	// Step 2: PKCE challenge
	codeVerifier := "test-code-verifier-with-enough-entropy-for-testing-1234567890"
	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	// Step 3: GET /authorize (login page)
	authURL := "/authorize?response_type=code&client_id=" + clientID +
		"&redirect_uri=" + url.QueryEscape("https://claude.ai/cb") +
		"&code_challenge=" + codeChallenge +
		"&code_challenge_method=S256&state=test-state"
	req = httptest.NewRequest(http.MethodGet, authURL, nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authorize GET: expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("expected HTML, got %s", ct)
	}

	// Step 4: POST /authorize with wrong credentials
	form := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"https://claude.ai/cb"},
		"state":                 {"test-state"},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"username":              {"wrong"},
		"password":              {"wrong"},
	}
	req = httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authorize POST (bad creds): expected 200 (re-render), got %d", w.Code)
	}
	bodyBytes, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(bodyBytes), "Invalid username or password") {
		t.Error("expected error message in login page")
	}

	// Step 5: POST /authorize with correct credentials
	form.Set("username", "admin")
	form.Set("password", "secret")
	req = httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authorize POST (good creds): expected 200, got %d: %s", w.Code, w.Body.String())
	}
	successBody := w.Body.String()
	if !strings.Contains(successBody, "Authorization Successful") {
		t.Error("expected success page")
	}
	// Extract code from the redirect URL embedded in the success page
	codeIdx := strings.Index(successBody, "code=")
	if codeIdx == -1 {
		t.Fatal("code should be in success page redirect URL")
	}
	// Parse the callback URL from the page to extract code
	// The URL is in a JS string, find it between quotes after code=
	codeStr := successBody[codeIdx+5:]
	if ampIdx := strings.IndexAny(codeStr, "&\"\\"); ampIdx > 0 {
		codeStr = codeStr[:ampIdx]
	}
	code := codeStr
	if code == "" {
		t.Fatal("code should not be empty")
	}
	if !strings.Contains(successBody, "state=test-state") {
		t.Error("state should be preserved in redirect URL")
	}

	// Step 6: POST /token (authorization_code grant)
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {codeVerifier},
	}
	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(tokenForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tokenResp map[string]any
	json.NewDecoder(w.Body).Decode(&tokenResp)
	accessToken := tokenResp["access_token"].(string)
	refreshToken := tokenResp["refresh_token"].(string)
	if accessToken == "" || refreshToken == "" {
		t.Fatal("access_token and refresh_token must be set")
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Error("token_type should be Bearer")
	}

	// Step 7: Use access token with middleware
	handler := srv.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	req = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated request: expected 200, got %d", w.Code)
	}

	// Step 8: Use code again (should fail - already used)
	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(tokenForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("reuse code: expected 400, got %d", w.Code)
	}

	// Step 9: Refresh token
	refreshForm := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(refreshForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var refreshResp map[string]any
	json.NewDecoder(w.Body).Decode(&refreshResp)
	newAccessToken := refreshResp["access_token"].(string)
	newRefreshToken := refreshResp["refresh_token"].(string)
	if newAccessToken == "" || newRefreshToken == "" {
		t.Fatal("new tokens must be set after refresh")
	}
	if newRefreshToken == refreshToken {
		t.Error("refresh token should be rotated")
	}

	// Step 10: Immediate old refresh token replay should be tolerated for
	// concurrent clients sharing the same token cache.
	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(refreshForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("immediate refresh replay: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var replayResp map[string]any
	json.NewDecoder(w.Body).Decode(&replayResp)
	if replayResp["access_token"] != newAccessToken || replayResp["refresh_token"] != newRefreshToken {
		t.Fatal("refresh replay should return the same replacement token pair")
	}

	// Step 11: New access token works
	req = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+newAccessToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("new token: expected 200, got %d", w.Code)
	}
}

func TestRefreshReplayExpiresAfterGrace(t *testing.T) {
	srv, mux := setupTestServer(t)
	srv.grace = time.Second
	clientID := registerTestClient(t, mux)

	refreshToken, _ := generateToken(32)
	srv.tokens.PutRefreshToken(refreshToken, clientID, timeNow().Add(refreshTokenTTL))

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	orig := timeNow
	timeNow = func() time.Time { return orig().Add(2 * time.Second) }
	defer func() { timeNow = orig }()

	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("late refresh replay: expected 400, got %d", w.Code)
	}
}

func TestDefaultRefreshReplayGraceCoversStaleWorker(t *testing.T) {
	tokens := newTokenStore()
	clientID := "codex-client"
	refreshToken := "refresh-old"
	now := time.Date(2026, 5, 27, 7, 4, 48, 0, time.UTC)
	tokens.PutRefreshToken(refreshToken, clientID, now.Add(refreshTokenTTL))

	pair, err := tokens.RotateRefreshToken(refreshToken, clientID, now, defaultRefreshReplayGrace, func() (*TokenPair, error) {
		return &TokenPair{
			AccessToken:      "access-new",
			RefreshToken:     "refresh-new",
			AccessExpiresAt:  now.Add(accessTokenTTL),
			RefreshExpiresAt: now.Add(refreshTokenTTL),
		}, nil
	})
	if err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	replayAt := now.Add(accessTokenTTL - time.Minute)
	replay, err := tokens.RotateRefreshToken(refreshToken, clientID, replayAt, defaultRefreshReplayGrace, func() (*TokenPair, error) {
		t.Fatal("replay should not issue a new token pair")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("stale worker refresh replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("expected replayed token pair")
	}
	if replay.AccessToken != pair.AccessToken || replay.RefreshToken != pair.RefreshToken {
		t.Fatal("refresh replay should return the same replacement token pair")
	}

	_, err = tokens.RotateRefreshToken(refreshToken, clientID, now.Add(defaultRefreshReplayGrace+time.Second), defaultRefreshReplayGrace, func() (*TokenPair, error) {
		return nil, nil
	})
	if !errors.Is(err, errInvalidRefreshToken) {
		t.Fatalf("late refresh replay: expected invalid refresh token, got %v", err)
	}
}

func TestPersistentOAuthStoreSurvivesRestart(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "oauth.db")
	srv, err := NewOAuthServerWithError(Config{
		Username:  "admin",
		Password:  "secret",
		StorePath: storePath,
	})
	if err != nil {
		t.Fatalf("new persistent oauth server: %v", err)
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, func(h http.Handler) http.Handler { return h })

	clientID := registerTestClient(t, mux)
	accessToken, refreshToken := issueTestTokenPair(t, srv, clientID)
	srv.Close()

	srv2, err := NewOAuthServerWithError(Config{
		Username:  "admin",
		Password:  "secret",
		StorePath: storePath,
	})
	if err != nil {
		t.Fatalf("restart persistent oauth server: %v", err)
	}
	defer srv2.Close()

	if _, ok := srv2.clients.Get(clientID); !ok {
		t.Fatal("registered OAuth client should survive restart")
	}
	handler := srv2.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("persisted access token: expected 200, got %d", w.Code)
	}

	mux2 := http.NewServeMux()
	srv2.RegisterRoutes(mux2, func(h http.Handler) http.Handler { return h })
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux2.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("persisted refresh token: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPersistentOAuthStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not portable on Windows")
	}
	storePath := filepath.Join(t.TempDir(), "oauth.db")
	srv, err := NewOAuthServerWithError(Config{
		Username:  "admin",
		Password:  "secret",
		StorePath: storePath,
	})
	if err != nil {
		t.Fatalf("new persistent oauth server: %v", err)
	}
	defer srv.Close()

	info, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("stat oauth store: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("oauth store permissions: expected 0600, got %03o", got)
	}
}

func TestExpiredAccessToken(t *testing.T) {
	srv, mux := setupTestServer(t)

	// Register + authorize + get token
	clientID := registerTestClient(t, mux)
	accessToken := issueTestToken(t, srv, clientID)

	// Simulate token expiry
	orig := timeNow
	timeNow = func() time.Time { return time.Now().Add(2 * time.Hour) }
	defer func() { timeNow = orig }()

	handler := srv.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: expected 401, got %d", w.Code)
	}
}

func TestIssuerFromConfig(t *testing.T) {
	srv := NewOAuthServer(Config{
		Username: "admin",
		Password: "secret",
		Issuer:   "https://my.server.com",
	})
	defer srv.Close()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, func(h http.Handler) http.Handler { return h })

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	if body["resource"] != "https://my.server.com/mcp" {
		t.Errorf("expected issuer from config, got: %v", body["resource"])
	}
}

// --- helpers ---

func registerTestClient(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	body := `{"client_name":"test","redirect_uris":["http://localhost/cb"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	return resp["client_id"].(string)
}

func issueTestToken(t *testing.T, srv *OAuthServer, clientID string) string {
	t.Helper()
	tok, _ := generateToken(32)
	srv.tokens.PutAccessToken(tok, clientID, time.Now().Add(accessTokenTTL), "")
	return tok
}

func issueTestTokenPair(t *testing.T, srv *OAuthServer, clientID string) (string, string) {
	t.Helper()
	accessToken, _ := generateToken(32)
	refreshToken, _ := generateToken(32)
	srv.tokens.PutAccessToken(accessToken, clientID, timeNow().Add(accessTokenTTL), refreshToken)
	srv.tokens.PutRefreshToken(refreshToken, clientID, timeNow().Add(refreshTokenTTL))
	return accessToken, refreshToken
}

// --- spec-conformance tests ---

func pkcePair(verifier string) (string, string) {
	h := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(h[:])
}

const testVerifier = "test-code-verifier-with-enough-entropy-for-testing-1234567890"

func registerClientURIs(t *testing.T, mux http.Handler, uris ...string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"client_name": "Test App", "redirect_uris": uris})
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	return resp["client_id"].(string)
}

func authzQuery(clientID, redirect string, extra map[string]string) url.Values {
	_, challenge := pkcePair(testVerifier)
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"st4te"},
	}
	for k, val := range extra {
		if val == "" {
			v.Del(k)
		} else {
			v.Set(k, val)
		}
	}
	return v
}

func getAuthorize(mux http.Handler, v url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+v.Encode(), nil)
	req.Host = "mcp.example.com"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func postAuthorize(mux http.Handler, v url.Values, user, pass string) *httptest.ResponseRecorder {
	f := url.Values{}
	for k, vals := range v {
		f[k] = vals
	}
	f.Set("username", user)
	f.Set("password", pass)
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(f.Encode()))
	req.Host = "mcp.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

var codeRe = regexp.MustCompile(`code=([0-9a-f]{64})`)

// authorizeCode logs in and returns the authorization code.
func authorizeCode(t *testing.T, mux http.Handler, v url.Values) string {
	t.Helper()
	w := postAuthorize(mux, v, "admin", "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("authorize POST: %d %s", w.Code, w.Body.String())
	}
	m := codeRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("no code in success page: %s", w.Body.String())
	}
	return m[1]
}

func postForm(mux http.Handler, path string, f url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(f.Encode()))
	req.Host = "mcp.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func tokenForm(code, clientID string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {testVerifier},
	}
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %q", w.Body.String())
	}
	return m
}

func TestAuthorizeRedirectURIValidation(t *testing.T) {
	_, mux := setupTestServer(t)
	exact := registerClientURIs(t, mux, "https://app.example.com/cb")
	loop := registerClientURIs(t, mux, "http://127.0.0.1/cb")
	two := registerClientURIs(t, mux, "https://a.example.com/cb", "https://b.example.com/cb")

	tests := []struct {
		name     string
		client   string
		redirect string
		want     int
	}{
		{"exact match", exact, "https://app.example.com/cb", 200},
		{"attacker host", exact, "https://evil.example.com/cb", 400},
		{"path variance", exact, "https://app.example.com/cb2", 400},
		{"query added", exact, "https://app.example.com/cb?x=1", 400},
		{"scheme downgrade", exact, "http://app.example.com/cb", 400},
		{"javascript", exact, "javascript:alert(1)", 400},
		{"empty single registered", exact, "", 200},
		{"empty with two registered", two, "", 400},
		{"loopback port variance", loop, "http://127.0.0.1:54321/cb", 200},
		{"loopback path variance", loop, "http://127.0.0.1:54321/other", 400},
		{"loopback host variance", loop, "http://localhost:54321/cb", 400},
		{"loopback https", loop, "https://127.0.0.1:54321/cb", 400},
		{"non-loopback port variance", exact, "https://app.example.com:8443/cb", 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := authzQuery(tc.client, tc.redirect, map[string]string{"redirect_uri": tc.redirect})
			for _, w := range []*httptest.ResponseRecorder{
				getAuthorize(mux, v),
				postAuthorize(mux, v, "admin", "secret"),
			} {
				if w.Code != tc.want {
					t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
				}
				if tc.want == 400 {
					if loc := w.Header().Get("Location"); loc != "" {
						t.Fatalf("must never redirect on bad redirect_uri, got Location %q", loc)
					}
					if jsonBody(t, w)["error"] != "invalid_request" {
						t.Fatalf("unexpected body: %s", w.Body.String())
					}
				}
			}
		})
	}

	t.Run("unknown client never redirects", func(t *testing.T) {
		w := getAuthorize(mux, authzQuery("nope", "https://app.example.com/cb", nil))
		if w.Code != 400 || w.Header().Get("Location") != "" {
			t.Fatalf("got %d loc=%q", w.Code, w.Header().Get("Location"))
		}
	})
}

func TestAuthorizeErrorRedirects(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	tests := []struct {
		name  string
		extra map[string]string
		code  string
	}{
		{"missing PKCE", map[string]string{"code_challenge": "", "code_challenge_method": ""}, "invalid_request"},
		{"missing method", map[string]string{"code_challenge_method": ""}, "invalid_request"},
		{"method plain", map[string]string{"code_challenge_method": "plain"}, "invalid_request"},
		{"short challenge", map[string]string{"code_challenge": "abc"}, "invalid_request"},
		{"bad response_type", map[string]string{"response_type": "token"}, "unsupported_response_type"},
		{"missing response_type", map[string]string{"response_type": ""}, "unsupported_response_type"},
		{"bad resource", map[string]string{"resource": "https://other.example.com/mcp"}, "invalid_target"},
		{"bad resource path", map[string]string{"resource": "http://mcp.example.com/other"}, "invalid_target"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := authzQuery(cid, "https://app.example.com/cb", tc.extra)
			for name, w := range map[string]*httptest.ResponseRecorder{
				"GET":  getAuthorize(mux, v),
				"POST": postAuthorize(mux, v, "admin", "secret"),
			} {
				if w.Code != http.StatusFound {
					t.Fatalf("%s: status %d, want 302: %s", name, w.Code, w.Body.String())
				}
				loc, err := url.Parse(w.Header().Get("Location"))
				if err != nil || loc.Host != "app.example.com" || loc.Path != "/cb" {
					t.Fatalf("%s: bad Location %q", name, w.Header().Get("Location"))
				}
				q := loc.Query()
				if q.Get("error") != tc.code || q.Get("state") != "st4te" || q.Get("iss") != "http://mcp.example.com" || q.Get("error_description") == "" {
					t.Fatalf("%s: unexpected redirect params %v", name, q)
				}
				if q.Get("code") != "" {
					t.Fatalf("%s: must not issue a code", name)
				}
			}
		})
	}
}

func TestAuthorizeResourceAccepted(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	for _, res := range []string{"", "http://mcp.example.com/mcp", "http://mcp.example.com/mcp/", "http://mcp.example.com"} {
		v := authzQuery(cid, "https://app.example.com/cb", map[string]string{"resource": res})
		if w := getAuthorize(mux, v); w.Code != 200 {
			t.Errorf("resource %q: status %d", res, w.Code)
		}
	}
}

func TestSuccessRedirectHasIss(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	w := postAuthorize(mux, authzQuery(cid, "https://app.example.com/cb", nil), "admin", "secret")
	body := w.Body.String()
	if !strings.Contains(body, "iss=http") || !strings.Contains(body, "state=st4te") {
		t.Fatalf("success page missing iss/state: %s", body)
	}
}

func TestTokenEndpointRequirements(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	other := registerClientURIs(t, mux, "https://other.example.com/cb")
	redirect := "https://app.example.com/cb"

	tests := []struct {
		name    string
		mutate  func(f url.Values)
		wantErr string
		status  int
	}{
		{"ok", func(f url.Values) {}, "", 200},
		{"ok with matching redirect and resource", func(f url.Values) {
			f.Set("redirect_uri", redirect)
			f.Set("resource", "http://mcp.example.com/mcp")
		}, "", 200},
		{"wrong verifier", func(f url.Values) { f.Set("code_verifier", strings.Repeat("a", 50)) }, "invalid_grant", 400},
		{"missing verifier", func(f url.Values) { f.Del("code_verifier") }, "invalid_grant", 400},
		{"redirect mismatch", func(f url.Values) { f.Set("redirect_uri", "https://app.example.com/other") }, "invalid_grant", 400},
		{"client mismatch", func(f url.Values) { f.Set("client_id", other) }, "invalid_grant", 400},
		{"resource not ours", func(f url.Values) { f.Set("resource", "https://other.example.com/mcp") }, "invalid_target", 400},
		{"resource differs from authorize", func(f url.Values) { f.Set("resource", "http://mcp.example.com") }, "invalid_target", 400},
		{"unknown code", func(f url.Values) { f.Set("code", strings.Repeat("0", 64)) }, "invalid_grant", 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code := authorizeCode(t, mux, authzQuery(cid, redirect, map[string]string{"resource": "http://mcp.example.com/mcp"}))
			f := tokenForm(code, cid)
			tc.mutate(f)
			w := postForm(mux, "/token", f)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
			}
			body := jsonBody(t, w)
			if tc.wantErr != "" {
				if body["error"] != tc.wantErr {
					t.Fatalf("error %v, want %s", body["error"], tc.wantErr)
				}
			} else if body["scope"] != "mcp" || body["token_type"] != "Bearer" {
				t.Fatalf("unexpected token response %v", body)
			}
		})
	}

	t.Run("refresh rejects foreign resource", func(t *testing.T) {
		code := authorizeCode(t, mux, authzQuery(cid, redirect, nil))
		tok := jsonBody(t, postForm(mux, "/token", tokenForm(code, cid)))
		w := postForm(mux, "/token", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)},
			"client_id": {cid}, "resource": {"https://other.example.com"},
		})
		if w.Code != 400 || jsonBody(t, w)["error"] != "invalid_target" {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
		w = postForm(mux, "/token", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)},
			"client_id": {cid}, "resource": {"http://mcp.example.com/mcp"},
		})
		if w.Code != 200 || jsonBody(t, w)["scope"] != "mcp" {
			t.Fatalf("refresh with valid resource: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("unsupported grant type uses RFC shape", func(t *testing.T) {
		w := postForm(mux, "/token", url.Values{"grant_type": {"password"}})
		if w.Code != 400 || jsonBody(t, w)["error"] != "unsupported_grant_type" {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
	})
}

func TestCodeConsumeIsAtomic(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	for round := 0; round < 5; round++ {
		code := authorizeCode(t, mux, authzQuery(cid, "https://app.example.com/cb", nil))
		const n = 32
		var ok, bad int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				w := postForm(mux, "/token", tokenForm(code, cid))
				switch w.Code {
				case 200:
					atomic.AddInt32(&ok, 1)
				case 400:
					atomic.AddInt32(&bad, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if ok != 1 || bad != n-1 {
			t.Fatalf("round %d: %d successes, %d rejections; want exactly 1 and %d", round, ok, bad, n-1)
		}
	}
}

func TestRegisterRedirectURIValidation(t *testing.T) {
	_, mux := setupTestServer(t)
	tests := []struct {
		uri  string
		want int
	}{
		{"https://app.example.com/cb", 201},
		{"http://localhost:8080/cb", 201},
		{"http://127.0.0.1/cb", 201},
		{"http://[::1]:9/cb", 201},
		{"com.example.app:/oauth", 201},
		{"cursor://anysphere.cursor-retrieval/oauth", 201},
		{"javascript:alert(1)", 400},
		{"JavaScript:alert(1)", 400},
		{"data:text/html,x", 400},
		{"vbscript:x", 400},
		{"file:///etc/passwd", 400},
		{"about:blank", 400},
		{"blob:https://x/y", 400},
		{"http://example.com/cb", 400},
		{"https://app.example.com/cb#frag", 400},
		{"https:///nohost", 400},
		{"raycast://oauth/callback", 201},
		{"/relative", 400},
		{"", 400},
	}
	for _, tc := range tests {
		t.Run(tc.uri, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"redirect_uris": []string{tc.uri}})
			req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(string(b)))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			body := jsonBody(t, w)
			if tc.want == 400 && body["error"] != "invalid_redirect_uri" {
				t.Fatalf("error = %v", body["error"])
			}
			if tc.want == 201 {
				if body["token_endpoint_auth_method"] != "none" || body["client_id_issued_at"] == nil || body["client_secret"] != nil {
					t.Fatalf("unexpected registration response %v", body)
				}
			}
		})
	}
}

func TestRegisterRejectsConfidentialAuthMethod(t *testing.T) {
	_, mux := setupTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"redirect_uris":["https://a.example.com/cb"],"token_endpoint_auth_method":"client_secret_basic"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 400 || jsonBody(t, w)["error"] != "invalid_client_metadata" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestBodySizeLimits(t *testing.T) {
	_, mux := setupTestServer(t)
	big := strings.Repeat("a", 100<<10)
	tests := []struct{ path, ctype, body string }{
		{"/register", "application/json", `{"client_name":"` + big + `","redirect_uris":["https://a.example.com/cb"]}`},
		{"/token", "application/x-www-form-urlencoded", "grant_type=authorization_code&code=" + big},
		{"/revoke", "application/x-www-form-urlencoded", "token=" + big},
		{"/authorize", "application/x-www-form-urlencoded", "client_id=x&password=" + big},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ctype)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 413/400", w.Code)
			}
		})
	}
}

func TestPathSuffixedProtectedResourceMetadata(t *testing.T) {
	srv := NewOAuthServer(Config{Username: "a", Password: "b", Issuer: "https://mcp.example.com/", ResourcePath: "memory/"})
	defer srv.Close()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, func(h http.Handler) http.Handler { return h })

	get := func(path string) string {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	root := get("/.well-known/oauth-protected-resource")
	suffixed := get("/.well-known/oauth-protected-resource/memory")
	if root != suffixed {
		t.Fatalf("root and path-suffixed metadata differ:\n%s\n%s", root, suffixed)
	}
	var body map[string]any
	json.Unmarshal([]byte(root), &body)
	if body["resource"] != "https://mcp.example.com/memory" {
		t.Errorf("resource = %v", body["resource"])
	}
	if as, _ := body["authorization_servers"].([]any); len(as) != 1 || as[0] != "https://mcp.example.com" {
		t.Errorf("authorization_servers = %v", body["authorization_servers"])
	}
	if sc, _ := body["scopes_supported"].([]any); len(sc) != 1 || sc[0] != "mcp" {
		t.Errorf("scopes_supported = %v", body["scopes_supported"])
	}
	if bm, _ := body["bearer_methods_supported"].([]any); len(bm) != 1 || bm[0] != "header" {
		t.Errorf("bearer_methods_supported = %v", body["bearer_methods_supported"])
	}
	if body["resource_name"] == nil {
		t.Error("resource_name missing")
	}
}

func TestAuthorizationServerMetadataSpecFields(t *testing.T) {
	_, mux := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	req.Host = "mcp.example.com"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := jsonBody(t, w)
	if body["revocation_endpoint"] != "http://mcp.example.com/revoke" ||
		body["client_id_metadata_document_supported"] != true ||
		body["authorization_response_iss_parameter_supported"] != true {
		t.Errorf("missing spec fields: %v", body)
	}
	for _, k := range []string{"scopes_supported", "token_endpoint_auth_methods_supported", "revocation_endpoint_auth_methods_supported"} {
		arr, _ := body[k].([]any)
		if k == "scopes_supported" && (len(arr) != 1 || arr[0] != "mcp") {
			t.Errorf("%s = %v", k, arr)
		}
		if k != "scopes_supported" && (len(arr) != 1 || arr[0] != "none") {
			t.Errorf("%s = %v", k, arr)
		}
	}
}

func TestWWWAuthenticateHeader(t *testing.T) {
	srv, _ := setupTestServer(t)
	handler := srv.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Host = "mcp.example.com"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	h := w.Header().Get("WWW-Authenticate")
	if !strings.Contains(h, `resource_metadata="http://mcp.example.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Errorf("missing path-suffixed resource_metadata: %s", h)
	}
	if !strings.Contains(h, `scope="mcp"`) {
		t.Errorf("missing scope: %s", h)
	}
}

func TestMiddlewareRejectsGarbageAndAcceptsValid(t *testing.T) {
	srv, mux := setupTestServer(t)
	cid := registerTestClient(t, mux)
	good := issueTestToken(t, srv, cid)
	handler := srv.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for auth, want := range map[string]int{"Bearer " + good: 200, "Bearer nope": 401, "Basic abc": 401, "": 401} {
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("Authorization %q: %d, want %d", auth, w.Code, want)
		}
	}
}

func TestRevocation(t *testing.T) {
	srv, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	other := registerClientURIs(t, mux, "https://other.example.com/cb")

	valid := func(tok string) bool {
		_, err := srv.verifyToken(nil, tok, nil)
		return err == nil
	}
	revoke := func(f url.Values) int { return postForm(mux, "/revoke", f).Code }

	t.Run("refresh revokes linked access tokens", func(t *testing.T) {
		access, refresh := issueTestTokenPair(t, srv, cid)
		if c := revoke(url.Values{"token": {refresh}, "client_id": {cid}}); c != 200 {
			t.Fatalf("status %d", c)
		}
		if valid(access) {
			t.Error("linked access token should be revoked")
		}
		if _, ok := srv.tokens.GetRefreshToken(refresh); ok {
			t.Error("refresh token should be revoked")
		}
	})
	t.Run("access token revocation leaves refresh", func(t *testing.T) {
		access, refresh := issueTestTokenPair(t, srv, cid)
		revoke(url.Values{"token": {access}, "token_type_hint": {"access_token"}, "client_id": {cid}})
		if valid(access) {
			t.Error("access token should be revoked")
		}
		if _, ok := srv.tokens.GetRefreshToken(refresh); !ok {
			t.Error("refresh token should survive")
		}
	})
	t.Run("other client ignored", func(t *testing.T) {
		access, refresh := issueTestTokenPair(t, srv, cid)
		if c := revoke(url.Values{"token": {refresh}, "client_id": {other}}); c != 200 {
			t.Fatalf("status %d", c)
		}
		if c := revoke(url.Values{"token": {access}, "client_id": {other}}); c != 200 {
			t.Fatalf("status %d", c)
		}
		if !valid(access) {
			t.Error("access token of another client must not be revoked")
		}
		if _, ok := srv.tokens.GetRefreshToken(refresh); !ok {
			t.Error("refresh token of another client must not be revoked")
		}
	})
	t.Run("wrong hint still works", func(t *testing.T) {
		access, _ := issueTestTokenPair(t, srv, cid)
		revoke(url.Values{"token": {access}, "token_type_hint": {"refresh_token"}})
		if valid(access) {
			t.Error("hint is only advisory")
		}
	})
	t.Run("unknown token is 200, missing token is 400", func(t *testing.T) {
		if c := revoke(url.Values{"token": {"nonexistent"}}); c != 200 {
			t.Errorf("unknown: %d", c)
		}
		if c := revoke(url.Values{}); c != 400 {
			t.Errorf("missing: %d", c)
		}
	})
	t.Run("rotation chain", func(t *testing.T) {
		code := authorizeCode(t, mux, authzQuery(cid, "https://app.example.com/cb", nil))
		first := jsonBody(t, postForm(mux, "/token", tokenForm(code, cid)))
		second := jsonBody(t, postForm(mux, "/token", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {first["refresh_token"].(string)}, "client_id": {cid}}))
		revoke(url.Values{"token": {second["refresh_token"].(string)}, "client_id": {cid}})
		for _, tok := range []string{first["access_token"].(string), second["access_token"].(string)} {
			if valid(tok) {
				t.Error("every access token in the chain should be revoked")
			}
		}
		w := postForm(mux, "/token", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {first["refresh_token"].(string)}, "client_id": {cid}})
		if w.Code != 400 {
			t.Errorf("old refresh token must not replay after revocation, got %d", w.Code)
		}
	})
}

func TestRevocationPersistentStore(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "oauth.db")
	srv, err := NewOAuthServerWithError(Config{Username: "admin", Password: "secret", StorePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, func(h http.Handler) http.Handler { return h })
	cid := registerTestClient(t, mux)
	access, refresh := issueTestTokenPair(t, srv, cid)
	srv.Close()

	// Fresh server: tokens exist only in SQLite.
	srv2, err := NewOAuthServerWithError(Config{Username: "admin", Password: "secret", StorePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	mux2 := http.NewServeMux()
	srv2.RegisterRoutes(mux2, func(h http.Handler) http.Handler { return h })
	if c := postForm(mux2, "/revoke", url.Values{"token": {refresh}, "client_id": {cid}}).Code; c != 200 {
		t.Fatalf("revoke: %d", c)
	}
	if _, ok := srv2.store.getAccessToken(hashToken(access)); ok {
		t.Error("persisted access token should be deleted")
	}
	if _, ok := srv2.store.getRefreshToken(hashToken(refresh)); ok {
		t.Error("persisted refresh token should be deleted")
	}
	if _, err := srv2.verifyToken(nil, access, nil); err == nil {
		t.Error("revoked token must not verify")
	}
}

func TestPersistentTokensWithoutResourceStillWork(t *testing.T) {
	// Tokens persisted by older versions carry no resource info.
	srv, _ := setupTestServer(t)
	tok := issueTestToken(t, srv, "legacy-client")
	info, err := srv.verifyToken(nil, tok, nil)
	if err != nil || info.UserID != "admin" || info.Extra["client_id"] != "legacy-client" || len(info.Scopes) != 1 || info.Scopes[0] != "mcp" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestValidatePublicURL(t *testing.T) {
	tests := []struct {
		in, out string
		ok      bool
	}{
		{"https://mcp.example.com", "https://mcp.example.com", true},
		{"https://mcp.example.com/", "https://mcp.example.com", true},
		{"http://localhost:8080", "http://localhost:8080", true},
		{"ftp://example.com", "", false},
		{"example.com", "", false},
		{"https://", "", false},
		{"https://example.com/path", "", false},
		{"https://example.com?x=1", "", false},
		{"https://example.com#frag", "", false},
		{"https://user:pw@example.com", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, err := ValidatePublicURL(tc.in)
		if (err == nil) != tc.ok || got != tc.out {
			t.Errorf("ValidatePublicURL(%q) = %q, %v", tc.in, got, err)
		}
	}
	if _, err := NewOAuthServerWithError(Config{Username: "a", Password: "b", Issuer: "https://x.example.com/path"}); err == nil {
		t.Error("invalid issuer must be rejected")
	}
}

func TestHTMLSecurityHeaders(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	v := authzQuery(cid, "https://app.example.com/cb", nil)
	nonceRe := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`)

	var nonces []string
	for name, w := range map[string]*httptest.ResponseRecorder{
		"login":   getAuthorize(mux, v),
		"success": postAuthorize(mux, v, "admin", "secret"),
	} {
		h := w.Header()
		for k, want := range map[string]string{
			"Cache-Control":          "no-store",
			"X-Frame-Options":        "DENY",
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "no-referrer",
		} {
			if h.Get(k) != want {
				t.Errorf("%s: %s = %q", name, k, h.Get(k))
			}
		}
		csp := h.Get("Content-Security-Policy")
		m := nonceRe.FindStringSubmatch(csp)
		if m == nil || !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "form-action 'self'") || !strings.Contains(csp, "base-uri 'none'") || !strings.Contains(csp, "style-src 'unsafe-inline'") {
			t.Errorf("%s: bad CSP %q", name, csp)
			continue
		}
		nonces = append(nonces, m[1])
		if name == "success" && !strings.Contains(w.Body.String(), `<script nonce="`+m[1]+`">`) {
			t.Errorf("success page script lacks matching nonce")
		}
	}
	if len(nonces) == 2 && nonces[0] == nonces[1] {
		t.Error("nonce must differ per response")
	}
}

func TestLoginPageShowsClientAndHidesNothingImportant(t *testing.T) {
	srv, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	v := authzQuery(cid, "https://app.example.com/cb", map[string]string{"resource": "http://mcp.example.com/mcp/"})
	body := getAuthorize(mux, v).Body.String()
	for _, want := range []string{"Test App", "app.example.com", `name="resource" value="http://mcp.example.com/mcp"`, "name declared by the application"} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
	// resource must survive the login round trip into the code
	code := authorizeCode(t, mux, v)
	if ac, ok := srv.codes.Get(code); !ok || ac.Resource != "http://mcp.example.com/mcp" {
		t.Errorf("resource not remembered on code: %+v", ac)
	}
}

func TestLoginThrottling(t *testing.T) {
	srv, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	v := authzQuery(cid, "https://app.example.com/cb", nil)

	now := time.Now()
	orig := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = orig }()

	for i := 0; i < 4; i++ {
		if w := postAuthorize(mux, v, "admin", "bad"); w.Code != 200 || !strings.Contains(w.Body.String(), "Invalid username or password") {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	// A success clears the key.
	if w := postAuthorize(mux, v, "admin", "secret"); w.Code != 200 || !strings.Contains(w.Body.String(), "Authorization Successful") {
		t.Fatalf("login after 4 failures: %d", w.Code)
	}
	for i := 0; i < 5; i++ {
		postAuthorize(mux, v, "admin", "bad")
	}
	w := postAuthorize(mux, v, "admin", "secret") // even the right password is refused while blocked
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	ra, err := time.ParseDuration(w.Header().Get("Retry-After") + "s")
	if err != nil || ra <= 0 || ra > loginFailureWindow {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	if !strings.Contains(w.Body.String(), "Too many failed attempts") {
		t.Error("429 should render the login page error")
	}

	// A different client IP is unaffected (per-key limit).
	f := v
	f.Set("username", "admin")
	f.Set("password", "secret")
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.7:4444"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("other IP: %d", rec.Code)
	}

	// Window expiry unblocks.
	now = now.Add(loginFailureWindow + time.Second)
	if w := postAuthorize(mux, v, "admin", "secret"); w.Code != 200 {
		t.Fatalf("after window: %d", w.Code)
	}
	_ = srv
}

func TestGlobalLoginThrottle(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	v := authzQuery(cid, "https://app.example.com/cb", nil)
	post := func(ip string) int {
		f := url.Values{}
		for k, vals := range v {
			f[k] = vals
		}
		f.Set("username", "admin")
		f.Set("password", "bad")
		req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = ip + ":1"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	for i := 0; i < loginFailuresGlobal; i++ {
		post("10.0." + strconv.Itoa(i/200) + "." + strconv.Itoa(i%200+1))
	}
	// Each IP failed once (far below the per-key limit); the global counter alone must now block everyone.
	if c := post("198.51.100.200"); c != http.StatusTooManyRequests {
		t.Fatalf("expected global 429, got %d", c)
	}
}

func TestRegistrationThrottling(t *testing.T) {
	_, mux := setupTestServer(t)
	now := time.Now()
	orig := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = orig }()

	reg := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"redirect_uris":["https://a.example.com/cb"]}`))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < registrationsPerKey; i++ {
		if w := reg(); w.Code != 201 {
			t.Fatalf("registration %d: %d", i, w.Code)
		}
	}
	w := reg()
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	now = now.Add(registrationWindow + time.Second)
	if w := reg(); w.Code != 201 {
		t.Fatalf("after window: %d", w.Code)
	}
}

func TestClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies("172.20.0.0/16, 10.1.2.3, fd00::/8")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		remote  string
		xri     string
		trusted []netip.Prefix
		want    string
	}{
		{"loopback v4 peer honours header", "127.0.0.1:5000", "203.0.113.9", nil, "203.0.113.9"},
		{"loopback v6 peer honours header", "[::1]:5000", "203.0.113.9", nil, "203.0.113.9"},
		{"untrusted peer ignores spoofed header", "198.51.100.1:5000", "203.0.113.9", nil, "198.51.100.1"},
		{"untrusted peer with list ignores header", "198.51.100.1:5000", "203.0.113.9", trusted, "198.51.100.1"},
		{"trusted CIDR peer honours header", "172.20.0.5:5000", "203.0.113.9", trusted, "203.0.113.9"},
		{"trusted bare IP peer honours header", "10.1.2.3:5000", "203.0.113.9", trusted, "203.0.113.9"},
		{"neighbour of bare IP is not trusted", "10.1.2.4:5000", "203.0.113.9", trusted, "10.1.2.4"},
		{"trusted v6 peer honours v6 header", "[fd00::5]:5000", "2001:db8::1", trusted, "2001:db8::1"},
		{"trusted peer without header uses peer", "172.20.0.5:5000", "", trusted, "172.20.0.5"},
		{"invalid header falls back to loopback peer", "127.0.0.1:5000", "garbage", nil, "127.0.0.1"},
		{"invalid header falls back to trusted peer", "172.20.0.5:5000", "not-an-ip", trusted, "172.20.0.5"},
		{"list header is invalid", "172.20.0.5:5000", "203.0.113.9, 198.51.100.1", trusted, "172.20.0.5"},
		{"header is trimmed", "172.20.0.5:5000", "  203.0.113.9 ", trusted, "203.0.113.9"},
		{"mapped header is unmapped", "172.20.0.5:5000", "::ffff:203.0.113.9", trusted, "203.0.113.9"},
		{"mapped peer matches v4 prefix", "[::ffff:172.20.0.5]:5000", "203.0.113.9", trusted, "203.0.113.9"},
		{"mapped loopback peer is loopback", "[::ffff:127.0.0.1]:5000", "203.0.113.9", nil, "203.0.113.9"},
		{"mapped untrusted peer is unmapped key", "[::ffff:198.51.100.1]:5000", "203.0.113.9", trusted, "198.51.100.1"},
		{"peer without port", "172.20.0.5", "203.0.113.9", trusted, "203.0.113.9"},
		{"unparsable peer is returned as is", "pipe", "203.0.113.9", trusted, "pipe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remote
			if c.xri != "" {
				r.Header.Set("X-Real-IP", c.xri)
			}
			r.Header.Set("X-Forwarded-For", "192.0.2.77") // must never be used
			if got := clientIP(r, c.trusted); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	ok := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"10.0.0.1", []string{"10.0.0.1/32"}},
		{"10.0.0.0/8", []string{"10.0.0.0/8"}},
		{"10.1.2.3/8", []string{"10.0.0.0/8"}},
		{" 172.20.0.0/16 , ::1 ,fd00::/8", []string{"172.20.0.0/16", "::1/128", "fd00::/8"}},
		{"::ffff:10.0.0.1", []string{"10.0.0.1/32"}},
		{"::ffff:10.0.0.0/104", []string{"10.0.0.0/8"}},
	}
	for _, c := range ok {
		got, err := ParseTrustedProxies(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		var s []string
		for _, p := range got {
			s = append(s, p.String())
		}
		if strings.Join(s, " ") != strings.Join(c.want, " ") {
			t.Errorf("%q = %v, want %v", c.in, s, c.want)
		}
	}
	for _, in := range []string{"nope", "10.0.0.0/33", "10.0.0.1,", ",10.0.0.1", "10.0.0.1,,10.0.0.2", "10.0.0.1/", "fe80::1%eth0", "::ffff:0:0/80", "example.com"} {
		if _, err := ParseTrustedProxies(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestLoginLimiterUsesTrustedProxyHeader(t *testing.T) {
	trusted, _ := ParseTrustedProxies("172.20.0.0/16")
	s := NewOAuthServer(Config{Username: "u", Password: "p", TrustedProxies: trusted})
	defer s.Close()
	mk := func(remote, xri string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Real-IP", xri)
		return r
	}
	if got := s.clientIP(mk("172.20.0.2:1", "203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("trusted proxy: %s", got)
	}
	if got := s.clientIP(mk("172.21.0.2:1", "203.0.113.7")); got != "172.21.0.2" {
		t.Errorf("untrusted proxy: %s", got)
	}
}

func TestRateLimiterBoundedGrowth(t *testing.T) {
	l := newRateLimiter(1, time.Hour)
	l.maxKeys = 10
	for i := 0; i < 100; i++ {
		l.record(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	if l.size() > 10 {
		t.Fatalf("limiter grew to %d keys", l.size())
	}
}

func TestCrossOriginPOSTRejected(t *testing.T) {
	_, mux := setupTestServer(t)
	cid := registerClientURIs(t, mux, "https://app.example.com/cb")
	v := authzQuery(cid, "https://app.example.com/cb", nil)
	v.Set("username", "admin")
	v.Set("password", "secret")
	for header, want := range map[string]int{"": 200, "same-origin": 200, "cross-site": 403, "same-site": 403} {
		req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if header != "" {
			req.Header.Set("Sec-Fetch-Site", header)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("Sec-Fetch-Site %q: %d, want %d", header, w.Code, want)
		}
	}
}

func TestSecureEqual(t *testing.T) {
	if !secureEqual("abc", "abc") || secureEqual("abc", "abcd") || secureEqual("", "x") || !secureEqual("", "") {
		t.Error("secureEqual misbehaves")
	}
}

// --- CIMD ---

func newCIMDTestServer(t *testing.T, handler http.HandlerFunc) (*OAuthServer, http.Handler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	srv, mux := setupTestServer(t)
	srv.cimdControl = nil // tests talk to 127.0.0.1
	srv.cimdTLS = &tls.Config{InsecureSkipVerify: true}
	return srv, mux, ts
}

func TestCIMDHappyPath(t *testing.T) {
	var hits int32
	var clientID string
	srv, mux, ts := newCIMDTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		json.NewEncoder(w).Encode(map[string]any{
			"client_id": clientID, "client_name": "CIMD App",
			"redirect_uris":              []string{"https://cimd.example.com/cb"},
			"token_endpoint_auth_method": "none",
		})
	})
	clientID = ts.URL + "/client.json"
	v := authzQuery(clientID, "https://cimd.example.com/cb", nil)

	w := getAuthorize(mux, v)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "CIMD App") {
		t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	code := authorizeCode(t, mux, v)
	tw := postForm(mux, "/token", tokenForm(code, clientID))
	if tw.Code != 200 {
		t.Fatalf("token: %d %s", tw.Code, tw.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("metadata fetched %d times, want 1 (cached)", n)
	}
	// Cache expiry triggers a refetch.
	orig := timeNow
	timeNow = func() time.Time { return orig().Add(cimdCacheTTL + time.Second) }
	defer func() { timeNow = orig }()
	getAuthorize(mux, v)
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("after TTL fetched %d times, want 2", n)
	}
	_ = srv
}

func TestCIMDRejections(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   func(id string) string
	}{
		{"client_id mismatch", 200, func(id string) string {
			return `{"client_id":"https://evil.example.com/x","redirect_uris":["https://a.example.com/cb"]}`
		}},
		{"not found", 404, func(id string) string { return `{}` }},
		{"invalid json", 200, func(id string) string { return `not json` }},
		{"confidential auth method", 200, func(id string) string {
			return `{"client_id":"` + id + `","redirect_uris":["https://a.example.com/cb"],"token_endpoint_auth_method":"client_secret_post"}`
		}},
		{"client_secret present", 200, func(id string) string {
			return `{"client_id":"` + id + `","redirect_uris":["https://a.example.com/cb"],"client_secret":"x"}`
		}},
		{"javascript redirect", 200, func(id string) string {
			return `{"client_id":"` + id + `","redirect_uris":["javascript:alert(1)"]}`
		}},
		{"no redirect uris", 200, func(id string) string { return `{"client_id":"` + id + `"}` }},
		{"too large", 200, func(id string) string {
			return `{"client_id":"` + id + `","redirect_uris":["https://a.example.com/cb"],"pad":"` + strings.Repeat("a", 70<<10) + `"}`
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var clientID string
			_, mux, ts := newCIMDTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body(clientID))
			})
			clientID = ts.URL + "/client.json"
			w := getAuthorize(mux, authzQuery(clientID, "https://a.example.com/cb", nil))
			if w.Code != 400 || w.Header().Get("Location") != "" {
				t.Fatalf("got %d loc=%q: %s", w.Code, w.Header().Get("Location"), w.Body.String())
			}
		})
	}
}

func TestCIMDDoesNotFollowRedirects(t *testing.T) {
	var clientID string
	_, mux, ts := newCIMDTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final.json" {
			json.NewEncoder(w).Encode(map[string]any{"client_id": clientID, "redirect_uris": []string{"https://a.example.com/cb"}})
			return
		}
		http.Redirect(w, r, "/final.json", http.StatusFound)
	})
	clientID = ts.URL + "/client.json"
	if w := getAuthorize(mux, authzQuery(clientID, "https://a.example.com/cb", nil)); w.Code != 400 {
		t.Fatalf("redirect must not be followed, got %d", w.Code)
	}
}

func TestCIMDSSRFGuardRefusesLoopback(t *testing.T) {
	var reached int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reached, 1)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "x", "redirect_uris": []string{"https://a.example.com/cb"}})
	}))
	defer ts.Close()
	srv, mux := setupTestServer(t) // real guard (default cimdControl)
	srv.cimdTLS = &tls.Config{InsecureSkipVerify: true}

	clientID := ts.URL + "/client.json"
	w := getAuthorize(mux, authzQuery(clientID, "https://a.example.com/cb", nil))
	if w.Code != 400 || w.Header().Get("Location") != "" {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "blocked address") {
		t.Errorf("expected guard refusal, got %s", w.Body.String())
	}
	if atomic.LoadInt32(&reached) != 0 {
		t.Fatal("request reached the loopback server")
	}
}

func TestIPBlocked(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.0.0.1", "172.16.5.4", "192.168.1.1", "169.254.169.254", "fe80::1",
		"224.0.0.1", "ff02::1", "0.0.0.0", "::", "100.64.0.1", "100.127.255.255", "fd00::1", "fc00::1", "::ffff:127.0.0.1", "::ffff:10.1.1.1", "0.1.2.3"}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "100.63.255.255", "100.128.0.1", "2606:4700:4700::1111"}
	for _, s := range blocked {
		if !ipBlocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range allowed {
		if ipBlocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestIsCIMDClientID(t *testing.T) {
	tests := map[string]bool{
		"https://app.example.com/client.json": true,
		"https://app.example.com/":            false,
		"https://app.example.com":             false,
		"http://app.example.com/client.json":  false,
		"https://app.example.com/a#f":         false,
		"https://u:p@app.example.com/a":       false,
		"https://app.example.com/a/../b":      false,
		"abcdef0123456789":                    false,
	}
	for in, want := range tests {
		if got := isCIMDClientID(in); got != want {
			t.Errorf("isCIMDClientID(%q) = %v", in, got)
		}
	}
}

func TestCIMDCacheBounded(t *testing.T) {
	var c cimdCache
	for i := 0; i < cimdCacheLimit*2; i++ {
		c.put("https://x.example.com/"+strings.Repeat("a", i%50)+string(rune('A'+i%26))+string(rune('a'+i/26%26))+string(rune('0'+i/676)), &OAuthClient{})
	}
	if len(c.entries) > cimdCacheLimit {
		t.Fatalf("cache grew to %d", len(c.entries))
	}
}
