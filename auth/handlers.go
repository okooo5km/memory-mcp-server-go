// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
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
	writeJSON(w, http.StatusOK, &oauthex.ProtectedResourceMetadata{
		Resource:               s.resourceURI(issuer),
		AuthorizationServers:   []string{issuer},
		ScopesSupported:        []string{scopeMCP},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Memory MCP Server",
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
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + "/authorize",
		"token_endpoint":                                 issuer + "/token",
		"registration_endpoint":                          issuer + "/register",
		"revocation_endpoint":                            issuer + "/revoke",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"scopes_supported":                               []string{scopeMCP},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// --- 3. Dynamic Client Registration (RFC 7591) ---
//
// DCR is deprecated by the MCP spec in favour of Client ID Metadata Documents
// but is kept because current clients still use it.

func (s *OAuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if ok, wait := s.regLimiter.take(s.clientIP(r)); !ok {
		writeRateLimited(w, wait)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)

	var req struct {
		ClientName    string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if isBodyTooLarge(err) {
			writeOAuthError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "request body too large")
			return
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON body")
		return
	}

	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	if len(req.RedirectURIs) > maxRedirectURIs {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "too many redirect_uris")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}

	// Defaults
	if len(req.GrantTypes) == 0 {
		req.GrantTypes = []string{"authorization_code"}
	}
	if len(req.ResponseTypes) == 0 {
		req.ResponseTypes = []string{"code"}
	}
	// RFC 7591 section 3.2.1: the server may replace requested metadata, and the
	// response tells the client what it got. Unsupported values are dropped, and
	// the auth method is always "none", so that clients which ask for more than
	// a public client can have still register. Refuse only when nothing usable
	// is left.
	req.GrantTypes = keepSupported(req.GrantTypes, "authorization_code", "refresh_token")
	if len(req.GrantTypes) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "grant_types must include authorization_code or refresh_token")
		return
	}
	req.ResponseTypes = keepSupported(req.ResponseTypes, "code")
	if len(req.ResponseTypes) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "response_types must include code")
		return
	}

	clientID, err := generateToken(16) // 32 hex chars
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}

	client := &OAuthClient{
		ClientID:      clientID,
		ClientName:    cleanDisplayName(req.ClientName),
		RedirectURIs:  req.RedirectURIs,
		GrantTypes:    req.GrantTypes,
		ResponseTypes: req.ResponseTypes,
		CreatedAt:     timeNow(),
	}
	if err := s.clients.Put(client); err != nil {
		log.Printf("OAuth client registration failed to persist: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to store client registration")
		return
	}

	resp := map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                client.GrantTypes,
		"response_types":             client.ResponseTypes,
		"token_endpoint_auth_method": "none",
	}
	if client.ClientName != "" {
		resp["client_name"] = client.ClientName
	}
	setNoStore(w)
	writeJSON(w, http.StatusCreated, resp)
}

// keepSupported returns the values that appear in supported, in their original order.
func keepSupported(values []string, supported ...string) []string {
	return slices.DeleteFunc(slices.Clone(values), func(v string) bool {
		return !slices.Contains(supported, v)
	})
}

// --- 4 & 5. Authorization Endpoint ---

func (s *OAuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAuthorizeGET(w, r)
	case http.MethodPost:
		// Reject cross-site browser POSTs. Requests carrying neither Origin nor
		// Sec-Fetch-Site (curl, tests, non-browser flows) are allowed through.
		s.cop.Handler(http.HandlerFunc(s.handleAuthorizePOST)).ServeHTTP(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// authzRequest is a fully validated authorization request.
type authzRequest struct {
	Client      *OAuthClient
	RedirectURI string
	State       string
	Challenge   string
	Method      string
	Resource    string
}

// authzFailure describes a rejected authorization request. When RedirectURI is
// empty the error must be shown directly (never redirected), because the
// client or redirect_uri could not be trusted.
type authzFailure struct {
	RedirectURI string
	State       string
	Code        string
	Description string
}

// validateAuthorize checks an authorization request. Client and redirect_uri
// are validated first; only after both pass are errors reported by redirect.
func (s *OAuthServer) validateAuthorize(ctx context.Context, issuer string, v url.Values) (*authzRequest, *authzFailure) {
	clientID := v.Get("client_id")
	if clientID == "" {
		return nil, &authzFailure{Code: "invalid_request", Description: "client_id is required"}
	}
	client, err := s.resolveClient(ctx, clientID)
	if err != nil {
		desc := "unknown client_id"
		if !errors.Is(err, errUnknownClient) {
			desc = "invalid client: " + err.Error()
		}
		return nil, &authzFailure{Code: "invalid_request", Description: desc}
	}
	redirectURI, ok := matchRedirectURI(client.RedirectURIs, v.Get("redirect_uri"))
	if !ok {
		return nil, &authzFailure{Code: "invalid_request", Description: "redirect_uri does not match a registered redirect URI"}
	}

	state := v.Get("state")
	fail := func(code, desc string) (*authzRequest, *authzFailure) {
		return nil, &authzFailure{RedirectURI: redirectURI, State: state, Code: code, Description: desc}
	}
	if v.Get("response_type") != "code" {
		return fail("unsupported_response_type", "response_type must be \"code\"")
	}
	challenge := v.Get("code_challenge")
	if challenge == "" {
		return fail("invalid_request", "code_challenge is required (PKCE)")
	}
	if v.Get("code_challenge_method") != "S256" {
		return fail("invalid_request", "code_challenge_method must be S256")
	}
	if !isValidCodeChallenge(challenge) {
		return fail("invalid_request", "code_challenge is malformed")
	}
	resource, ok := s.normalizeResource(issuer, v["resource"])
	if !ok {
		return fail("invalid_target", "resource does not identify this server")
	}
	return &authzRequest{
		Client:      client,
		RedirectURI: redirectURI,
		State:       state,
		Challenge:   challenge,
		Method:      "S256",
		Resource:    resource,
	}, nil
}

// writeAuthzFailure reports f either as a 400 JSON body or as a 302 redirect
// to the client carrying error, error_description, state and iss (RFC 9207).
func writeAuthzFailure(w http.ResponseWriter, r *http.Request, issuer string, f *authzFailure) {
	if f.RedirectURI == "" {
		setNoStore(w)
		writeOAuthError(w, http.StatusBadRequest, f.Code, f.Description)
		return
	}
	u, err := url.Parse(f.RedirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, f.Code, f.Description)
		return
	}
	q := u.Query()
	q.Set("error", f.Code)
	q.Set("error_description", f.Description)
	if f.State != "" {
		q.Set("state", f.State)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	setNoStore(w)
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *OAuthServer) loginData(ar *authzRequest, errMsg string) *loginPageData {
	return &loginPageData{
		ClientID:            ar.Client.ClientID,
		ClientName:          cleanDisplayName(ar.Client.ClientName),
		RedirectURI:         ar.RedirectURI,
		RedirectHost:        redirectDisplay(ar.RedirectURI),
		State:               ar.State,
		CodeChallenge:       ar.Challenge,
		CodeChallengeMethod: ar.Method,
		ResponseType:        "code",
		Resource:            ar.Resource,
		Error:               errMsg,
	}
}

// handleAuthorizeGET renders the login page.
func (s *OAuthServer) handleAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	issuer := s.resolveIssuer(r)
	ar, fail := s.validateAuthorize(r.Context(), issuer, r.URL.Query())
	if fail != nil {
		writeAuthzFailure(w, r, issuer, fail)
		return
	}
	renderHTML(w, http.StatusOK, loginTmpl, s.loginData(ar, ""))
}

// secureEqual compares two strings in constant time. Both sides are hashed
// first so differing lengths do not leak through ConstantTimeCompare's
// early return.
func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// handleAuthorizePOST processes the login form submission.
func (s *OAuthServer) handleAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest
		if isBodyTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		writeOAuthError(w, status, "invalid_request", "malformed form body")
		return
	}

	issuer := s.resolveIssuer(r)
	ar, fail := s.validateAuthorize(r.Context(), issuer, r.PostForm)
	if fail != nil {
		writeAuthzFailure(w, r, issuer, fail)
		return
	}

	ip := s.clientIP(r)
	okKey, waitKey := s.loginLimiter.check(ip)
	okGlobal, waitGlobal := s.globalLimiter.check(globalLimiterKey)
	if !okKey || !okGlobal {
		wait := waitKey
		if waitGlobal > wait {
			wait = waitGlobal
		}
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		renderHTML(w, http.StatusTooManyRequests, loginTmpl, s.loginData(ar, tooManyLoginsMessage))
		return
	}

	// Validate credentials using constant-time comparison (evaluate both).
	usernameMatch := secureEqual(r.PostForm.Get("username"), s.config.Username)
	passwordMatch := secureEqual(r.PostForm.Get("password"), s.config.Password)
	if !usernameMatch || !passwordMatch {
		s.loginLimiter.record(ip)
		s.globalLimiter.record(globalLimiterKey)
		renderHTML(w, http.StatusOK, loginTmpl, s.loginData(ar, invalidCredentialsText))
		return
	}
	s.loginLimiter.reset(ip)

	// Generate authorization code
	code, err := generateToken(32) // 64 hex chars
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	s.codes.Put(&AuthorizationCode{
		Code:                code,
		ClientID:            ar.Client.ClientID,
		RedirectURI:         ar.RedirectURI,
		CodeChallenge:       ar.Challenge,
		CodeChallengeMethod: ar.Method,
		Resource:            ar.Resource,
		State:               ar.State,
		ExpiresAt:           timeNow().Add(authCodeTTL),
	})

	// Build callback URL with code, state and iss (RFC 9207).
	redirectURL, err := url.Parse(ar.RedirectURI)
	if err != nil {
		http.Error(w, "Invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := redirectURL.Query()
	q.Set("code", code)
	if ar.State != "" {
		q.Set("state", ar.State)
	}
	q.Set("iss", issuer)
	redirectURL.RawQuery = q.Encode()

	// Render success page with delayed redirect
	renderHTML(w, http.StatusOK, successTmpl, &successPageData{RedirectURL: redirectURL.String()})
}

// --- 6. Token Endpoint ---

func (s *OAuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}

	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.handleTokenAuthorizationCode(w, r)
	case "refresh_token":
		s.handleTokenRefresh(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *OAuthServer) handleTokenAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	codeVerifier := r.PostForm.Get("code_verifier")
	clientID := r.PostForm.Get("client_id")
	issuer := s.resolveIssuer(r)

	// Atomically redeem the code. It is burned even if a later check fails, so
	// a stolen code cannot be retried with guessed parameters.
	ac, err := s.codes.Consume(code, timeNow())
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}

	if ac.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if ru := r.PostForm.Get("redirect_uri"); ru != "" && ru != ac.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}

	// PKCE is always required.
	if ac.CodeChallenge == "" || !isValidCodeChallenge(codeVerifier) || !verifyPKCE(codeVerifier, ac.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	resource, ok := s.normalizeResource(issuer, r.PostForm["resource"])
	if !ok || (resource != "" && ac.Resource != "" && resource != ac.Resource) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the authorization request")
		return
	}

	s.issueTokenPair(w, clientID)
}

func (s *OAuthServer) handleTokenRefresh(w http.ResponseWriter, r *http.Request) {
	refreshToken := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")

	if _, ok := s.normalizeResource(s.resolveIssuer(r), r.PostForm["resource"]); !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource does not identify this server")
		return
	}

	pair, err := s.tokens.RotateRefreshToken(refreshToken, clientID, timeNow(), s.grace, func() (*TokenPair, error) {
		return newTokenPair()
	})
	if err != nil {
		switch {
		case errors.Is(err, errRefreshClientMismatch):
			log.Printf("OAuth refresh failed: client_id=%s reason=client_mismatch", shortOAuthID(clientID))
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		case errors.Is(err, errReplayUnseal):
			log.Printf("OAuth refresh failed: client_id=%s reason=replay_unseal_failed", shortOAuthID(clientID))
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
		case errors.Is(err, errInvalidRefreshToken):
			log.Printf("OAuth refresh failed: client_id=%s reason=invalid_or_expired_refresh_token", shortOAuthID(clientID))
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
		default:
			log.Printf("OAuth refresh failed: client_id=%s reason=internal_error error=%v", shortOAuthID(clientID), err)
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
		}
		return
	}
	if pair.Replayed {
		log.Printf("OAuth refresh replay accepted: client_id=%s access_expires_at=%s", shortOAuthID(clientID), pair.AccessExpiresAt.UTC().Format(time.RFC3339))
	}
	s.writeTokenPair(w, pair)
}

func shortOAuthID(v string) string {
	if len(v) <= 12 {
		return v
	}
	return v[:12]
}

// issueTokenPair generates and returns a new access+refresh token pair.
func (s *OAuthServer) issueTokenPair(w http.ResponseWriter, clientID string) {
	pair, err := newTokenPair()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	if err := s.tokens.PutAccessToken(pair.AccessToken, clientID, pair.AccessExpiresAt, pair.RefreshToken); err != nil {
		log.Printf("OAuth token issue failed: client_id=%s error=%v", shortOAuthID(clientID), err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to store token")
		return
	}
	if err := s.tokens.PutRefreshToken(pair.RefreshToken, clientID, pair.RefreshExpiresAt); err != nil {
		log.Printf("OAuth token issue failed: client_id=%s error=%v", shortOAuthID(clientID), err)
		s.tokens.DeleteAccessToken(pair.AccessToken)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to store token")
		return
	}
	s.writeTokenPair(w, pair)
}

func newTokenPair() (*TokenPair, error) {
	accessToken, err := generateToken(32)
	if err != nil {
		return nil, err
	}
	refreshToken, err := generateToken(32)
	if err != nil {
		return nil, err
	}

	now := timeNow()
	return &TokenPair{
		AccessToken:      accessToken,
		RefreshToken:     refreshToken,
		AccessExpiresAt:  now.Add(accessTokenTTL),
		RefreshExpiresAt: now.Add(refreshTokenTTL),
	}, nil
}

func (s *OAuthServer) writeTokenPair(w http.ResponseWriter, pair *TokenPair) {
	expiresIn := int(pair.AccessExpiresAt.Sub(timeNow()).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	setNoStore(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  pair.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    expiresIn,
		"refresh_token": pair.RefreshToken,
		"scope":         scopeMCP,
	})
}

// --- 7. Token Revocation (RFC 7009) ---

func (s *OAuthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	clientID := r.PostForm.Get("client_id")
	owned := func(owner string) bool { return clientID == "" || clientID == owner }

	revokeRefresh := func() bool {
		rt, ok := s.tokens.GetRefreshToken(token)
		if !ok {
			return false
		}
		if owned(rt.ClientID) {
			s.tokens.RevokeRefreshToken(token)
		}
		return true
	}
	revokeAccess := func() bool {
		at, ok := s.tokens.GetAccessToken(token)
		if !ok {
			return false
		}
		if owned(at.ClientID) {
			s.tokens.DeleteAccessToken(token)
		}
		return true
	}
	// Try the hinted type first, then fall back to the other (RFC 7009 2.1).
	if r.PostForm.Get("token_type_hint") == "access_token" {
		_ = revokeAccess() || revokeRefresh()
	} else {
		_ = revokeRefresh() || revokeAccess()
	}
	w.WriteHeader(http.StatusOK)
}

// --- Helpers ---

type successPageData struct {
	RedirectURL string
	Nonce       string
}

func (d *successPageData) setNonce(n string) { d.Nonce = n }

type loginPageData struct {
	ClientID            string
	ClientName          string // self-declared by the client
	RedirectURI         string
	RedirectHost        string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	ResponseType        string
	Resource            string
	Error               string
	Nonce               string
}

func (d *loginPageData) setNonce(n string) { d.Nonce = n }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// setNoStore marks a response as uncacheable (RFC 6749 section 5.1).
func setNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// writeOAuthError writes an RFC 6749 section 5.2 style JSON error.
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	setNoStore(w)
	body := map[string]string{"error": code}
	if description != "" {
		body["error_description"] = description
	}
	writeJSON(w, status, body)
}

func writeRateLimited(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", retryAfterSeconds(wait))
	writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests")
}

func retryAfterSeconds(d time.Duration) string {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

func isBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe) || (err != nil && strings.Contains(err.Error(), "request body too large"))
}
