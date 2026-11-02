# OAuth 2.1 Authentication Guide

Memory MCP Server supports OAuth 2.1 authentication, compatible with Claude Desktop Connectors and other MCP clients that implement the OAuth flow. For a ready-made HTTPS deployment (nginx, Let's Encrypt, systemd) see [deploy.md](deploy.md); for Docker, Caddy, Traefik or an existing proxy see [deploy-custom.md](deploy-custom.md).

## Quick Start

```bash
# Behind a reverse proxy: the public URL selects the http transport on 127.0.0.1
# and keeps the OAuth store next to the memory database (oauth.db).
export MMS_PUBLIC_URL=https://mcp.example.com
export MMS_PORT=8080
export OAUTH_USER=admin
export OAUTH_PASS='a-long-random-password'
export MEMORY_FILE_PATH=/var/lib/mms/memory.db
./mms

# Local trial without TLS
./mms --transport http --host 127.0.0.1 --oauth-user admin --oauth-pass mypassword \
      --oauth-store ./oauth.db --port 8080
```

## Configuration

### CLI Flags

| Flag | Env Variable | Description |
|------|-------------|-------------|
| `--oauth-user` | `OAUTH_USER` | OAuth login username |
| `--oauth-pass` | `OAUTH_PASS` | OAuth login password. Prefer the environment variable: command-line arguments are visible in the process list |
| `--public-url` | `MMS_PUBLIC_URL` | Public base URL, e.g. `https://mcp.example.com`. Used as the OAuth issuer and resource |
| `--oauth-issuer` | `OAUTH_ISSUER` | Legacy: sets only the OAuth issuer. Prefer `--public-url` |
| `--oauth-store` | `OAUTH_STORE_PATH` | SQLite store for durable OAuth clients, access tokens, and refresh tokens |
| `--oauth-refresh-replay-grace` | `OAUTH_REFRESH_REPLAY_GRACE` | Duration to accept replay of a rotated refresh token (default `75m`) |
| `--trusted-proxies` | `MMS_TRUSTED_PROXIES` | Comma-separated CIDRs or bare IPs of reverse proxies whose `X-Real-IP` header is trusted for rate limiting. Loopback is always trusted. An invalid value is a fatal startup error |

Both `--oauth-user` and `--oauth-pass` must be provided together to enable OAuth. Flags win over environment variables. Use a password of at least 12 characters; the server warns on shorter ones.

### Mutual Exclusion

`--auth-bearer` and OAuth (`--oauth-user`/`--oauth-pass`) are mutually exclusive. The server will exit with an error if both are configured.

### Public URL and Issuer

Set `--public-url` (or `MMS_PUBLIC_URL`) to the externally visible origin, `scheme://host[:port]` without a path. It is the OAuth issuer, the base of the protected resource identifier (`<public URL>/mcp`), and the only `Host` the server accepts besides loopback addresses: `/mcp` (or `/sse`) and the OAuth routes answer `403` to any other `Host` header. `/healthz` is exempt.

`--oauth-issuer` / `OAUTH_ISSUER` is kept for existing deployments. On its own it sets only the OAuth issuer and resource: transport, bind address and OAuth store keep their usual defaults, and the `Host` restriction does not apply, so upgrading does not change how such a server is reached. Switch to `--public-url` to get those protections. If both are set they must be equal, otherwise the server exits with an error.

When a public URL is set and the matching option is not given explicitly, the server defaults to `--transport http`, `--host 127.0.0.1`, and (with OAuth enabled) `--oauth-store <memory directory>/oauth.db`.

Without a public URL the issuer is auto-detected from the request's `Host` header and the `X-Forwarded-Proto` header. That is only suitable for local use; the server logs a warning when OAuth runs without a public URL on a non-loopback address.

## OAuth Flow

The server implements the following OAuth 2.1 endpoints:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/.well-known/oauth-protected-resource` | GET | Protected Resource Metadata (RFC 9728) |
| `/.well-known/oauth-protected-resource/mcp` | GET | Same document at the path-suffixed location named in the `WWW-Authenticate` challenge |
| `/.well-known/oauth-authorization-server` | GET | Authorization Server Metadata (RFC 8414) |
| `/register` | POST | Dynamic Client Registration (RFC 7591) |
| `/authorize` | GET | Display login page |
| `/authorize` | POST | Process login form |
| `/token` | POST | Issue/refresh tokens |
| `/revoke` | POST | Revoke an access or refresh token (RFC 7009) |

The path-suffixed metadata location follows the MCP endpoint (`--http-endpoint`).

### Flow Steps

1. Client connects to the MCP endpoint and receives `401` with a `WWW-Authenticate: Bearer resource_metadata="...", scope="mcp"` header
2. Client fetches the protected resource metadata, then the authorization server metadata
3. Client identifies itself with a Client ID Metadata Document (an `https` URL as `client_id`) or registers dynamically via `/register`
4. Client sends the user to `/authorize` with a PKCE `S256` challenge and the `resource` parameter
5. User enters username/password on the login page
6. Server redirects back with an authorization code (and `iss`, RFC 9207)
7. Client exchanges the code at `/token` with the PKCE verifier and `resource`
8. Client sends the access token on every MCP request
9. Client refreshes the token via `/token` when it expires; it may revoke tokens via `/revoke`

### Requirements on Clients

- **PKCE with `S256` is mandatory.** Authorization requests without a `code_challenge`, or with another method, are rejected.
- **Resource indicators (RFC 8707).** The `resource` parameter, if sent, must identify this server (`<public URL>/mcp`); anything else is rejected with `invalid_target`.
- **Scope.** The single scope is `mcp`.
- **Public clients only.** The token endpoint authentication method is `none`; there are no client secrets. A registration asking for another method, or for unsupported grant or response types alongside supported ones, succeeds with those values replaced, and the response states what applies.
- **Redirect URIs** must be `https`, a loopback `http` URL, or a custom scheme for native clients (such as `cursor://` or `com.example.app:/cb`). Dangerous schemes such as `javascript:` are refused.

### Client ID Metadata Documents

A `client_id` that is an `https` URL is treated as a Client ID Metadata Document: the server fetches the JSON document to learn the client name and redirect URIs. The fetch is hardened against SSRF (no private or loopback addresses, size and time limits, cached for a short time). Dynamic client registration remains available for clients that do not support metadata documents.

### Token Lifetimes

| Token Type | Lifetime |
|-----------|----------|
| Authorization Code | 10 minutes |
| Access Token | 1 hour |
| Refresh Token | 30 days |

Refresh tokens are rotated on each use.

The server keeps a replay grace window for refresh token rotation. If multiple client workers refresh the same token from stale local cache, later requests receive the same replacement token pair instead of failing with `invalid_grant`. The default is `75m`, which covers the access token lifetime plus clock/process skew. For clients that keep multiple long-lived workers, set a longer value such as `24h`.

### Abuse Protection

- Failed logins are limited per client IP (5 per 15 minutes) and globally (50 per 15 minutes); the login page then answers `429`.
- Dynamic client registrations are limited to 20 per hour per IP.
- Request bodies on the OAuth endpoints are capped at 64 KiB.
- The login form (`POST /authorize`) rejects cross-origin browser submissions.

The client address is the TCP peer, except that `X-Real-IP` is believed when the peer is loopback (a proxy on the same host: nginx, Caddy, cloudflared) or inside `--trusted-proxies` (a proxy in another container or host, for example `--trusted-proxies 172.20.0.0/16`). From any other peer the header is ignored, so it cannot be forged to dodge or poison the limits. An unparsable header falls back to the peer; `X-Forwarded-For` is never used. Your proxy must overwrite `X-Real-IP` with the real client address. Without `--trusted-proxies`, a proxy in another container or host makes every user share the proxy's address, and five failed logins lock out everybody; the server logs a warning when OAuth is enabled on a non-loopback bind with none configured. The deployment script's nginx configuration also rate limits these endpoints.

### Durable Token Storage

Without an OAuth store, clients and tokens are held in memory and are lost on restart; every client then has to log in again. With a public URL the store defaults to `oauth.db` next to the memory database. Otherwise set one explicitly:

```bash
./mms --transport http \
  --oauth-user admin \
  --oauth-pass secret \
  --public-url https://mcp.example.com \
  --oauth-store /var/lib/mms/oauth.db \
  --oauth-refresh-replay-grace 24h \
  --host 127.0.0.1
```

**Tokens are never stored in plaintext.** Access and refresh tokens are persisted (and held in memory) only as SHA-256 hashes, so a stolen `oauth.db` or backup contains no usable token. The replacement pair kept for the refresh replay grace window is encrypted with AES-256-GCM under a key derived (HKDF-SHA256) from the old refresh token, so only the holder of that old token, which is exactly the client replay is meant for, can recover it. Deleted rows are overwritten (`secure_delete`).

**Upgrading is automatic and one-way.** On first start the server converts a legacy plaintext store in a single transaction (all-or-nothing; a failure aborts startup and leaves the file untouched), then checkpoints the WAL and runs `VACUUM` so the old plaintext does not linger in the file. Logged-in clients keep working, including replay of just-rotated tokens. A store written by a newer schema version is refused with an error. An older binary cannot read the upgraded store: on downgrade, restore a pre-upgrade backup, or delete `oauth.db` (clients then have to log in again).

The OAuth store file is forced to `0600` when opened. For systemd deployments, also set `UMask=0077` so SQLite sidecar files such as `oauth.db-wal` and `oauth.db-shm` are created with private permissions.

## Claude Desktop Integration

1. Start the server with OAuth enabled
2. In Claude Desktop, go to Settings → Connectors → Add Custom Connector
3. Enter the server URL (e.g., `https://mcp.example.com/mcp`)
4. Claude Desktop will automatically discover OAuth endpoints and initiate the login flow
5. Enter your configured username and password in the browser popup

## Verification

```bash
# Test 401 response and the challenge header
curl -i -X POST http://localhost:8080/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

# Check metadata endpoints
curl http://localhost:8080/.well-known/oauth-protected-resource/mcp
curl http://localhost:8080/.well-known/oauth-authorization-server

# Register a client
curl -X POST http://localhost:8080/register \
  -H 'Content-Type: application/json' \
  -d '{"client_name":"test","redirect_uris":["http://localhost:3000/cb"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}'
```

## Security Notes

- Credentials are compared using constant-time comparison to prevent timing attacks
- PKCE (S256) is required for the authorization code exchange
- All tokens are opaque random strings (not JWT)
- Use an OAuth store in production (the default with a public URL) to persist clients and tokens across restarts
- Use a restrictive process umask such as systemd `UMask=0077` for OAuth SQLite sidecar files
- Refresh token rotation includes a configurable replay grace window for concurrent or stale-worker refresh attempts
- Bind HTTP/SSE transports to localhost behind a reverse proxy with `--host 127.0.0.1` (the default when a public URL is set)
- Requests are accepted only for the public host and loopback hosts, which also defeats DNS rebinding
- For production use, deploy behind HTTPS (TLS termination via reverse proxy); the server warns about a plain `http` public URL on a non-loopback host
