// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Throttling parameters.
const (
	loginFailuresPerKey    = 5
	loginFailuresGlobal    = 50
	loginFailureWindow     = 15 * time.Minute
	registrationsPerKey    = 20
	registrationWindow     = time.Hour
	rateLimiterMaxKeys     = 10000
	maxFormBodyBytes       = 64 << 10
	maxJSONBodyBytes       = 64 << 10
	globalLimiterKey       = ""
	tooManyLoginsMessage   = "Too many failed attempts. Please try again later."
	invalidCredentialsText = "Invalid username or password"
)

// rateLimiter is a small in-memory sliding-window counter. Time comes from the
// package-level timeNow so tests can control it.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	hits    map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, maxKeys: rateLimiterMaxKeys, hits: make(map[string][]time.Time)}
}

// prune drops expired timestamps for key. Caller holds l.mu.
func (l *rateLimiter) prune(key string, now time.Time) []time.Time {
	ts := l.hits[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= l.window {
		i++
	}
	if i > 0 {
		ts = ts[i:]
		if len(ts) == 0 {
			delete(l.hits, key)
		} else {
			l.hits[key] = ts
		}
	}
	return ts
}

// check reports whether key may proceed, without recording anything. When
// blocked it returns how long until the oldest hit leaves the window.
func (l *rateLimiter) check(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkLocked(key, timeNow())
}

func (l *rateLimiter) checkLocked(key string, now time.Time) (bool, time.Duration) {
	ts := l.prune(key, now)
	if len(ts) < l.limit {
		return true, 0
	}
	return false, ts[0].Add(l.window).Sub(now)
}

// record notes one hit for key.
func (l *rateLimiter) record(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recordLocked(key, timeNow())
}

func (l *rateLimiter) recordLocked(key string, now time.Time) {
	if _, exists := l.hits[key]; !exists && len(l.hits) >= l.maxKeys {
		l.cleanupLocked(now)
		if len(l.hits) >= l.maxKeys {
			return // map full: do not grow; other limiters still apply
		}
	}
	l.hits[key] = append(l.prune(key, now), now)
}

// take atomically checks and records one hit.
func (l *rateLimiter) take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := timeNow()
	if ok, wait := l.checkLocked(key, now); !ok {
		return false, wait
	}
	l.recordLocked(key, now)
	return true, 0
}

func (l *rateLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

func (l *rateLimiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(timeNow())
}

func (l *rateLimiter) cleanupLocked(now time.Time) {
	for k := range l.hits {
		l.prune(k, now)
	}
}

func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

// clientIP returns the address used to key throttling.
//
// X-Real-IP is honoured only when the TCP peer is a loopback address, i.e. a
// reverse proxy running on the same host (nginx, caddy, cloudflared) that sets
// the header itself. Any other peer could forge the header to dodge or poison
// limits, so for those we use the real peer address. X-Forwarded-For is never
// trusted because it is client-appendable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Unmap().IsLoopback() {
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			if a, err := netip.ParseAddr(xr); err == nil {
				return a.Unmap().String()
			}
		}
	}
	return host
}
