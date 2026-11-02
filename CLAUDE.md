# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Go implementation of a Memory MCP (Model Context Protocol) server that provides persistent knowledge graph storage functionality. The server has been upgraded to use a layered storage architecture supporting both SQLite and JSONL backends, with automatic migration capabilities.

## Build Commands

```bash
# Build for current platform (recommended)
make build

# Run tests
go test ./...

# Build for all platforms (cross-compile)
make build-all

# Clean build artifacts
make clean

# Create distribution packages
make dist
```

## Development Workflow

### Project Architecture

1. **Layered Architecture**: Storage abstraction with pluggable backends
2. **Storage Backends**: SQLite (preferred) and JSONL (legacy)
3. **Auto-Migration**: Automatic upgrade from JSONL to SQLite
4. **Version**: Managed via `VERSION` file at repo root
5. Build outputs go to `.build/` directory

### Local Verification Checklist

**IMPORTANT**: After completing any code changes, you MUST run the verification sequence before finishing. This ensures CI will pass when pushed to GitHub.

Before pushing any changes, run this verification sequence:

```bash
# Complete verification (one-liner)
make fmt && make check && go test ./... && make build

# Or step by step:
make fmt          # Format code (auto-fix)
make vet          # Static analysis
go mod tidy       # Sync dependencies
go test ./...     # Run all tests
make build        # Verify build
```

### CI Pipeline Mapping

| Local Command | CI Step | Purpose |
|---------------|---------|---------|
| `make deps` | Dependencies | Download Go modules |
| `go mod tidy` | Tidy check | Ensure no uncommitted module changes |
| `make check` | Lint | Formatting (`gofmt -s`) + `go vet` |
| `make build` | Build | Compile for current platform |

### Troubleshooting CI Failures

| Failure | Cause | Fix |
|---------|-------|-----|
| "files need formatting" | Code not formatted | Run `make fmt` and re-commit |
| "go.mod/go.sum changed" | Dependencies out of sync | Run `go mod tidy` and commit changes |
| "go vet" errors | Static analysis issues | Fix reported issues in code |
| Build failure | Syntax or import errors | Check compiler output, fix errors |

## Testing

Tests are available in `storage/search_priority_test.go`, `auth/oauth_test.go` and the root package:

- Search priority tests (name > type > content match ranking)
- Storage interface compatibility tests
- `e2e_test.go`: every tool/resource/prompt through a real SDK client (in-memory transport)
- `http_test.go`: HTTP end-to-end (OAuth flow driven over HTTP, bearer, Host guard, DNS-rebinding protection, SSE, 202 for client responses)
- `config_test.go`: flag/env/public-URL config derivation
- Run with `go test -v ./...`

## Architecture

### Storage Layer (storage/ directory)

1. **Storage Interface** (storage/interface.go):
   - `Storage` interface for pluggable backends
   - `Config` for storage configuration
   - Factory function `NewStorage()`

2. **SQLite Storage** (storage/sqlite.go):
   - High-performance database backend
   - WAL mode, caching, busy timeout support
   - ACID transactions for data integrity
   - Foreign key constraints and cascading deletes

3. **FTS5 Search** (storage/sqlite_fts.go):
   - Full-text search using SQLite FTS5
   - Auto-fallback to LIKE-based search
   - Search suggestions and analytics
   - BM25 ranking for relevance

4. **JSONL Storage** (storage/jsonl.go):
   - Legacy format compatibility
   - Simple file-based persistence
   - Used for migration source

5. **Migration Tool** (storage/migration.go):
   - Automatic JSONL to SQLite migration
   - Batch processing for large datasets
   - Verification and rollback support
   - Progress reporting

### OAuth Layer (auth/ directory)

1. **OAuth Server** (auth/oauth.go):
   - OAuth 2.1 authorization server for MCP
   - Config, NewOAuthServer(), NewOAuthServerWithError(), RegisterRoutes(), Middleware(), Close()
   - Issuer URL auto-detection from request or explicit config

2. **HTTP Handlers** (auth/handlers.go):
   - `/.well-known/oauth-protected-resource` — Protected Resource Metadata (RFC 9728)
   - `/.well-known/oauth-authorization-server` — Authorization Server Metadata (RFC 8414)
   - `/register` — Dynamic Client Registration (RFC 7591)
   - `/authorize` GET/POST — Login page + form submission
   - `/token` — Token issuance (authorization_code) and refresh (refresh_token)

3. **OAuth Store** (auth/store.go, auth/persistent_store.go):
   - ClientStore, CodeStore, TokenStore with sync.RWMutex
   - Optional SQLite-backed persistence via `--oauth-store` / `OAUTH_STORE_PATH`
   - Persistent OAuth store files are chmodded to `0600`; systemd deployments should also use `UMask=0077`
   - Refresh token rotation includes a configurable replay grace window for concurrent or stale-worker clients
   - Rate-limit client key: `X-Real-IP` is believed only from loopback or `--trusted-proxies` / `MMS_TRUSTED_PROXIES` peers (`auth.ParseTrustedProxies`); `X-Forwarded-For` is never used
   - Periodic cleanup goroutine (every 15 minutes)

4. **Token & PKCE** (auth/token.go):
   - Opaque token generation (crypto/rand + hex)
   - PKCE S256 verification (sha256 + base64url + constant-time compare)

5. **Login Page** (auth/login.html):
   - Embedded HTML template (go:embed)
   - English labels, card-style layout, error display

### Core Components

1. **KnowledgeGraphManager** (`main.go`, type `KnowledgeGraphManager`):
   - Uses storage abstraction layer
   - Auto-detection of storage type
   - Backward compatibility with legacy API

2. **MCP server** (`mcp_server.go`, `newMCPServer`), built on the official `github.com/modelcontextprotocol/go-sdk`:
   - Tools (typed `mcp.AddTool` with hand-written input schemas; handler errors surface as tool errors with `IsError=true`): create_entities, create_relations, add_observations, delete_entities, delete_relations, delete_observations, read_graph, search_nodes, open_nodes, merge_entities, update_entities, update_observations, detect_conflicts
   - Resources `memory://graph/summary`, `memory://graph/types`, template `memory://entities/{name}`; prompts `memory-recall`, `memory-save`, `memory-review`
   - Receiving middleware converts handler panics into errors and strips null-valued arguments before schema validation

3. **HTTP layer** (`http_server.go`, `newHTTPHandler`): Streamable HTTP (`--transport http`, alias `streamable-http`) or SSE (`/sse` only; the old `/message` path is gone), `GET /healthz`, CORS, Bearer/OAuth auth, and a Host guard when a public URL is set. The SDK's loopback DNS-rebinding protection stays on only for unauthenticated loopback listeners without a public URL. The SDK handles client JSON-RPC responses (202) itself.

4. **Configuration** (`config.go`, `resolveConfig`): merges flags and env (flags win) and derives defaults from `--public-url` / `MMS_PUBLIC_URL` (http transport, host 127.0.0.1, OAuth store `oauth.db` next to the memory database). Deployment: `scripts/deploy-vps.sh`, `doc/deploy.md`. **For AI agents** deploying without the script (Docker, Caddy, Traefik, existing nginx, bearer-only): follow `doc/deploy-custom.md` and run `scripts/verify-deploy.sh`.

### Command-line Arguments

```bash
./mms [options]
  -t, --transport string     stdio, http (alias streamable-http) or sse (env: MMS_TRANSPORT; default stdio, http when a public URL is set)
  -m, --memory string        Memory file path
  -p, --port int            Port for http/sse (env: MMS_PORT, default 8080)
  --host string             Host/IP to bind for http/sse (env: MMS_HOST; 127.0.0.1 when a public URL is set)
  -v, --version             Show version
  -h, --help               Show help

  # Storage options
  --storage string          Storage type: sqlite or jsonl (auto-detected)
  --auto-migrate           Automatically migrate from JSONL to SQLite (default true)

  # Migration commands
  --migrate string         Migrate data from JSONL file to SQLite
  --migrate-to string      Destination SQLite file for migration
  --dry-run               Perform a dry run of migration
  --force                 Force overwrite destination file

  # Streamable HTTP
  --http-endpoint string   Endpoint path (env: MMS_HTTP_ENDPOINT, default /mcp)
  --http-heartbeat string  Keepalive ping interval, 0 disables (default 0)
  --http-stateless         Stateless mode

  # Public URL, auth & CORS
  --public-url string      Public base URL (env: MMS_PUBLIC_URL); OAuth issuer, Host guard, deployment defaults
  --auth-bearer string     Require Bearer token for http/sse (env: MMS_AUTH_BEARER)
  --oauth-user string      OAuth login username (env: OAUTH_USER)
  --oauth-pass string      OAuth login password (env: OAUTH_PASS; prefer env)
  --oauth-issuer string    Legacy: set only the OAuth issuer, with no other --public-url effects (env: OAUTH_ISSUER)
  --oauth-store string     SQLite path for durable OAuth clients and tokens (env: OAUTH_STORE_PATH)
  --oauth-refresh-replay-grace string
                         Duration to accept replay of a rotated refresh token (env: OAUTH_REFRESH_REPLAY_GRACE, default 75m)
  --trusted-proxies string Comma-separated CIDRs/IPs of proxies whose X-Real-IP is trusted for rate limiting (env: MMS_TRUSTED_PROXIES); loopback always trusted
  --cors-origin string     Allowed CORS origins: '*' for all, or comma-separated list (env: MMS_CORS_ORIGIN, default "*")
```

### Storage Type Detection

1. **SQLite**: `.db`, `.sqlite`, `.sqlite3` extensions
2. **Auto-migration**: JSONL files are auto-migrated to SQLite when `--auto-migrate=true`
3. **Fallback**: JSONL for backward compatibility

## Key Dependencies

- `github.com/modelcontextprotocol/go-sdk v1.8.0` - official MCP SDK for Go (also used by `auth/`)
- `modernc.org/sqlite` - pure Go SQLite driver

## Migration

### Automatic Migration

- Enabled by default with `--auto-migrate=true`
- Detects existing JSONL files and migrates to SQLite
- Creates backup of original JSONL file

### Manual Migration

```bash
# Migrate specific file
./mms --migrate=old_data.json --migrate-to=new_data.db

# Dry run to check migration
./mms --migrate=old_data.json --dry-run

# Force overwrite existing destination
./mms --migrate=old_data.json --force
```

## Performance Optimizations

1. **SQLite Configuration**:
   - WAL mode for better concurrency
   - Configurable cache size (default: 10000 pages)
   - Busy timeout handling

2. **Search Features**:
   - FTS5 full-text search with BM25 ranking
   - Automatic fallback to LIKE-based search
   - Search suggestions and analytics
   - **Name Priority Ranking**: Results sorted by match location
     - Name exact match (priority: 100)
     - Name partial match (priority: 80)
     - Entity type match (priority: 50)
     - Observations content match (priority: 20)

3. **Database Schema**:
   - Proper indexing on frequently queried columns
   - Foreign key constraints for data integrity
   - Optimized queries with prepared statements

## Release & Distribution

- **Binary name**: `mms` (short for Memory MCP Server)
- **GoReleaser**: `.goreleaser.yaml` handles cross-compilation, archive creation, changelog, and Homebrew formula
- **Homebrew**: `brew install okooo5km/tap/mms` — formula auto-published to `okooo5km/homebrew-tap` via GoReleaser
- **Release workflow**: Push a `v*` tag to trigger `.github/workflows/release.yml` (GoReleaser-based)
- **Install script**: `scripts/install.sh` downloads GoReleaser archives (`mms_VERSION_os_arch.tar.gz`)

## Important Notes

1. **No CGO**: the SQLite driver is pure Go (`modernc.org/sqlite`); builds and GoReleaser use `CGO_ENABLED=0`
2. **Backward Compatibility**: All existing APIs remain unchanged
3. **Data Safety**: Automatic backups during migration
4. **Performance**: 10x+ improvement with SQLite backend
5. **Concurrent Access**: WAL mode supports multiple readers, single writer
