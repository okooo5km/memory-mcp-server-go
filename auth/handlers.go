// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// timeNow is a variable for testing time-dependent logic.
var timeNow = time.Now

// Token lifetimes.
const (
	authCodeTTL     = 10 * time.Minute
	accessTokenTTL  = 1 * time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour // 30 days
)

// --- 1. Protected Resource Metadata (RFC 9728) ---

func (s *OAuthServer) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	issuer := s.resolveIssuer(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              issuer,
		"authorization_servers": []string{issuer},
		"resource_name":         "Memory MCP Server",
	})
}

// --- 2. Authorization Server Metadata (RFC 8414) ---

func (s *OAuthServer) handleAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	issuer := s.resolveIssuer(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"registration_endpoint":                 issuer + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"mcp:read"},
	})
}

// --- 3. Dynamic Client Registration (RFC 7591) ---

func (s *OAuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ClientName    string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "invalid JSON body"})
		return
	}

	if len(req.RedirectURIs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "redirect_uris is required"})
		return
	}

	// Defaults
	if len(req.GrantTypes) == 0 {
		req.GrantTypes = []string{"authorization_code"}
	}
	if len(req.ResponseTypes) == 0 {
		req.ResponseTypes = []string{"code"}
	}

	clientID, err := generateToken(16) // 32 hex chars
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	client := &OAuthClient{
		ClientID:      clientID,
		ClientName:    req.ClientName,
		RedirectURIs:  req.RedirectURIs,
		GrantTypes:    req.GrantTypes,
		ResponseTypes: req.ResponseTypes,
		CreatedAt:     timeNow(),
	}
	s.clients.Put(client)

	writeJSON(w, http.StatusCreated, client)
}

// --- 4 & 5. Authorization Endpoint ---

func (s *OAuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAuthorizeGET(w, r)
	case http.MethodPost:
		s.handleAuthorizePOST(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleAuthorizeGET renders the login page.
func (s *OAuthServer) handleAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	if clientID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "client_id is required"})
		return
	}
	if _, ok := s.clients.Get(clientID); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "unknown client_id"})
		return
	}

	data := loginPageData{
		ClientID:            clientID,
		RedirectURI:         q.Get("redirect_uri"),
		State:               q.Get("state"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		ResponseType:        q.Get("response_type"),
		Scope:               q.Get("scope"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	loginTmpl.Execute(w, data)
}

// handleAuthorizePOST processes the login form submission.
func (s *OAuthServer) handleAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}

	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	state := r.FormValue("state")
	codeChallenge := r.FormValue("code_challenge")
	codeChallengeMethod := r.FormValue("code_challenge_method")
	username := r.FormValue("username")
	password := r.FormValue("password")

	// Validate client
	if _, ok := s.clients.Get(clientID); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "unknown client_id"})
		return
	}

	// Validate credentials using constant-time comparison
	usernameMatch := subtle.ConstantTimeCompare([]byte(username), []byte(s.config.Username)) == 1
	passwordMatch := subtle.ConstantTimeCompare([]byte(password), []byte(s.config.Password)) == 1
	if !usernameMatch || !passwordMatch {
		// Re-render login page with error
		data := loginPageData{
			ClientID:            clientID,
			RedirectURI:         redirectURI,
			State:               state,
			CodeChallenge:       codeChallenge,
			CodeChallengeMethod: codeChallengeMethod,
			ResponseType:        r.FormValue("response_type"),
			Scope:               r.FormValue("scope"),
			Error:               "用户名或密码错误",
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		loginTmpl.Execute(w, data)
		return
	}

	// Generate authorization code
	code, err := generateToken(32) // 64 hex chars
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	s.codes.Put(&AuthorizationCode{
		Code:                code,
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		State:               state,
		ExpiresAt:           timeNow().Add(authCodeTTL),
	})

	// Redirect back with code and state
	redirectURL, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "Invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := redirectURL.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	redirectURL.RawQuery = q.Encode()
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

// --- 6. Token Endpoint ---

func (s *OAuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}

	grantType := r.FormValue("grant_type")
	switch grantType {
	case "authorization_code":
		s.handleTokenAuthorizationCode(w, r)
	case "refresh_token":
		s.handleTokenRefresh(w, r)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (s *OAuthServer) handleTokenAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	codeVerifier := r.FormValue("code_verifier")
	clientID := r.FormValue("client_id")

	// Validate code
	ac, ok := s.codes.Get(code)
	if !ok || ac.Used || timeNow().After(ac.ExpiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "invalid or expired authorization code"})
		return
	}

	// Validate client_id matches
	if ac.ClientID != clientID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "client_id mismatch"})
		return
	}

	// Validate PKCE
	if ac.CodeChallenge != "" {
		if codeVerifier == "" || !verifyPKCE(codeVerifier, ac.CodeChallenge) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "PKCE verification failed"})
			return
		}
	}

	// Mark code as used
	s.codes.MarkUsed(code)

	// Issue tokens
	s.issueTokenPair(w, clientID)
}

func (s *OAuthServer) handleTokenRefresh(w http.ResponseWriter, r *http.Request) {
	refreshToken := r.FormValue("refresh_token")
	clientID := r.FormValue("client_id")

	rt, ok := s.tokens.GetRefreshToken(refreshToken)
	if !ok || timeNow().After(rt.ExpiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "invalid or expired refresh token"})
		return
	}

	if rt.ClientID != clientID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "client_id mismatch"})
		return
	}

	// Revoke old refresh token (rotation)
	s.tokens.DeleteRefreshToken(refreshToken)

	// Issue new token pair
	s.issueTokenPair(w, clientID)
}

// issueTokenPair generates and returns a new access+refresh token pair.
func (s *OAuthServer) issueTokenPair(w http.ResponseWriter, clientID string) {
	accessToken, err := generateToken(32)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	refreshToken, err := generateToken(32)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	now := timeNow()
	s.tokens.PutAccessToken(&TokenEntry{
		Token:        accessToken,
		ClientID:     clientID,
		ExpiresAt:    now.Add(accessTokenTTL),
		RefreshToken: refreshToken,
	})
	s.tokens.PutRefreshToken(&RefreshTokenEntry{
		Token:     refreshToken,
		ClientID:  clientID,
		ExpiresAt: now.Add(refreshTokenTTL),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refreshToken,
	})
}

// --- Helpers ---

type loginPageData struct {
	ClientID            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	ResponseType        string
	Scope               string
	Error               string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
