// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Client ID Metadata Documents (draft-ietf-oauth-client-id-metadata-document):
// a client_id that is an https URL pointing at a JSON document describing the
// client. The fetch is attacker-triggered (anyone can send /authorize with an
// arbitrary client_id), so it is SSRF-hardened.
const (
	cimdMaxBytes   = 64 << 10
	cimdTimeout    = 5 * time.Second
	cimdCacheTTL   = 10 * time.Minute
	cimdCacheLimit = 256
)

type cimdCacheEntry struct {
	client  *OAuthClient
	expires time.Time
}

type cimdCache struct {
	mu      sync.Mutex
	entries map[string]cimdCacheEntry
}

func (c *cimdCache) get(key string) (*OAuthClient, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !timeNow().Before(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return e.client, true
}

func (c *cimdCache) put(key string, client *OAuthClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]cimdCacheEntry)
	}
	now := timeNow()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= cimdCacheLimit {
		c.purgeLocked(now)
		if len(c.entries) >= cimdCacheLimit {
			// still full: evict the entry closest to expiry
			var oldestKey string
			var oldest time.Time
			for k, e := range c.entries {
				if oldestKey == "" || e.expires.Before(oldest) {
					oldestKey, oldest = k, e.expires
				}
			}
			delete(c.entries, oldestKey)
		}
	}
	c.entries[key] = cimdCacheEntry{client: client, expires: now.Add(cimdCacheTTL)}
}

func (c *cimdCache) purgeLocked(now time.Time) {
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
}

func (c *cimdCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked(timeNow())
}

// isCIMDClientID reports whether clientID is an https URL usable as a metadata
// document location.
func isCIMDClientID(clientID string) bool {
	if len(clientID) > maxRedirectURILength || !strings.HasPrefix(clientID, "https://") || strings.Contains(clientID, "#") {
		return false
	}
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Path == "" || u.Path == "/" {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8",       // "this" network
		"100.64.0.0/10",   // CGNAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved + broadcast
		"fc00::/7",        // IPv6 ULA
		"64:ff9b::/96",    // NAT64
		"2001:db8::/32",   // documentation
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// ipBlocked reports whether addr must never be dialed for a CIMD fetch.
func ipBlocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ssrfDialControl is a net.Dialer.Control callback. It runs after DNS
// resolution, on the concrete address about to be connected, so DNS rebinding
// and redirects to internal names cannot bypass it.
func ssrfDialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("refusing to dial non-IP address %q", host)
	}
	if ipBlocked(addr) {
		return fmt.Errorf("refusing to dial blocked address %s", addr)
	}
	return nil
}

func (s *OAuthServer) newCIMDHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: cimdTimeout, Control: s.cimdControl}
	return &http.Client{
		Timeout: cimdTimeout,
		Transport: &http.Transport{
			Proxy:             nil, // never route through an env-configured proxy
			TLSClientConfig:   s.cimdTLS,
			DialContext:       dialer.DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type cimdDocument struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	ClientSecret            string   `json:"client_secret"`
}

// fetchCIMD downloads and validates the metadata document at clientID.
func (s *OAuthServer) fetchCIMD(ctx context.Context, clientID string) (*OAuthClient, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "memory-mcp-server-oauth")
	hc := s.newCIMDHTTPClient()
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch client metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client metadata returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read client metadata: %w", err)
	}
	if len(body) > cimdMaxBytes {
		return nil, errors.New("client metadata document too large")
	}
	var doc cimdDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, errors.New("client metadata is not valid JSON")
	}
	if doc.ClientID != clientID {
		return nil, errors.New("client metadata client_id does not match its URL")
	}
	if doc.TokenEndpointAuthMethod != "" && doc.TokenEndpointAuthMethod != "none" {
		return nil, errors.New("client metadata token_endpoint_auth_method must be none")
	}
	if doc.ClientSecret != "" {
		return nil, errors.New("client metadata must not contain client_secret")
	}
	if len(doc.RedirectURIs) == 0 || len(doc.RedirectURIs) > maxRedirectURIs {
		return nil, errors.New("client metadata redirect_uris missing or too many")
	}
	for _, u := range doc.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			return nil, fmt.Errorf("client metadata: %w", err)
		}
	}
	return &OAuthClient{
		ClientID:      clientID,
		ClientName:    cleanDisplayName(doc.ClientName),
		RedirectURIs:  doc.RedirectURIs,
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		CreatedAt:     timeNow(),
	}, nil
}

var errUnknownClient = errors.New("unknown client_id")

// resolveClient finds the client for clientID: a CIMD document for https URL
// client IDs, otherwise a dynamically registered client.
func (s *OAuthServer) resolveClient(ctx context.Context, clientID string) (*OAuthClient, error) {
	if clientID == "" {
		return nil, errors.New("client_id is required")
	}
	if isCIMDClientID(clientID) {
		if c, ok := s.cimd.get(clientID); ok {
			return c, nil
		}
		c, err := s.fetchCIMD(ctx, clientID)
		if err != nil {
			return nil, err
		}
		s.cimd.put(clientID, c)
		return c, nil
	}
	if c, ok := s.clients.Get(clientID); ok {
		return c, nil
	}
	return nil, errUnknownClient
}
