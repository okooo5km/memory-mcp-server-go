// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	if body["resource"] != "http://mcp.example.com" {
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

	// Step 10: Old refresh token should be revoked
	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(refreshForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("reuse refresh: expected 400, got %d", w.Code)
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
	if body["resource"] != "https://my.server.com" {
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
	srv.tokens.PutAccessToken(&TokenEntry{
		Token:     tok,
		ClientID:  clientID,
		ExpiresAt: time.Now().Add(accessTokenTTL),
	})
	return tok
}
