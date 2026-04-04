# OAuth 2.1 Authentication Guide

Memory MCP Server supports OAuth 2.1 authentication, compatible with Claude Desktop Connectors and other MCP clients that implement the OAuth flow.

## Quick Start

```bash
# Start with OAuth authentication
./mms --transport http --oauth-user admin --oauth-pass mypassword --port 8080

# Or use environment variables
export OAUTH_USER=admin
export OAUTH_PASS=mypassword
./mms --transport http --port 8080
```

## Configuration

### CLI Flags

| Flag | Env Variable | Description |
|------|-------------|-------------|
| `--oauth-user` | `OAUTH_USER` | OAuth login username |
| `--oauth-pass` | `OAUTH_PASS` | OAuth login password |
| `--oauth-issuer` | `OAUTH_ISSUER` | Issuer URL (auto-detected from request if empty) |

Both `--oauth-user` and `--oauth-pass` must be provided together to enable OAuth.

### Mutual Exclusion

`--auth-bearer` and OAuth (`--oauth-user`/`--oauth-pass`) are mutually exclusive. The server will exit with an error if both are configured.

### Issuer URL

When `--oauth-issuer` is not set, the server auto-detects the issuer URL from the incoming request's `Host` header and protocol. For production use behind a reverse proxy, set `--oauth-issuer` explicitly:

```bash
./mms --transport http --oauth-user admin --oauth-pass secret \
      --oauth-issuer https://mcp.example.com
```

The server respects the `X-Forwarded-Proto` header for HTTPS detection behind proxies.

## OAuth Flow

The server implements the following OAuth 2.1 endpoints:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/.well-known/oauth-protected-resource` | GET | Protected Resource Metadata (RFC 9728) |
| `/.well-known/oauth-authorization-server` | GET | Authorization Server Metadata (RFC 8414) |
| `/register` | POST | Dynamic Client Registration (RFC 7591) |
| `/authorize` | GET | Display login page |
| `/authorize` | POST | Process login form |
| `/token` | POST | Issue/refresh tokens |

### Flow Steps

1. Client connects to MCP endpoint → receives `401` with `WWW-Authenticate` header
2. Client discovers OAuth metadata via `.well-known` endpoints
3. Client registers dynamically via `/register`
4. Client redirects user to `/authorize` with PKCE challenge
5. User enters username/password on login page
6. Server redirects back with authorization code
7. Client exchanges code for access token via `/token`
8. Client uses access token for subsequent MCP requests
9. Client refreshes token via `/token` when expired

### Token Lifetimes

| Token Type | Lifetime |
|-----------|----------|
| Authorization Code | 10 minutes |
| Access Token | 1 hour |
| Refresh Token | 30 days |

Refresh tokens are rotated on each use (old token is revoked).

## Claude Desktop Integration

1. Start the server with OAuth enabled
2. In Claude Desktop, go to Settings → Connectors → Add Custom Connector
3. Enter the server URL (e.g., `http://localhost:8080/mcp`)
4. Claude Desktop will automatically discover OAuth endpoints and initiate the login flow
5. Enter your configured username and password in the browser popup

## Verification

```bash
# Test 401 response
curl -i http://localhost:8080/mcp

# Check metadata endpoints
curl http://localhost:8080/.well-known/oauth-protected-resource
curl http://localhost:8080/.well-known/oauth-authorization-server

# Register a client
curl -X POST http://localhost:8080/register \
  -H 'Content-Type: application/json' \
  -d '{"client_name":"test","redirect_uris":["http://localhost:3000/cb"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}'
```

## Security Notes

- Credentials are compared using constant-time comparison to prevent timing attacks
- PKCE (S256) is supported for secure authorization code exchange
- All tokens are opaque random strings (not JWT) stored in memory
- Token storage is cleared on server restart (clients must re-authenticate)
- For production use, deploy behind HTTPS (TLS termination via reverse proxy)
