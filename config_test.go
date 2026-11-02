// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package main

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memory-mcp-server-go/auth"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// defaultFlags mirrors the defaults registered in main().
func defaultFlags() flagValues {
	return flagValues{Transport: "stdio", Port: 8080, HTTPEndpoint: "/mcp", Heartbeat: "0", CORSOrigin: "*"}
}

func TestResolveConfig(t *testing.T) {
	type tc struct {
		name     string
		explicit []string
		env      map[string]string
		mutate   func(*flagValues)
		wantErr  string // substring; empty means success
		check    func(t *testing.T, c *appConfig)
	}
	cases := []tc{
		{name: "defaults", check: func(t *testing.T, c *appConfig) {
			if c.Transport != "stdio" || c.Port != 8080 || c.Host != "" || c.HTTPEndpoint != "/mcp" ||
				c.CORSOrigin != "*" || c.Heartbeat != 0 || c.PublicURL != "" || c.OAuthStoreDefault {
				t.Errorf("unexpected defaults: %+v", c)
			}
		}},
		{name: "public url flag implies http on loopback", mutate: func(f *flagValues) { f.PublicURL = "https://mcp.example.com/" },
			check: func(t *testing.T, c *appConfig) {
				if c.Transport != "http" || c.Host != "127.0.0.1" || c.PublicURL != "https://mcp.example.com" || c.Port != 8080 {
					t.Errorf("%+v", c)
				}
			}},
		{name: "public url env", env: map[string]string{"MMS_PUBLIC_URL": "https://mcp.example.com"},
			check: func(t *testing.T, c *appConfig) {
				if c.Transport != "http" || c.Host != "127.0.0.1" || c.PublicURL != "https://mcp.example.com" {
					t.Errorf("%+v", c)
				}
			}},
		{name: "explicit transport wins over public url", explicit: []string{"transport"}, mutate: func(f *flagValues) { f.Transport = "sse"; f.PublicURL = "https://x.example" },
			check: func(t *testing.T, c *appConfig) {
				if c.Transport != "sse" {
					t.Errorf("transport = %s", c.Transport)
				}
			}},
		{name: "explicit stdio wins over public url", explicit: []string{"transport"}, mutate: func(f *flagValues) { f.PublicURL = "https://x.example" },
			check: func(t *testing.T, c *appConfig) {
				if c.Transport != "stdio" {
					t.Errorf("transport = %s", c.Transport)
				}
			}},
		{name: "env transport wins over public url", env: map[string]string{"MMS_TRANSPORT": "sse", "MMS_PUBLIC_URL": "https://x.example"},
			check: func(t *testing.T, c *appConfig) {
				if c.Transport != "sse" || c.Host != "127.0.0.1" {
					t.Errorf("%+v", c)
				}
			}},
		{name: "flag transport beats env", explicit: []string{"transport"}, env: map[string]string{"MMS_TRANSPORT": "sse"}, mutate: func(f *flagValues) { f.Transport = "http" },
			check: func(t *testing.T, c *appConfig) {
				if c.Transport != "http" {
					t.Errorf("transport = %s", c.Transport)
				}
			}},
		{name: "streamable-http alias", explicit: []string{"transport"}, mutate: func(f *flagValues) { f.Transport = "streamable-http" },
			check: func(t *testing.T, c *appConfig) {
				if c.Transport != "http" {
					t.Errorf("transport = %s", c.Transport)
				}
			}},
		{name: "invalid transport", explicit: []string{"transport"}, mutate: func(f *flagValues) { f.Transport = "ws" }, wantErr: "invalid transport"},
		{name: "explicit host wins", explicit: []string{"host"}, mutate: func(f *flagValues) { f.Host = "0.0.0.0"; f.PublicURL = "https://x.example" },
			check: func(t *testing.T, c *appConfig) {
				if c.Host != "0.0.0.0" {
					t.Errorf("host = %q", c.Host)
				}
			}},
		{name: "explicit empty host means all interfaces", explicit: []string{"host"}, mutate: func(f *flagValues) { f.PublicURL = "https://x.example" },
			check: func(t *testing.T, c *appConfig) {
				if c.Host != "" {
					t.Errorf("host = %q", c.Host)
				}
			}},
		{name: "env host wins over derived", env: map[string]string{"MMS_HOST": "10.0.0.5"}, mutate: func(f *flagValues) { f.PublicURL = "https://x.example" },
			check: func(t *testing.T, c *appConfig) {
				if c.Host != "10.0.0.5" {
					t.Errorf("host = %q", c.Host)
				}
			}},
		{name: "env port", env: map[string]string{"MMS_PORT": "18765"}, check: func(t *testing.T, c *appConfig) {
			if c.Port != 18765 {
				t.Errorf("port = %d", c.Port)
			}
		}},
		{name: "flag port beats env", explicit: []string{"port"}, env: map[string]string{"MMS_PORT": "18765"}, mutate: func(f *flagValues) { f.Port = 9000 },
			check: func(t *testing.T, c *appConfig) {
				if c.Port != 9000 {
					t.Errorf("port = %d", c.Port)
				}
			}},
		{name: "invalid env port", env: map[string]string{"MMS_PORT": "http"}, wantErr: "MMS_PORT"},
		{name: "out of range env port", env: map[string]string{"MMS_PORT": "70000"}, wantErr: "port"},
		{name: "out of range flag port", explicit: []string{"port"}, mutate: func(f *flagValues) { f.Port = 0 }, wantErr: "port"},
		{name: "issuer alone sets only the issuer", explicit: []string{"transport"}, mutate: func(f *flagValues) {
			f.Transport = "http"
			f.OAuthIssuer = "https://mcp.example.com"
			f.OAuthUser, f.OAuthPass = "admin", "pw"
		},
			check: func(t *testing.T, c *appConfig) {
				if c.Issuer != "https://mcp.example.com" || c.PublicURL != "" || c.Host != "" || c.OAuthStoreDefault {
					t.Errorf("%+v", c)
				}
			}},
		{name: "issuer env alone keeps the default transport", env: map[string]string{"OAUTH_ISSUER": "https://mcp.example.com"},
			check: func(t *testing.T, c *appConfig) {
				if c.Issuer != "https://mcp.example.com" || c.PublicURL != "" || c.Transport != "stdio" {
					t.Errorf("%+v", c)
				}
			}},
		{name: "public url is the issuer", mutate: func(f *flagValues) { f.PublicURL = "https://mcp.example.com" },
			check: func(t *testing.T, c *appConfig) {
				if c.Issuer != "https://mcp.example.com" {
					t.Errorf("%+v", c)
				}
			}},
		{name: "issuer equal to public url", mutate: func(f *flagValues) {
			f.OAuthIssuer = "https://mcp.example.com/"
			f.PublicURL = "https://mcp.example.com"
		},
			check: func(t *testing.T, c *appConfig) {
				if c.PublicURL != "https://mcp.example.com" {
					t.Errorf("%+v", c)
				}
			}},
		{name: "issuer differs from public url", mutate: func(f *flagValues) { f.OAuthIssuer = "https://a.example"; f.PublicURL = "https://b.example" }, wantErr: "differ"},
		{name: "invalid public url", mutate: func(f *flagValues) { f.PublicURL = "https://mcp.example.com/path" }, wantErr: "public URL"},
		{name: "invalid issuer", mutate: func(f *flagValues) { f.OAuthIssuer = "mcp.example.com" }, wantErr: "issuer"},
		{name: "oauth store defaults only with oauth and public url", mutate: func(f *flagValues) {
			f.PublicURL = "https://x.example"
			f.OAuthUser, f.OAuthPass = "admin", "pw"
		}, check: func(t *testing.T, c *appConfig) {
			if !c.OAuthStoreDefault || c.OAuthStore != "" {
				t.Errorf("%+v", c)
			}
			c.setDefaultOAuthStore(filepath.Join("data", "memory.db"))
			if c.OAuthStore != filepath.Join("data", "oauth.db") {
				t.Errorf("store = %q", c.OAuthStore)
			}
		}},
		{name: "no oauth store default without oauth", mutate: func(f *flagValues) { f.PublicURL = "https://x.example" },
			check: func(t *testing.T, c *appConfig) {
				if c.OAuthStoreDefault {
					t.Errorf("store default without oauth")
				}
			}},
		{name: "no oauth store default without public url", mutate: func(f *flagValues) { f.OAuthUser, f.OAuthPass = "admin", "pw" },
			check: func(t *testing.T, c *appConfig) {
				if c.OAuthStoreDefault {
					t.Errorf("store default without public url")
				}
			}},
		{name: "explicit oauth store kept", mutate: func(f *flagValues) {
			f.PublicURL = "https://x.example"
			f.OAuthUser, f.OAuthPass = "admin", "pw"
			f.OAuthStore = "custom.db"
		}, check: func(t *testing.T, c *appConfig) {
			c.setDefaultOAuthStore("data/memory.db")
			if c.OAuthStore != "custom.db" || c.OAuthStoreDefault {
				t.Errorf("%+v", c)
			}
		}},
		{name: "env oauth store kept", env: map[string]string{"OAUTH_STORE_PATH": "env.db", "OAUTH_USER": "u", "OAUTH_PASS": "p", "MMS_PUBLIC_URL": "https://x.example"},
			check: func(t *testing.T, c *appConfig) {
				c.setDefaultOAuthStore("data/memory.db")
				if c.OAuthStore != "env.db" {
					t.Errorf("store = %q", c.OAuthStore)
				}
			}},
		{name: "oauth env credentials", env: map[string]string{"OAUTH_USER": "u", "OAUTH_PASS": "p"},
			check: func(t *testing.T, c *appConfig) {
				if !c.OAuthEnabled() {
					t.Errorf("oauth should be enabled")
				}
			}},
		{name: "user without password", mutate: func(f *flagValues) { f.OAuthUser = "admin" }, wantErr: "together"},
		{name: "oauth and bearer", mutate: func(f *flagValues) { f.OAuthUser, f.OAuthPass, f.AuthBearer = "a", "b", "t" }, wantErr: "mutually exclusive"},
		{name: "oauth env and bearer env", env: map[string]string{"OAUTH_USER": "u", "OAUTH_PASS": "p", "MMS_AUTH_BEARER": "t"}, wantErr: "mutually exclusive"},
		{name: "bearer env", env: map[string]string{"MMS_AUTH_BEARER": "tok"}, check: func(t *testing.T, c *appConfig) {
			if c.AuthBearer != "tok" {
				t.Errorf("bearer = %q", c.AuthBearer)
			}
		}},
		{name: "bearer flag beats env", env: map[string]string{"MMS_AUTH_BEARER": "env"}, mutate: func(f *flagValues) { f.AuthBearer = "flag" },
			check: func(t *testing.T, c *appConfig) {
				if c.AuthBearer != "flag" {
					t.Errorf("bearer = %q", c.AuthBearer)
				}
			}},
		{name: "endpoint env normalized", explicit: []string{"transport"}, env: map[string]string{"MMS_HTTP_ENDPOINT": "/api/mcp/"}, mutate: func(f *flagValues) { f.Transport = "http" },
			check: func(t *testing.T, c *appConfig) {
				if c.HTTPEndpoint != "/api/mcp" {
					t.Errorf("endpoint = %q", c.HTTPEndpoint)
				}
			}},
		{name: "endpoint flag beats env", explicit: []string{"http-endpoint"}, env: map[string]string{"MMS_HTTP_ENDPOINT": "/env"}, mutate: func(f *flagValues) { f.HTTPEndpoint = "/flag" },
			check: func(t *testing.T, c *appConfig) {
				if c.HTTPEndpoint != "/flag" {
					t.Errorf("endpoint = %q", c.HTTPEndpoint)
				}
			}},
		{name: "endpoint without slash", env: map[string]string{"MMS_PUBLIC_URL": "https://x.example", "MMS_HTTP_ENDPOINT": "mcp"}, wantErr: "endpoint"},
		{name: "endpoint collides with healthz", env: map[string]string{"MMS_PUBLIC_URL": "https://x.example", "MMS_HTTP_ENDPOINT": "/healthz"}, wantErr: "collides"},
		{name: "endpoint collides with oauth route", env: map[string]string{"MMS_PUBLIC_URL": "https://x.example", "MMS_HTTP_ENDPOINT": "/token"}, wantErr: "collides"},
		{name: "endpoint under well-known", env: map[string]string{"MMS_PUBLIC_URL": "https://x.example", "MMS_HTTP_ENDPOINT": "/.well-known/x"}, wantErr: "collides"},
		{name: "heartbeat zero disables", mutate: func(f *flagValues) { f.Heartbeat = "0" }, check: func(t *testing.T, c *appConfig) {
			if c.Heartbeat != 0 {
				t.Errorf("heartbeat = %v", c.Heartbeat)
			}
		}},
		{name: "heartbeat custom", mutate: func(f *flagValues) { f.Heartbeat = "1m" }, check: func(t *testing.T, c *appConfig) {
			if c.Heartbeat != time.Minute {
				t.Errorf("heartbeat = %v", c.Heartbeat)
			}
		}},
		{name: "heartbeat invalid", mutate: func(f *flagValues) { f.Heartbeat = "soon" }, wantErr: "http-heartbeat"},
		{name: "heartbeat negative", mutate: func(f *flagValues) { f.Heartbeat = "-5s" }, wantErr: "http-heartbeat"},
		{name: "replay grace", env: map[string]string{"OAUTH_REFRESH_REPLAY_GRACE": "10m"}, check: func(t *testing.T, c *appConfig) {
			if c.RefreshReplayGrace != 10*time.Minute {
				t.Errorf("grace = %v", c.RefreshReplayGrace)
			}
		}},
		{name: "replay grace invalid", env: map[string]string{"OAUTH_REFRESH_REPLAY_GRACE": "x"}, wantErr: "replay-grace"},
		{name: "cors env", env: map[string]string{"MMS_CORS_ORIGIN": "https://a.example"}, check: func(t *testing.T, c *appConfig) {
			if c.CORSOrigin != "https://a.example" {
				t.Errorf("cors = %q", c.CORSOrigin)
			}
		}},
		{name: "cors flag beats env", explicit: []string{"cors-origin"}, env: map[string]string{"MMS_CORS_ORIGIN": "https://a.example"}, mutate: func(f *flagValues) { f.CORSOrigin = "*" },
			check: func(t *testing.T, c *appConfig) {
				if c.CORSOrigin != "*" {
					t.Errorf("cors = %q", c.CORSOrigin)
				}
			}},
		{name: "mcp path follows transport", explicit: []string{"transport"}, mutate: func(f *flagValues) { f.Transport = "sse" }, check: func(t *testing.T, c *appConfig) {
			if c.MCPPath() != "/sse" {
				t.Errorf("path = %q", c.MCPPath())
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fv := defaultFlags()
			if c.mutate != nil {
				c.mutate(&fv)
			}
			explicit := map[string]bool{}
			for _, e := range c.explicit {
				explicit[e] = true
			}
			cfg, err := resolveConfig(explicit, envFrom(c.env), fv)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.check != nil {
				c.check(t, cfg)
			}
		})
	}
}

func TestResolveConfigTrustedProxies(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		flag    string
		want    []string
		wantErr bool
	}{
		{name: "unset"},
		{name: "env", env: map[string]string{"MMS_TRUSTED_PROXIES": "172.20.0.0/16,10.0.0.5"}, want: []string{"172.20.0.0/16", "10.0.0.5/32"}},
		{name: "flag wins over env", env: map[string]string{"MMS_TRUSTED_PROXIES": "10.0.0.5"}, flag: "192.168.0.0/24", want: []string{"192.168.0.0/24"}},
		{name: "invalid env", env: map[string]string{"MMS_TRUSTED_PROXIES": "caddy"}, wantErr: true},
		{name: "invalid flag", flag: "10.0.0.0/99", wantErr: true},
		{name: "empty element", flag: "10.0.0.1,,10.0.0.2", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fv := defaultFlags()
			fv.TrustedProxies = c.flag
			cfg, err := resolveConfig(map[string]bool{}, envFrom(c.env), fv)
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), "trusted proxies") {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range cfg.TrustedProxies {
				got = append(got, p.String())
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestExplicitFlagsCanonicalizesAliases(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	var s string
	var p int
	for _, n := range []string{"transport", "t", "memory", "m", "http-endpoint", "http_ep", "host", "cors-origin"} {
		fs.StringVar(&s, n, "", "")
	}
	for _, n := range []string{"port", "p"} {
		fs.IntVar(&p, n, 8080, "")
	}
	if err := fs.Parse([]string{"-t", "sse", "-p", "9", "--m=x", "-http_ep", "/a"}); err != nil {
		t.Fatal(err)
	}
	got := explicitFlags(fs)
	for _, want := range []string{"transport", "port", "memory", "http-endpoint"} {
		if !got[want] {
			t.Errorf("%s should be explicit: %v", want, got)
		}
	}
	for _, not := range []string{"host", "cors-origin", "t", "p"} {
		if got[not] {
			t.Errorf("%s should not be set: %v", not, got)
		}
	}
}

func TestWarnings(t *testing.T) {
	has := func(c *appConfig, sub string) bool {
		for _, w := range c.Warnings() {
			if strings.Contains(w, sub) {
				return true
			}
		}
		return false
	}
	base := func() *appConfig {
		return &appConfig{Transport: "http", Host: "127.0.0.1", OAuthUser: "admin", OAuthPass: "a-long-enough-pass", OAuthStore: "oauth.db", PublicURL: "https://x.example", Issuer: "https://x.example"}
	}
	if w := base().Warnings(); len(w) != 0 {
		t.Errorf("clean config warned: %v", w)
	}
	if c := (&appConfig{Transport: "stdio", OAuthUser: "a", OAuthPass: "b"}); len(c.Warnings()) != 0 {
		t.Errorf("stdio should not warn")
	}
	c := base()
	c.PublicURL, c.Issuer, c.Host = "", "", ""
	if !has(c, "auto-detected") {
		t.Errorf("expected issuer auto-detect warning: %v", c.Warnings())
	}
	c.Host = "127.0.0.1"
	if has(c, "auto-detected") {
		t.Errorf("loopback bind should not warn about issuer")
	}
	c = base()
	c.Host = "0.0.0.0"
	if !has(c, "trusted proxies") {
		t.Errorf("expected trusted proxies warning: %v", c.Warnings())
	}
	c.Host = ""
	if !has(c, "trusted proxies") {
		t.Errorf("all-interfaces bind should warn about trusted proxies")
	}
	c.TrustedProxies, _ = auth.ParseTrustedProxies("172.20.0.0/16")
	if has(c, "trusted proxies") {
		t.Errorf("configured trusted proxies should not warn")
	}
	c = base()
	if has(c, "trusted proxies") {
		t.Errorf("loopback bind should not warn about trusted proxies")
	}
	c = base()
	c.OAuthPass = "short"
	if !has(c, "shorter than 12") {
		t.Errorf("expected short password warning")
	}
	for _, w := range c.Warnings() {
		if strings.Contains(w, "short") && strings.Contains(w, c.OAuthPass+"\"") {
			t.Errorf("warning leaks the password: %s", w)
		}
	}
	c = base()
	c.OAuthStore = ""
	if !has(c, "in memory") {
		t.Errorf("expected non-durable store warning")
	}
	c = base()
	c.Issuer = "http://mcp.example.com"
	if !has(c, "plain http") {
		t.Errorf("expected plain http warning")
	}
	c.Issuer = "http://localhost:8080"
	if has(c, "plain http") {
		t.Errorf("loopback http should not warn")
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	ok := map[string]string{"/mcp": "/mcp", "/mcp/": "/mcp", "/a/b": "/a/b", " /x ": "/x"}
	for in, want := range ok {
		if got, err := normalizeEndpoint(in); err != nil || got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "mcp", "/", "//", "/healthz", "/healthz/", "/authorize", "/register", "/revoke", "/token", "/.well-known", "/.well-known/x", "/a?b", "/a#b", "/a b", "/{x}"} {
		if got, err := normalizeEndpoint(in); err == nil {
			t.Errorf("normalizeEndpoint(%q) = %q, want error", in, got)
		}
	}
}
