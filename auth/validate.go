// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// ValidatePublicURL validates a public base URL (scheme://host[:port]) and
// returns it with any trailing slash removed.
func ValidatePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	trimmed := strings.TrimRight(raw, "/")
	if trimmed == "" {
		return "", errors.New("URL is empty")
	}
	if strings.ContainsAny(trimmed, "?#") {
		return "", errors.New("URL must not contain a query or fragment")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("URL scheme must be http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("URL must include a host")
	}
	if u.User != nil {
		return "", errors.New("URL must not contain userinfo")
	}
	if u.Path != "" || u.RawPath != "" {
		return "", errors.New("URL must not contain a path")
	}
	return trimmed, nil
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func isLoopbackHTTP(u *url.URL) bool {
	return u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

// blockedRedirectSchemes can never be valid redirect targets. Other custom
// schemes are allowed: native MCP clients often use dotless ones (raycast://),
// and an attacker able to register can already use an https redirect, so an
// allow list would break clients without adding protection.
var blockedRedirectSchemes = map[string]bool{
	"javascript": true, "data": true, "vbscript": true, "file": true,
	"about": true, "blob": true, "ftp": true, "ws": true, "wss": true,
}

const (
	maxRedirectURILength = 2048
	maxRedirectURIs      = 10
)

// validateRedirectURI checks a redirect URI supplied at registration time
// (DCR or CIMD).
func validateRedirectURI(raw string) error {
	if raw == "" || len(raw) > maxRedirectURILength {
		return errors.New("redirect_uri is empty or too long")
	}
	if strings.Contains(raw, "#") {
		return errors.New("redirect_uri must not contain a fragment")
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("redirect_uri contains invalid characters")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirect_uri is not a valid URI: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return errors.New("redirect_uri must be absolute")
	}
	if u.User != nil {
		return errors.New("redirect_uri must not contain userinfo")
	}
	switch {
	case blockedRedirectSchemes[scheme]:
		return fmt.Errorf("redirect_uri scheme %q is not allowed", scheme)
	case scheme == "https":
		if u.Hostname() == "" {
			return errors.New("https redirect_uri must include a host")
		}
	case scheme == "http":
		if !isLoopbackHost(u.Hostname()) {
			return errors.New("http redirect_uri is only allowed for loopback hosts")
		}
	}
	return nil
}

// matchRedirectURI resolves the redirect_uri of an authorization request
// against the client's registered URIs. Matching is exact, except that for
// http loopback URIs the port may differ (RFC 8252 section 7.3). An empty
// request value selects the sole registered URI, if there is exactly one.
func matchRedirectURI(registered []string, given string) (string, bool) {
	if given == "" {
		if len(registered) == 1 {
			return registered[0], true
		}
		return "", false
	}
	for _, reg := range registered {
		if reg == given {
			return given, true
		}
	}
	gu, err := url.Parse(given)
	if err != nil || !isLoopbackHTTP(gu) || gu.User != nil || gu.Fragment != "" || strings.Contains(given, "#") {
		return "", false
	}
	for _, reg := range registered {
		ru, err := url.Parse(reg)
		if err != nil || !isLoopbackHTTP(ru) {
			continue
		}
		if strings.EqualFold(gu.Hostname(), ru.Hostname()) &&
			gu.EscapedPath() == ru.EscapedPath() &&
			gu.RawQuery == ru.RawQuery {
			return given, true
		}
	}
	return "", false
}

// redirectDisplay is the part of a redirect URI shown to the user.
func redirectDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return u.Host
	}
	return u.Scheme + "://" + u.Host
}

// isValidCodeChallenge checks RFC 7636 charset and length (43..128).
func isValidCodeChallenge(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// cleanDisplayName strips control characters and bounds the length of a
// self-declared client name.
func cleanDisplayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	return s
}
