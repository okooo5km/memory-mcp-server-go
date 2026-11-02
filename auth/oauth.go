// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

//go:embed login.html success.html
var templateFS embed.FS

var loginTmpl = template.Must(template.ParseFS(templateFS, "login.html"))
var successTmpl = template.Must(template.ParseFS(templateFS, "success.html"))

const (
	defaultRefreshReplayGrace = accessTokenTTL + 15*time.Minute
	defaultResourcePath       = "/mcp"
	scopeMCP                  = "mcp"
)

// Config holds OAuth server configuration.
type Config struct {
	Username           string        // required login username
	Password           string        // required login password
	Issuer             string        // optional public base URL, scheme://host[:port] (auto-detected from request if empty)
	ResourcePath       string        // MCP endpoint path (default "/mcp")
	StorePath          string        // optional SQLite path for durable OAuth clients and tokens
	RefreshReplayGrace time.Duration // optional grace period for concurrent refresh replay
	// TrustedProxies lists the peers (besides loopback, which is always
	// trusted) whose X-Real-IP header is believed when keying rate limits.
	TrustedProxies []netip.Prefix
}

// OAuthServer implements OAuth 2.1 authorization server endpoints for MCP.
type OAuthServer struct {
	config       Config
	resourcePath string
	clients      *ClientStore
	codes        *CodeStore
	tokens       *TokenStore
	store        *persistentStore
	grace        time.Duration
	stopCh       chan struct{} // stops cleanup goroutine

	loginLimiter  *rateLimiter // failed logins per client IP
	globalLimiter *rateLimiter // failed logins across all clients
	regLimiter    *rateLimiter // dynamic registrations per client IP
	cop           *http.CrossOriginProtection

	cimd        cimdCache
	cimdControl func(network, address string, c syscall.RawConn) error // dial guard; tests may replace
	cimdTLS     *tls.Config                                            // nil in production (system roots); tests inject
}

// NewOAuthServer creates a new OAuth server. It panics only when an explicitly
// configured persistent store cannot be opened or the configuration is
// invalid; production code should prefer NewOAuthServerWithError for a clean
// startup error.
func NewOAuthServer(cfg Config) *OAuthServer {
	srv, err := NewOAuthServerWithError(cfg)
	if err != nil {
		panic(err)
	}
	return srv
}

// NewOAuthServerWithError creates a new OAuth server with optional durable storage.
func NewOAuthServerWithError(cfg Config) (*OAuthServer, error) {
	if cfg.Issuer != "" {
		issuer, err := ValidatePublicURL(cfg.Issuer)
		if err != nil {
			return nil, fmt.Errorf("invalid OAuth issuer %q: %w", cfg.Issuer, err)
		}
		cfg.Issuer = issuer
	}
	resourcePath := strings.TrimSpace(cfg.ResourcePath)
	if resourcePath == "" {
		resourcePath = defaultResourcePath
	}
	if !strings.HasPrefix(resourcePath, "/") {
		resourcePath = "/" + resourcePath
	}
	resourcePath = strings.TrimRight(resourcePath, "/")
	if resourcePath == "" || strings.ContainsAny(resourcePath, "?#") {
		return nil, fmt.Errorf("invalid OAuth resource path %q", cfg.ResourcePath)
	}
	cfg.ResourcePath = resourcePath

	codes := newCodeStore()
	tokens := newTokenStore()
	clients := newClientStore()
	var store *persistentStore
	if cfg.StorePath != "" {
		var err error
		store, err = openPersistentStore(cfg.StorePath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize OAuth store: %w", err)
		}
		clients.persist = store
		tokens.persist = store
	}
	grace := cfg.RefreshReplayGrace
	if grace <= 0 {
		grace = defaultRefreshReplayGrace
	}
	s := &OAuthServer{
		config:        cfg,
		resourcePath:  resourcePath,
		clients:       clients,
		codes:         codes,
		tokens:        tokens,
		store:         store,
		grace:         grace,
		loginLimiter:  newRateLimiter(loginFailuresPerKey, loginFailureWindow),
		globalLimiter: newRateLimiter(loginFailuresGlobal, loginFailureWindow),
		regLimiter:    newRateLimiter(registrationsPerKey, registrationWindow),
		cop:           http.NewCrossOriginProtection(),
		cimdControl:   ssrfDialControl,
	}
	s.stopCh = startCleanup(codes, tokens, s.loginLimiter.cleanup, s.globalLimiter.cleanup, s.regLimiter.cleanup, s.cimd.cleanup)
	return s, nil
}

// clientIP returns the address used to key throttling for r.
func (s *OAuthServer) clientIP(r *http.Request) string {
	return clientIP(r, s.config.TrustedProxies)
}

// StorePath returns the configured durable OAuth store path, if any.
func (s *OAuthServer) StorePath() string {
	return s.config.StorePath
}

// RefreshReplayGrace returns the effective refresh replay grace duration.
func (s *OAuthServer) RefreshReplayGrace() time.Duration {
	return s.grace
}

// Close stops the background cleanup goroutine.
func (s *OAuthServer) Close() {
	close(s.stopCh)
	_ = s.store.close()
}

// RegisterRoutes registers OAuth endpoints on the given mux.
// wrapMiddleware is applied to each route (typically CORS middleware).
func (s *OAuthServer) RegisterRoutes(mux *http.ServeMux, wrapMiddleware func(http.Handler) http.Handler) {
	wrap := wrapMiddleware
	prm := http.HandlerFunc(s.handleProtectedResourceMetadata)
	mux.Handle("/.well-known/oauth-protected-resource", wrap(prm))
	mux.Handle("/.well-known/oauth-protected-resource"+s.resourcePath, wrap(prm))
	mux.Handle("/.well-known/oauth-authorization-server", wrap(http.HandlerFunc(s.handleAuthorizationServerMetadata)))
	mux.Handle("/register", wrap(http.HandlerFunc(s.handleRegister)))
	mux.Handle("/authorize", wrap(http.HandlerFunc(s.handleAuthorize)))
	mux.Handle("/token", wrap(http.HandlerFunc(s.handleToken)))
	mux.Handle("/revoke", wrap(http.HandlerFunc(s.handleRevoke)))
}

// Middleware returns an HTTP middleware that validates OAuth bearer tokens.
// Unauthenticated requests receive 401 with a WWW-Authenticate header pointing
// to the path-suffixed protected resource metadata URL and the required scope,
// triggering the OAuth flow in MCP clients.
func (s *OAuthServer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Built per request because the issuer may be auto-detected.
		sdkauth.RequireBearerToken(s.verifyToken, &sdkauth.RequireBearerTokenOptions{
			ResourceMetadataURL: s.resolveIssuer(r) + "/.well-known/oauth-protected-resource" + s.resourcePath,
			Scopes:              []string{scopeMCP},
		})(next).ServeHTTP(w, r)
	})
}

// verifyToken is the sdkauth.TokenVerifier for opaque access tokens issued here.
func (s *OAuthServer) verifyToken(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	entry, ok := s.tokens.GetAccessToken(token)
	if !ok {
		return nil, fmt.Errorf("%w: unknown token", sdkauth.ErrInvalidToken)
	}
	if !entry.ExpiresAt.After(timeNow()) {
		s.tokens.DeleteAccessToken(token)
		return nil, fmt.Errorf("%w: token expired", sdkauth.ErrInvalidToken)
	}
	return &sdkauth.TokenInfo{
		Scopes:     []string{scopeMCP},
		Expiration: entry.ExpiresAt,
		UserID:     s.config.Username,
		Extra:      map[string]any{"client_id": entry.ClientID},
	}, nil
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

// resourceURI is the canonical identifier of the MCP resource (RFC 8707).
func (s *OAuthServer) resourceURI(issuer string) string {
	return issuer + s.resourcePath
}

// normalizeResource validates RFC 8707 resource parameters. This server has a
// single resource, so every value must name it (the endpoint URL, or the bare
// issuer for clients holding a cached older metadata document). It returns the
// first value with any trailing slash removed.
func (s *OAuthServer) normalizeResource(issuer string, values []string) (string, bool) {
	first := ""
	for _, v := range values {
		if v == "" {
			continue
		}
		t := strings.TrimRight(v, "/")
		if t != s.resourceURI(issuer) && t != issuer {
			return "", false
		}
		if first == "" {
			first = t
		}
	}
	return first, true
}

// --- HTML helpers ---

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// URL-safe alphabet: html/template would entity-escape "+" in the attribute.
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// renderHTML executes tmpl with security headers and a per-response CSP nonce.
type noncePage interface{ setNonce(string) }

func renderHTML(w http.ResponseWriter, status int, tmpl *template.Template, data noncePage) {
	nonce, err := newNonce()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	data.setNonce(nonce)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	// Not no-referrer: browsers then send "Origin: null" on the login form POST,
	// which fails both the CORS allow list and the cross-origin check.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
