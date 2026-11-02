// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"memory-mcp-server-go/auth"
)

const (
	testPublicURL  = "https://mcp.example.com"
	testPublicHost = "mcp.example.com"
	initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`
)

// headerRT rewrites the Host header and adds a bearer token to every request.
type headerRT struct {
	host  string
	token string
}

func (h headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if h.host != "" {
		req.Host = h.host
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func newHTTPTestServer(t *testing.T, cfg httpConfig) *httptest.Server {
	t.Helper()
	if cfg.Server == nil {
		cfg.Server = newMCPServer(newTestManager(t))
	}
	h, err := newHTTPHandler(cfg)
	if err != nil {
		t.Fatalf("newHTTPHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newTestOAuth(t *testing.T, issuer string) *auth.OAuthServer {
	t.Helper()
	o, err := auth.NewOAuthServerWithError(auth.Config{
		Username:     "admin",
		Password:     "correct horse battery staple",
		Issuer:       issuer,
		ResourcePath: "/mcp",
		StorePath:    filepath.Join(t.TempDir(), "oauth.db"),
	})
	if err != nil {
		t.Fatalf("NewOAuthServerWithError: %v", err)
	}
	t.Cleanup(o.Close)
	return o
}

// rawRequest sends a request and returns the response with its body read.
func rawRequest(t *testing.T, client *http.Client, method, target, host string, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func mcpPost(t *testing.T, base, host, token string) *http.Response {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	resp, _ := rawRequest(t, nil, http.MethodPost, base+"/mcp", host, initializeBody, headers)
	return resp
}

// connectHTTP connects a real SDK client over Streamable HTTP.
func connectHTTP(t *testing.T, endpoint, host, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "http-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: headerRT{host: host, token: token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestOAuthEndToEnd(t *testing.T) {
	oauth := newTestOAuth(t, testPublicURL)
	srv := newHTTPTestServer(t, httpConfig{OAuth: oauth, PublicURL: testPublicURL})

	// A fresh client talks to the loopback listener but, like a real client
	// behind a reverse proxy, always sends the public Host.
	client := &http.Client{
		Transport:     headerRT{host: testPublicHost},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	local := func(raw string) string {
		if !strings.HasPrefix(raw, testPublicURL) {
			t.Fatalf("unexpected non-public URL %q", raw)
		}
		return srv.URL + strings.TrimPrefix(raw, testPublicURL)
	}
	getJSON := func(target string) map[string]any {
		resp, body := rawRequest(t, client, http.MethodGet, local(target), "", "", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d: %s", target, resp.StatusCode, body)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("GET %s: bad JSON %q", target, body)
		}
		return m
	}

	// 1. Unauthenticated MCP request: 401 with a resource_metadata pointer.
	resp, _ := rawRequest(t, client, http.MethodPost, srv.URL+"/mcp", "", initializeBody,
		map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /mcp = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	m := regexp.MustCompile(`resource_metadata="([^"]+)"`).FindStringSubmatch(challenge)
	if m == nil {
		t.Fatalf("WWW-Authenticate lacks resource_metadata: %q", challenge)
	}
	if m[1] != testPublicURL+"/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("resource_metadata = %q", m[1])
	}
	if !strings.Contains(challenge, `scope="mcp"`) {
		t.Errorf("challenge lacks scope: %q", challenge)
	}

	// 2. Protected resource metadata -> authorization server metadata.
	prm := getJSON(m[1])
	if prm["resource"] != testPublicURL+"/mcp" {
		t.Errorf("resource = %v", prm["resource"])
	}
	servers, _ := prm["authorization_servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("authorization_servers = %v", prm["authorization_servers"])
	}
	asm := getJSON(servers[0].(string) + "/.well-known/oauth-authorization-server")
	for _, k := range []string{"authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		if s, _ := asm[k].(string); !strings.HasPrefix(s, testPublicURL+"/") {
			t.Fatalf("AS metadata %s = %v", k, asm[k])
		}
	}

	// 3. Dynamic client registration.
	redirect := "http://127.0.0.1:45678/callback"
	regBody, _ := json.Marshal(map[string]any{
		"client_name":                "e2e",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	resp, body := rawRequest(t, client, http.MethodPost, local(asm["registration_endpoint"].(string)), "", string(regBody),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d: %s", resp.StatusCode, body)
	}
	var reg map[string]any
	_ = json.Unmarshal([]byte(body), &reg)
	clientID := reg["client_id"].(string)

	// 4. Authorization request (login page), PKCE + resource indicator.
	verifier := "e2e-code-verifier-with-plenty-of-entropy-0123456789-abcdef"
	sum := sha256.Sum256([]byte(verifier))
	challengeS256 := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challengeS256},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz"},
		"scope":                 {"mcp"},
		"resource":              {testPublicURL + "/mcp"},
	}
	resp, page := rawRequest(t, client, http.MethodGet, local(asm["authorization_endpoint"].(string))+"?"+q.Encode(), "", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(page, "<form") {
		t.Fatalf("authorize GET = %d: %s", resp.StatusCode, page)
	}

	// 5. Log in: post the page's hidden fields plus credentials.
	form := url.Values{"username": {"admin"}, "password": {"correct horse battery staple"}}
	for _, hm := range regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)"`).FindAllStringSubmatch(page, -1) {
		form.Set(hm[1], html.UnescapeString(hm[2]))
	}
	resp, page = rawRequest(t, client, http.MethodPost, local(asm["authorization_endpoint"].(string)), "", form.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != 200 {
		t.Fatalf("authorize POST = %d: %s", resp.StatusCode, page)
	}
	cm := regexp.MustCompile(`code=([0-9a-f]{64})`).FindStringSubmatch(html.UnescapeString(page))
	if cm == nil {
		t.Fatalf("no authorization code in success page:\n%s", page)
	}

	// 6. Token exchange with PKCE and resource.
	tok := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cm[1]},
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
		"resource":      {testPublicURL + "/mcp"},
	}
	resp, body = rawRequest(t, client, http.MethodPost, local(asm["token_endpoint"].(string)), "", tok.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != 200 {
		t.Fatalf("token = %d: %s", resp.StatusCode, body)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal([]byte(body), &tr)
	if tr.AccessToken == "" {
		t.Fatalf("no access token: %s", body)
	}

	// 7. MCP over the public Host with the token: initialize + tools/call.
	cs := connectHTTP(t, srv.URL+"/mcp", testPublicHost, tr.AccessToken)
	text := mustCall(t, cs, "create_entities", map[string]any{"entities": []map[string]any{
		{"name": "ViaOAuth", "entityType": "test", "observations": []string{"works"}},
	}})
	if !strings.Contains(text, "ViaOAuth") {
		t.Errorf("create_entities result: %s", text)
	}

	// 8. Negative cases.
	if resp := mcpPost(t, srv.URL, testPublicHost, "not-a-token"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("invalid bearer = %d, want 401", resp.StatusCode)
	}
	for _, path := range []string{"/mcp", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/authorize", "/register"} {
		resp, _ := rawRequest(t, nil, http.MethodGet, srv.URL+path, "evil.example.net", "", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s with foreign Host = %d, want 403", path, resp.StatusCode)
		}
	}
	if resp := mcpPost(t, srv.URL, "evil.example.net", tr.AccessToken); resp.StatusCode != http.StatusForbidden {
		t.Errorf("valid token with foreign Host = %d, want 403", resp.StatusCode)
	}
	// Loopback hosts are accepted (local health checks, tests) and still need a token.
	if resp := mcpPost(t, srv.URL, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("loopback Host without token = %d, want 401", resp.StatusCode)
	}
	// /healthz is exempt from the Host guard and from auth.
	resp, body = rawRequest(t, nil, http.MethodGet, srv.URL+"/healthz", "evil.example.net", "", nil)
	if resp.StatusCode != 200 || body != "ok" {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
	}
}

func TestHealthz(t *testing.T) {
	srv := newHTTPTestServer(t, httpConfig{AuthBearer: "secret-token", CORSOrigin: "https://app.example"})
	resp, body := rawRequest(t, nil, http.MethodGet, srv.URL+"/healthz", "", "", map[string]string{"Origin": "https://other.example"})
	if resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("healthz must not carry CORS headers")
	}
	if resp, _ := rawRequest(t, nil, http.MethodPost, srv.URL+"/healthz", "", "", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", resp.StatusCode)
	}
}

func TestBearerAuth(t *testing.T) {
	const token = "s3cret-bearer-token"
	srv := newHTTPTestServer(t, httpConfig{AuthBearer: token})

	cs := connectHTTP(t, srv.URL+"/mcp", "", token)
	if out := mustCall(t, cs, "read_graph", map[string]any{}); out == "" {
		t.Errorf("empty read_graph result")
	}

	resp := mcpPost(t, srv.URL, "", "wrong-token")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("wrong token = %d, WWW-Authenticate=%q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	resp = mcpPost(t, srv.URL, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d", resp.StatusCode)
	}
	// Token prefix/extension must not match.
	for _, bad := range []string{token + "x", token[:len(token)-1]} {
		if resp := mcpPost(t, srv.URL, "", bad); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q = %d", bad, resp.StatusCode)
		}
	}
	// A bearer-protected server does not use the DNS-rebinding trap: any Host with a valid token works.
	if resp := mcpPost(t, srv.URL, "mcp.example.com", token); resp.StatusCode != http.StatusOK {
		t.Errorf("bearer with proxy Host = %d, want 200", resp.StatusCode)
	}
}

func TestCORS(t *testing.T) {
	t.Run("preflight bypasses auth", func(t *testing.T) {
		srv := newHTTPTestServer(t, httpConfig{AuthBearer: "tok"})
		resp, _ := rawRequest(t, nil, http.MethodOptions, srv.URL+"/mcp", "", "", map[string]string{
			"Origin": "https://app.example", "Access-Control-Request-Method": "POST",
		})
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("preflight = %d, ACAO=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
	})
	t.Run("origin allow list", func(t *testing.T) {
		srv := newHTTPTestServer(t, httpConfig{CORSOrigin: "https://a.example, https://b.example"})
		resp, _ := rawRequest(t, nil, http.MethodOptions, srv.URL+"/mcp", "", "", map[string]string{"Origin": "https://b.example"})
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "https://b.example" {
			t.Errorf("allowed origin = %d, ACAO=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
		resp, _ = rawRequest(t, nil, http.MethodOptions, srv.URL+"/mcp", "", "", map[string]string{"Origin": "https://evil.example"})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("disallowed origin = %d, want 403", resp.StatusCode)
		}
		// The OAuth login form posts back to its own origin, which is never
		// on the allow list.
		resp, _ = rawRequest(t, nil, http.MethodOptions, srv.URL+"/mcp", "", "", map[string]string{"Origin": srv.URL})
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("same origin = %d, want 204", resp.StatusCode)
		}
	})
	t.Run("oauth routes get CORS", func(t *testing.T) {
		srv := newHTTPTestServer(t, httpConfig{OAuth: newTestOAuth(t, "")})
		resp, _ := rawRequest(t, nil, http.MethodGet, srv.URL+"/.well-known/oauth-authorization-server", "", "", nil)
		if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("AS metadata = %d, ACAO=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
	})
}

func TestLocalhostProtection(t *testing.T) {
	cases := []struct {
		name      string
		cfg       httpConfig
		host      string
		token     string
		wantCode  int
		wantTrace string
	}{
		{"open loopback, loopback host", httpConfig{}, "", "", 200, ""},
		{"open loopback, foreign host is blocked (DNS rebinding)", httpConfig{}, "evil.example.net", "", 403, ""},
		{"open loopback, 127.0.0.2 host ok", httpConfig{}, "127.0.0.2", "", 200, ""},
		{"public URL, public host", httpConfig{PublicURL: testPublicURL}, testPublicHost, "", 200, ""},
		{"public URL, foreign host", httpConfig{PublicURL: testPublicURL}, "evil.example.net", "", 403, ""},
		{"public URL with port", httpConfig{PublicURL: "https://mcp.example.com:8443"}, "mcp.example.com:8443", "", 200, ""},
		{"public URL with port, bare host", httpConfig{PublicURL: "https://mcp.example.com:8443"}, "mcp.example.com", "", 403, ""},
		{"public URL, case-insensitive host", httpConfig{PublicURL: testPublicURL}, "MCP.Example.COM", "", 200, ""},
		{"bearer, proxied host with token", httpConfig{AuthBearer: "tok"}, "anything.example.net", "tok", 200, ""},
		{"bearer, proxied host without token", httpConfig{AuthBearer: "tok"}, "anything.example.net", "", 401, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newHTTPTestServer(t, tc.cfg)
			resp := mcpPost(t, srv.URL, tc.host, tc.token)
			if resp.StatusCode != tc.wantCode {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
		})
	}
}

func TestClientResponseAccepted(t *testing.T) {
	// Regression for 1e237ea: a JSON-RPC *response* POSTed by the client (for
	// example the reply to a server ping) must get 202 Accepted with no body in
	// a stateful Streamable HTTP session.
	srv := newHTTPTestServer(t, httpConfig{})
	hdr := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}

	resp, _ := rawRequest(t, nil, http.MethodPost, srv.URL+"/mcp", "", initializeBody, hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize = %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("stateful server did not issue Mcp-Session-Id")
	}
	hdr["Mcp-Session-Id"] = sid
	hdr["Mcp-Protocol-Version"] = "2025-06-18"

	resp, body := rawRequest(t, nil, http.MethodPost, srv.URL+"/mcp", "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, hdr)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized notification = %d %s", resp.StatusCode, body)
	}

	for name, payload := range map[string]string{
		"ping result": `{"jsonrpc":"2.0","id":"server-ping-1","result":{}}`,
		"error reply": `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"nope"}}`,
	} {
		resp, body = rawRequest(t, nil, http.MethodPost, srv.URL+"/mcp", "", payload, hdr)
		if resp.StatusCode != http.StatusAccepted || body != "" {
			t.Errorf("%s: status = %d body = %q, want 202 and empty body", name, resp.StatusCode, body)
		}
	}
}

func TestServerPingKeepsSessionAlive(t *testing.T) {
	// The server pings the client every 150ms; the client's replies are POSTed
	// responses. The session must survive many pings.
	s := newMCPServerWithKeepAlive(newTestManager(t), 150*time.Millisecond)
	srv := newHTTPTestServer(t, httpConfig{Server: s})
	cs := connectHTTP(t, srv.URL+"/mcp", "", "")
	time.Sleep(time.Second)
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("session died while being pinged: %v", err)
	}
}

func TestStatelessMode(t *testing.T) {
	srv := newHTTPTestServer(t, httpConfig{Stateless: true})
	resp, _ := rawRequest(t, nil, http.MethodPost, srv.URL+"/mcp", "", initializeBody,
		map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
	if resp.StatusCode != 200 || resp.Header.Get("Mcp-Session-Id") != "" {
		t.Errorf("stateless initialize = %d, session id %q", resp.StatusCode, resp.Header.Get("Mcp-Session-Id"))
	}
}

func TestCustomEndpoint(t *testing.T) {
	srv := newHTTPTestServer(t, httpConfig{Endpoint: "/api/memory/"})
	cs := connectHTTP(t, srv.URL+"/api/memory", "", "")
	mustCall(t, cs, "read_graph", map[string]any{})
	if resp, _ := rawRequest(t, nil, http.MethodGet, srv.URL+"/mcp", "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("default path should be absent, got %d", resp.StatusCode)
	}
}

func TestSSETransport(t *testing.T) {
	const token = "sse-token"
	srv := newHTTPTestServer(t, httpConfig{Transport: "sse", AuthBearer: token})

	if resp, _ := rawRequest(t, nil, http.MethodGet, srv.URL+"/sse", "", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /sse = %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "sse-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.SSEClientTransport{
		Endpoint:   srv.URL + "/sse",
		HTTPClient: &http.Client{Transport: headerRT{token: token}},
	}, nil)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	mustCall(t, cs, "create_entities", map[string]any{"entities": []map[string]any{
		{"name": "ViaSSE", "entityType": "test", "observations": []string{"ok"}},
	}})
	if out := mustCall(t, cs, "open_nodes", map[string]any{"names": []string{"ViaSSE"}}); !strings.Contains(out, "ViaSSE") {
		t.Errorf("open_nodes: %s", out)
	}
	if resp, _ := rawRequest(t, nil, http.MethodPost, srv.URL+"/mcp", "", initializeBody, map[string]string{"Authorization": "Bearer " + token}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/mcp on an SSE server = %d, want 404", resp.StatusCode)
	}
}

func TestNewHTTPHandlerValidation(t *testing.T) {
	s := newMCPServer(newTestManager(t))
	oauth := newTestOAuth(t, "")
	cases := []struct {
		name string
		cfg  httpConfig
	}{
		{"no server", httpConfig{}},
		{"oauth and bearer", httpConfig{Server: s, OAuth: oauth, AuthBearer: "x"}},
		{"bad transport", httpConfig{Server: s, Transport: "stdio"}},
		{"endpoint without slash", httpConfig{Server: s, Endpoint: "mcp"}},
		{"endpoint healthz", httpConfig{Server: s, Endpoint: "/healthz"}},
		{"endpoint token", httpConfig{Server: s, Endpoint: "/token"}},
		{"endpoint well-known", httpConfig{Server: s, Endpoint: "/.well-known/mcp"}},
		{"endpoint root", httpConfig{Server: s, Endpoint: "/"}},
		{"bad public url", httpConfig{Server: s, PublicURL: "ftp://x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newHTTPHandler(tc.cfg); err == nil {
				t.Errorf("expected an error")
			}
		})
	}
	for _, tr := range []string{"", "http", "streamable-http", "sse"} {
		if _, err := newHTTPHandler(httpConfig{Server: s, Transport: tr}); err != nil {
			t.Errorf("transport %q: %v", tr, err)
		}
	}
}

func TestPublicHosts(t *testing.T) {
	cases := []struct {
		public string
		host   string
		want   bool
	}{
		{"https://mcp.example.com", "mcp.example.com", true},
		{"https://mcp.example.com", "mcp.example.com:443", true},
		{"https://mcp.example.com", "mcp.example.com:8443", false},
		{"https://mcp.example.com:8443", "mcp.example.com:8443", true},
		{"http://mcp.example.com", "mcp.example.com:80", true},
		{"https://mcp.example.com", "mcp.example.org", false},
		{"https://mcp.example.com", "evil.com", false},
		{"https://[2001:db8::1]", "[2001:db8::1]", true},
	}
	for _, tc := range cases {
		u, _ := url.Parse(tc.public)
		if got := publicHosts(u)[strings.ToLower(tc.host)]; got != tc.want {
			t.Errorf("public %s host %s = %v, want %v", tc.public, tc.host, got, tc.want)
		}
	}
	for host, want := range map[string]bool{
		"localhost": true, "localhost:8080": true, "127.0.0.1": true, "127.0.0.1:1": true,
		"[::1]": true, "[::1]:8080": true, "foo.localhost": true,
		"example.com": false, "127.0.0.1.example.com": false, "10.0.0.1": false, "": false,
	} {
		if got := isLoopbackHostHeader(host); got != want {
			t.Errorf("isLoopbackHostHeader(%q) = %v, want %v", host, got, want)
		}
	}
}
