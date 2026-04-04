// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"embed"
	"html/template"
	"net/http"
	"strings"
)

//go:embed login.html success.html
var templateFS embed.FS

var loginTmpl = template.Must(template.ParseFS(templateFS, "login.html"))
var successTmpl = template.Must(template.ParseFS(templateFS, "success.html"))

// Config holds OAuth server configuration.
type Config struct {
	Username string // required login username
	Password string // required login password
	Issuer   string // optional explicit issuer URL (auto-detected from request if empty)
}

// OAuthServer implements OAuth 2.1 authorization server endpoints for MCP.
type OAuthServer struct {
	config  Config
	clients *ClientStore
	codes   *CodeStore
	tokens  *TokenStore
	stopCh  chan struct{} // stops cleanup goroutine
}

// NewOAuthServer creates a new OAuth server with in-memory stores.
func NewOAuthServer(cfg Config) *OAuthServer {
	codes := newCodeStore()
	tokens := newTokenStore()
	return &OAuthServer{
		config:  cfg,
		clients: newClientStore(),
		codes:   codes,
		tokens:  tokens,
		stopCh:  startCleanup(codes, tokens),
	}
}

// Close stops the background cleanup goroutine.
func (s *OAuthServer) Close() {
	close(s.stopCh)
}

// RegisterRoutes registers OAuth endpoints on the given mux.
// wrapMiddleware is applied to each route (typically CORS middleware).
func (s *OAuthServer) RegisterRoutes(mux *http.ServeMux, wrapMiddleware func(http.Handler) http.Handler) {
	wrap := wrapMiddleware
	mux.Handle("/.well-known/oauth-protected-resource", wrap(http.HandlerFunc(s.handleProtectedResourceMetadata)))
	mux.Handle("/.well-known/oauth-authorization-server", wrap(http.HandlerFunc(s.handleAuthorizationServerMetadata)))
	mux.Handle("/register", wrap(http.HandlerFunc(s.handleRegister)))
	mux.Handle("/authorize", wrap(http.HandlerFunc(s.handleAuthorize)))
	mux.Handle("/token", wrap(http.HandlerFunc(s.handleToken)))
}

// Middleware returns an HTTP middleware that validates OAuth bearer tokens.
// Unauthenticated requests receive 401 with WWW-Authenticate header pointing
// to the protected resource metadata URL, triggering the OAuth flow in MCP clients.
func (s *OAuthServer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(auth, "Bearer ") {
			s.unauthorized(w, r)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		entry, ok := s.tokens.GetAccessToken(token)
		if !ok {
			s.unauthorized(w, r)
			return
		}
		if entry.ExpiresAt.Before(timeNow()) {
			s.tokens.DeleteAccessToken(token)
			s.unauthorized(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// unauthorized sends a 401 response with proper WWW-Authenticate header.
func (s *OAuthServer) unauthorized(w http.ResponseWriter, r *http.Request) {
	issuer := s.resolveIssuer(r)
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+issuer+`/.well-known/oauth-protected-resource"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

// resolveIssuer determines the issuer URL from config or request.
func (s *OAuthServer) resolveIssuer(r *http.Request) string {
	if s.config.Issuer != "" {
		return strings.TrimRight(s.config.Issuer, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
