// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"memory-mcp-server-go/auth"
)

const (
	healthPath         = "/healthz"
	ssePath            = "/sse"
	defaultSessionTime = 24 * time.Hour
)

// httpConfig configures the HTTP handler tree served for the http and sse
// transports.
type httpConfig struct {
	Server    *mcp.Server       // the MCP server to expose
	Transport string            // "http" / "streamable-http" (default) or "sse"
	Endpoint  string            // Streamable HTTP path (default "/mcp"); ignored for sse
	Stateless bool              // Streamable HTTP without session tracking
	PublicURL string            // optional public base URL, scheme://host[:port]
	OAuth     *auth.OAuthServer // optional OAuth server (mutually exclusive with AuthBearer)
	// AuthBearer is a static bearer token required on the MCP endpoint.
	AuthBearer string
	// CORSOrigin is "*" (default) or a comma-separated allow list.
	CORSOrigin string
	// SessionTimeout closes idle Streamable HTTP sessions (default 24h).
	SessionTimeout time.Duration
}

// mcpPath returns the path of the MCP endpoint for the configured transport.
func (c httpConfig) mcpPath() string {
	if c.Transport == "sse" {
		return ssePath
	}
	if c.Endpoint == "" {
		return "/mcp"
	}
	return c.Endpoint
}

// reservedPath reports whether p collides with a route owned by the server.
func reservedPath(p string) bool {
	switch p {
	case healthPath, "/register", "/authorize", "/token", "/revoke":
		return true
	}
	return p == "/.well-known" || strings.HasPrefix(p, "/.well-known/")
}

// normalizeEndpoint validates the MCP endpoint path and returns it without a
// trailing slash.
func normalizeEndpoint(p string) (string, error) {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("endpoint %q must start with '/'", p)
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", errors.New("endpoint must not be '/'")
	}
	if strings.ContainsAny(p, "?#{}* \t") {
		return "", fmt.Errorf("endpoint %q contains invalid characters", p)
	}
	if reservedPath(p) {
		return "", fmt.Errorf("endpoint %q collides with a built-in route", p)
	}
	return p, nil
}

// parseCORS turns a CORS origin setting into the allow-all flag and the list
// of allowed origins.
func parseCORS(corsOrigin string) (allowAll bool, allowed []string) {
	if corsOrigin == "" || corsOrigin == "*" {
		return true, nil
	}
	for _, o := range strings.Split(corsOrigin, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed = append(allowed, o)
		}
	}
	return false, allowed
}

// newHTTPHandler assembles the HTTP handler tree for the http and sse
// transports: the MCP endpoint, the OAuth routes (when OAuth is enabled) and
// /healthz.
func newHTTPHandler(cfg httpConfig) (http.Handler, error) {
	if cfg.Server == nil {
		return nil, errors.New("httpConfig.Server is required")
	}
	if cfg.OAuth != nil && cfg.AuthBearer != "" {
		return nil, errors.New("OAuth and bearer token authentication are mutually exclusive")
	}
	switch cfg.Transport {
	case "", "http", "streamable-http":
		cfg.Transport = "http"
	case "sse":
	default:
		return nil, fmt.Errorf("unsupported HTTP transport %q", cfg.Transport)
	}
	if cfg.Transport == "http" {
		ep := cfg.Endpoint
		if ep == "" {
			ep = "/mcp"
		}
		var err error
		if cfg.Endpoint, err = normalizeEndpoint(ep); err != nil {
			return nil, err
		}
	}
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = defaultSessionTime
	}

	var publicURL *url.URL
	if cfg.PublicURL != "" {
		normalized, err := auth.ValidatePublicURL(cfg.PublicURL)
		if err != nil {
			return nil, fmt.Errorf("invalid public URL: %w", err)
		}
		if publicURL, err = url.Parse(normalized); err != nil {
			return nil, fmt.Errorf("invalid public URL: %w", err)
		}
	}
	authEnabled := cfg.OAuth != nil || cfg.AuthBearer != ""

	// The SDK rejects requests that reach a loopback listener with a
	// non-loopback Host header (DNS rebinding protection). That breaks a
	// reverse proxy in front of 127.0.0.1, so it is turned off whenever the
	// deployment has a public URL (the host guard below takes over) or
	// requires a token (a rebinding page cannot hold one).
	disableLocalhost := publicURL != nil || authEnabled

	var mcpHandler http.Handler
	if cfg.Transport == "sse" {
		mcpHandler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return cfg.Server }, &mcp.SSEOptions{
			DisableLocalhostProtection: disableLocalhost,
		})
	} else {
		mcpHandler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return cfg.Server }, &mcp.StreamableHTTPOptions{
			Stateless:                  cfg.Stateless,
			SessionTimeout:             cfg.SessionTimeout,
			DisableLocalhostProtection: disableLocalhost,
		})
	}

	allowAllOrigins, allowedOrigins := parseCORS(cfg.CORSOrigin)
	corsWrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Origin validation (MCP spec): reject disallowed origins
			if origin != "" && !allowAllOrigins {
				// Same-origin requests are not cross-origin at all, and the
				// OAuth login form posts back to its own origin.
				allowed := sameOriginHost(origin, r.Host)
				for _, o := range allowedOrigins {
					if o == origin {
						allowed = true
						break
					}
				}
				if !allowed {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}

			// Set CORS headers
			if allowAllOrigins {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Authorization, Last-Event-ID")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
			w.Header().Set("Access-Control-Max-Age", "86400")

			// Handle OPTIONS preflight
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}

	hostGuard := newHostGuard(publicURL)

	authWrap := func(next http.Handler) http.Handler { return next }
	switch {
	case cfg.OAuth != nil:
		authWrap = cfg.OAuth.Middleware
	case cfg.AuthBearer != "":
		authWrap = bearerMiddleware(cfg.AuthBearer)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
	})
	if cfg.OAuth != nil {
		cfg.OAuth.RegisterRoutes(mux, func(next http.Handler) http.Handler {
			return corsWrap(hostGuard(next))
		})
	}
	mux.Handle(cfg.mcpPath(), corsWrap(hostGuard(authWrap(mcpHandler))))
	return mux, nil
}

// sameOriginHost reports whether an Origin header names the request's own host.
func sameOriginHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

// newHostGuard rejects requests whose Host header is neither the public host
// nor a loopback host. Without a public URL it is a no-op.
func newHostGuard(publicURL *url.URL) func(http.Handler) http.Handler {
	if publicURL == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	allowed := publicHosts(publicURL)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := strings.ToLower(r.Host)
			if allowed[host] || isLoopbackHostHeader(host) {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "Forbidden: invalid Host header", http.StatusForbidden)
		})
	}
}

// publicHosts lists the Host header values that identify the public URL.
func publicHosts(u *url.URL) map[string]bool {
	hosts := map[string]bool{}
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	defaultPort := map[string]string{"http": "80", "https": "443"}[u.Scheme]
	display := hostname
	if strings.Contains(hostname, ":") {
		display = "[" + hostname + "]" // IPv6 literal
	}
	if port == "" || port == defaultPort {
		hosts[display] = true
		hosts[net.JoinHostPort(hostname, defaultPort)] = true
	}
	if port != "" {
		hosts[net.JoinHostPort(hostname, port)] = true
	}
	return hosts
}

// isLoopbackHostHeader reports whether a Host header names the local machine.
func isLoopbackHostHeader(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// bearerMiddleware requires a static bearer token, compared in constant time.
func bearerMiddleware(token string) func(http.Handler) http.Handler {
	expected := sha256.Sum256([]byte(token))
	verify := func(_ context.Context, presented string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		got := sha256.Sum256([]byte(presented))
		if subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
			return nil, fmt.Errorf("%w: bad token", sdkauth.ErrInvalidToken)
		}
		return &sdkauth.TokenInfo{UserID: "bearer"}, nil
	}
	require := sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only the auth middleware's own writes (the 401) go through the
			// wrapper; next gets the real writer so streaming still works.
			require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r)
			})).ServeHTTP(&challengeWriter{ResponseWriter: w}, r)
		})
	}
}

// challengeWriter adds the RFC 6750 challenge header to 401 responses.
type challengeWriter struct{ http.ResponseWriter }

func (c *challengeWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized && c.Header().Get("WWW-Authenticate") == "" {
		c.Header().Set("WWW-Authenticate", "Bearer")
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *challengeWriter) Write(b []byte) (int, error) {
	return c.ResponseWriter.Write(b)
}

// serveHTTP runs handler on addr until ctx is cancelled, then shuts down
// gracefully (waiting up to 5s for in-flight requests before closing).
func serveHTTP(ctx context.Context, addr string, handler http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serveListener(ctx, ln, handler)
}

func serveListener(ctx context.Context, ln net.Listener, handler http.Handler) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: MCP streams are long-lived.
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Printf("Shutting down HTTP server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown incomplete (%v); closing connections", err)
		_ = srv.Close()
	}
	return nil
}
