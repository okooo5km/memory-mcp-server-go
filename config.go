// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package main

import (
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"memory-mcp-server-go/auth"
)

// flagValues holds the values of the command-line flags as parsed.
type flagValues struct {
	Transport    string
	Memory       string
	Port         int
	Host         string
	HTTPEndpoint string
	Heartbeat    string
	Stateless    bool

	AuthBearer              string
	OAuthUser               string
	OAuthPass               string
	OAuthIssuer             string
	PublicURL               string
	OAuthStore              string
	OAuthRefreshReplayGrace string
	CORSOrigin              string
	TrustedProxies          string
}

// appConfig is the effective configuration after merging flags, environment
// variables and public-URL derived defaults.
type appConfig struct {
	Transport string // stdio, sse or http (streamable-http is normalized to http)
	Memory    string
	Port      int
	Host      string

	HTTPEndpoint string
	Heartbeat    time.Duration
	Stateless    bool

	AuthBearer         string
	OAuthUser          string
	OAuthPass          string
	PublicURL          string // normalized scheme://host[:port], or ""
	Issuer             string // OAuth issuer: the public URL or the legacy --oauth-issuer, or ""
	OAuthStore         string
	OAuthStoreDefault  bool // OAuthStore must be derived from the memory path (see setDefaultOAuthStore)
	RefreshReplayGrace time.Duration
	CORSOrigin         string
	TrustedProxies     []netip.Prefix // peers whose X-Real-IP is believed (loopback is always trusted)
}

// OAuthEnabled reports whether OAuth login credentials were configured.
func (c *appConfig) OAuthEnabled() bool { return c.OAuthUser != "" && c.OAuthPass != "" }

// MCPPath is the path of the MCP endpoint served for the transport.
func (c *appConfig) MCPPath() string {
	if c.Transport == "sse" {
		return ssePath
	}
	return c.HTTPEndpoint
}

// flagAliases maps alias flag names to their canonical name.
var flagAliases = map[string]string{
	"t":       "transport",
	"m":       "memory",
	"p":       "port",
	"http_ep": "http-endpoint",
}

// explicitFlags returns the canonical names of the flags set on the command line.
func explicitFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		name := f.Name
		if canonical, ok := flagAliases[name]; ok {
			name = canonical
		}
		set[name] = true
	})
	return set
}

// resolveConfig merges flags, environment variables and defaults. Flags win
// over environment variables. explicit holds the canonical names of the flags
// given on the command line (see explicitFlags).
//
// When a public URL is configured the following become defaults, used only
// when not given explicitly (flag or env): transport=http, host=127.0.0.1 and
// (when OAuth is enabled) the OAuth store next to the memory database. The
// legacy OAuth issuer sets only the issuer, so existing deployments keep their
// bind address and accepted hosts when they upgrade.
func resolveConfig(explicit map[string]bool, getenv func(string) string, f flagValues) (*appConfig, error) {
	c := &appConfig{
		Memory:       f.Memory,
		Stateless:    f.Stateless,
		HTTPEndpoint: "/mcp",
	}

	// firstNonEmpty: a non-empty flag value wins over the environment.
	firstNonEmpty := func(flagVal, envKey string) string {
		if flagVal != "" {
			return flagVal
		}
		return getenv(envKey)
	}
	// explicitOrEnv: an explicitly given flag (even empty) wins over the environment.
	explicitOrEnv := func(name, flagVal, envKey string) (string, bool) {
		if explicit[name] {
			return flagVal, true
		}
		if v := getenv(envKey); v != "" {
			return v, true
		}
		return "", false
	}

	// Public URL and the legacy OAuth issuer.
	publicURL := firstNonEmpty(f.PublicURL, "MMS_PUBLIC_URL")
	issuer := firstNonEmpty(f.OAuthIssuer, "OAUTH_ISSUER")
	if publicURL != "" {
		v, err := auth.ValidatePublicURL(publicURL)
		if err != nil {
			return nil, fmt.Errorf("invalid public URL (--public-url / MMS_PUBLIC_URL): %w", err)
		}
		publicURL = v
	}
	if issuer != "" {
		v, err := auth.ValidatePublicURL(issuer)
		if err != nil {
			return nil, fmt.Errorf("invalid OAuth issuer (--oauth-issuer / OAUTH_ISSUER): %w", err)
		}
		issuer = v
	}
	if publicURL != "" && issuer != "" && publicURL != issuer {
		return nil, fmt.Errorf("public URL %q and OAuth issuer %q differ; set only --public-url / MMS_PUBLIC_URL", publicURL, issuer)
	}
	c.PublicURL = publicURL
	c.Issuer = issuer
	if publicURL != "" {
		c.Issuer = publicURL
	}

	// Transport.
	if v, ok := explicitOrEnv("transport", f.Transport, "MMS_TRANSPORT"); ok {
		c.Transport = v
	} else if c.PublicURL != "" {
		c.Transport = "http"
	} else {
		c.Transport = "stdio"
	}
	switch c.Transport {
	case "stdio", "sse", "http":
	case "streamable-http":
		c.Transport = "http"
	default:
		return nil, fmt.Errorf("invalid transport %q (use stdio, http or sse)", c.Transport)
	}

	// Host and port.
	if v, ok := explicitOrEnv("host", f.Host, "MMS_HOST"); ok {
		c.Host = v
	} else if c.PublicURL != "" {
		c.Host = "127.0.0.1"
	}
	c.Port = f.Port
	if !explicit["port"] {
		c.Port = 8080
		if v := getenv("MMS_PORT"); v != "" {
			p, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("invalid MMS_PORT %q: not a number", v)
			}
			c.Port = p
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d (must be 1-65535)", c.Port)
	}

	// HTTP endpoint.
	if v, ok := explicitOrEnv("http-endpoint", f.HTTPEndpoint, "MMS_HTTP_ENDPOINT"); ok {
		c.HTTPEndpoint = v
	}
	if c.Transport == "http" {
		ep, err := normalizeEndpoint(c.HTTPEndpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid HTTP endpoint (--http-endpoint / MMS_HTTP_ENDPOINT): %w", err)
		}
		c.HTTPEndpoint = ep
	}

	// Heartbeat: "0" disables.
	hb := strings.TrimSpace(f.Heartbeat)
	if hb == "" {
		hb = "0"
	}
	d, err := time.ParseDuration(hb)
	if err != nil {
		return nil, fmt.Errorf("invalid --http-heartbeat %q: %w", f.Heartbeat, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("invalid --http-heartbeat %q: must not be negative", f.Heartbeat)
	}
	c.Heartbeat = d

	// Authentication.
	c.AuthBearer = firstNonEmpty(f.AuthBearer, "MMS_AUTH_BEARER")
	c.OAuthUser = firstNonEmpty(f.OAuthUser, "OAUTH_USER")
	c.OAuthPass = firstNonEmpty(f.OAuthPass, "OAUTH_PASS")
	if (c.OAuthUser != "") != (c.OAuthPass != "") {
		return nil, errors.New("both OAuth username and password (--oauth-user/--oauth-pass or OAUTH_USER/OAUTH_PASS) must be provided together")
	}
	if c.OAuthEnabled() && c.AuthBearer != "" {
		return nil, errors.New("bearer token (--auth-bearer) and OAuth credentials (--oauth-user/--oauth-pass) are mutually exclusive")
	}

	// OAuth store and refresh replay grace.
	c.OAuthStore = firstNonEmpty(f.OAuthStore, "OAUTH_STORE_PATH")
	if c.OAuthStore == "" && c.OAuthEnabled() && c.PublicURL != "" {
		c.OAuthStoreDefault = true
	}
	if g := firstNonEmpty(f.OAuthRefreshReplayGrace, "OAUTH_REFRESH_REPLAY_GRACE"); g != "" {
		c.RefreshReplayGrace, err = time.ParseDuration(g)
		if err != nil {
			return nil, fmt.Errorf("invalid --oauth-refresh-replay-grace: %w", err)
		}
	}

	// CORS.
	c.CORSOrigin = "*"
	if v, ok := explicitOrEnv("cors-origin", f.CORSOrigin, "MMS_CORS_ORIGIN"); ok {
		c.CORSOrigin = v
	}

	// Trusted proxies.
	c.TrustedProxies, err = auth.ParseTrustedProxies(firstNonEmpty(f.TrustedProxies, "MMS_TRUSTED_PROXIES"))
	if err != nil {
		return nil, fmt.Errorf("invalid trusted proxies (--trusted-proxies / MMS_TRUSTED_PROXIES): %w", err)
	}
	return c, nil
}

// setDefaultOAuthStore derives the OAuth store path from the directory of the
// real memory database path, when it was left to default.
func (c *appConfig) setDefaultOAuthStore(memoryPath string) {
	if c.OAuthStoreDefault && c.OAuthStore == "" {
		c.OAuthStore = filepath.Join(filepath.Dir(memoryPath), "oauth.db")
	}
}

// Warnings returns operator warnings about risky but valid configurations.
// They never contain secrets.
func (c *appConfig) Warnings() []string {
	if c.Transport == "stdio" {
		return nil
	}
	var w []string
	if c.OAuthEnabled() {
		if c.Issuer == "" && !isLoopbackListenHost(c.Host) {
			w = append(w, "OAuth is enabled without a public URL and the server is not bound to loopback: the issuer is auto-detected from request headers. Set --public-url / MMS_PUBLIC_URL.")
		}
		if len(c.OAuthPass) < 12 {
			w = append(w, "OAuth password is shorter than 12 characters; use a longer one (prefer the OAUTH_PASS environment variable).")
		}
		if len(c.TrustedProxies) == 0 && !isLoopbackListenHost(c.Host) {
			w = append(w, "OAuth is enabled, the server is not bound to loopback and no trusted proxies are set: if a reverse proxy in another container or host sits in front, the login rate limiter sees only the proxy's address, so a few failed logins lock out every user. Set --trusted-proxies / MMS_TRUSTED_PROXIES to the proxy's address or subnet.")
		}
		if c.OAuthStore == "" {
			w = append(w, "OAuth store is in memory: clients must log in again after every restart. Set --oauth-store / OAUTH_STORE_PATH.")
		}
	}
	if c.Issuer != "" {
		if u, err := url.Parse(c.Issuer); err == nil && u.Scheme == "http" && !isLoopbackListenHost(u.Hostname()) {
			w = append(w, "Public URL uses plain http on a non-loopback host: tokens and passwords travel unencrypted. Use https.")
		}
	}
	return w
}

// isLoopbackListenHost reports whether a bind host or URL hostname is loopback.
// An empty host means all interfaces and is not loopback.
func isLoopbackListenHost(host string) bool {
	if host == "" {
		return false
	}
	return isLoopbackHostHeader(strings.ToLower(host))
}
