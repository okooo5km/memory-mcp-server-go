# Memory MCP Server (Go)

> A Model Context Protocol server that provides knowledge graph management capabilities. This server enables LLMs to create, read, update, and delete entities and relations in a persistent knowledge graph, helping AI assistants maintain memory across conversations. This is a Go implementation of the official [TypeScript Memory MCP Server](https://github.com/modelcontextprotocol/servers/tree/main/src/memory).

![Go Platform](https://img.shields.io/badge/platform-cross--platform-lightgrey)
![License](https://img.shields.io/badge/license-MIT-blue)

## Features

* **High-Performance Storage**: SQLite backend with WAL mode, read/write connection separation for concurrent access
* **Knowledge Graph Management**: Persistent graph of entities, relationships, and observations
* **Advanced Search**: FTS5 full-text search with BM25 ranking, synonym expansion, and time-decay scoring
* **Graph Traversal**: Search results include 1-hop related entities for richer context
* **Entity Management**: Merge duplicate entities, update types, modify observations, detect conflicts
* **Observation Metadata**: Track source, confidence, and tags for each observation
* **MCP Resources & Prompts**: AI clients can passively load graph summaries and use guided memory workflows
* **Flexible Transport**: Supports stdio, SSE, and Streamable HTTP with optional Bearer or OAuth 2.1 authentication
* **Seamless Migration**: Automatic upgrade from JSONL to SQLite with zero intervention
* **Cross-Platform**: Pure Go SQLite (no CGO required), works on Linux, macOS, and Windows

## Available Tools

### Core CRUD

| Tool | Description |
|------|-------------|
| `create_entities` | Create new entities with name, type, and observations |
| `create_relations` | Create relations between entities (active voice) |
| `add_observations` | Add observations to existing entities |
| `delete_entities` | Delete entities and their associated relations |
| `delete_relations` | Delete specific relations |
| `delete_observations` | Delete specific observations from entities |

### Query

| Tool | Description |
|------|-------------|
| `search_nodes` | Search entities by keyword with FTS5, synonym expansion, and graph traversal. Returns lightweight results with snippets and related entities |
| `open_nodes` | Get full details of specific entities by exact name |
| `read_graph` | Get graph overview (`summary` mode) or full export (`full` mode) |

### Entity Management

| Tool | Description |
|------|-------------|
| `merge_entities` | Merge two entities: migrate observations and relations from source to target, then delete source |
| `update_entities` | Change an entity's type |
| `update_observations` | Replace an observation's content |
| `detect_conflicts` | Find potential duplicates and contradictions within an entity's observations |

### MCP Resources

| URI | Description |
|-----|-------------|
| `memory://graph/summary` | Graph statistics and entity type distribution |
| `memory://graph/recent` | Recently accessed entities |
| `memory://graph/types` | All entity and relation type enumerations |
| `memory://entities/{name}` | Full details of a specific entity |

### MCP Prompts

| Prompt | Description |
|--------|-------------|
| `memory-recall` | Recall relevant memories by topic |
| `memory-save` | Analyze conversation text and suggest what to save |
| `memory-review` | Generate a comprehensive review of an entity's memories |

## Installation

### Homebrew (macOS/Linux)

```bash
brew install okooo5km/tap/mms
```

### Quick Install Script (macOS/Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/okooo5km/memory-mcp-server-go/main/scripts/install.sh | bash
```

Options: `-v v0.2.3` for a specific version, `-d /usr/local/bin` for a custom directory.

### Pre-built Binaries

Download from [GitHub Releases](https://github.com/okooo5km/memory-mcp-server-go/releases/latest). Available for macOS (arm64/amd64), Linux (arm64/amd64), and Windows (arm64/amd64).

```bash
# Example: macOS Apple Silicon
curl -L https://github.com/okooo5km/memory-mcp-server-go/releases/latest/download/mms_VERSION_darwin_arm64.tar.gz | tar xz
chmod +x mms && mv mms ~/.local/bin/
```

### Build from Source

```bash
git clone https://github.com/okooo5km/memory-mcp-server-go.git
cd memory-mcp-server-go
make build        # binary in .build/
```

Make sure `~/.local/bin` (or your chosen directory) is in your `PATH`.

## Command Line Arguments

```
mms [options]
  -t, --transport string   Transport type: stdio, sse, or http (default "stdio")
  -m, --memory string      Memory file path (auto-detected if not specified)
  -p, --port int           Port for SSE/HTTP transport (default 8080)
  -v, --version            Show version

  Storage:
  --storage string         Force storage type: sqlite or jsonl (auto-detected)
  --auto-migrate           Auto-migrate JSONL to SQLite (default true)

  Migration:
  --migrate string         Source JSONL file for manual migration
  --migrate-to string      Destination SQLite file
  --dry-run                Dry run migration
  --force                  Overwrite destination

  Streamable HTTP:
  --http-endpoint string   HTTP endpoint path (default "/mcp")
  --http-heartbeat string  Heartbeat interval (default "30s")
  --http-stateless         Stateless HTTP mode

  Auth:
  --auth-bearer string     Require Bearer token for SSE/HTTP

  OAuth 2.1 (mutually exclusive with --auth-bearer):
  --oauth-user string      OAuth login username (env: OAUTH_USER)
  --oauth-pass string      OAuth login password (env: OAUTH_PASS)
  --oauth-issuer string    OAuth issuer URL (auto-detect if empty, env: OAUTH_ISSUER)

  CORS:
  --cors-origin string     Allowed CORS origins: '*' for all, or comma-separated list (default "*")
```

Examples:

```bash
mms                                          # stdio, auto-detect storage
mms --memory /path/to/memory.json            # custom path, auto-migrates to SQLite
mms --transport sse --port 9000              # SSE transport
mms --transport http --auth-bearer mytoken   # Streamable HTTP with Bearer auth
mms --transport http --oauth-user admin --oauth-pass secret  # OAuth 2.1 auth
mms --transport http --cors-origin "https://app.example.com,https://admin.example.com"  # CORS whitelist
```

## Configuration

### Claude Desktop / Claude.app

```json
"mcpServers": {
  "memory": {
    "command": "mms",
    "env": {
      "MEMORY_FILE_PATH": "/path/to/memory.json"
    }
  }
}
```

### Claude Desktop Connectors (OAuth)

For remote deployment with OAuth 2.1 authentication, compatible with Claude Desktop's Custom Connectors:

```bash
# Start server with OAuth
mms --transport http --oauth-user admin --oauth-pass secret --port 8080

# Or with environment variables and explicit issuer
OAUTH_USER=admin OAUTH_PASS=secret mms --transport http --oauth-issuer https://mcp.example.com
```

Then in Claude Desktop: **Settings → Connectors → Add Custom Connector** → enter your server URL (e.g., `https://mcp.example.com/mcp`). Claude will automatically discover the OAuth endpoints and prompt you to log in.

### Cursor

Add to Cursor Settings > mcp.json:

```json
{
  "mcpServers": {
    "memory": {
      "command": "mms",
      "env": {
        "MEMORY_FILE_PATH": "/path/to/memory.json"
      }
    }
  }
}
```

### Example System Prompt

```text
You have access to a Knowledge Graph memory system that persists across conversations.

Saving memories:
- create_entities: Add new people, places, concepts (check search_nodes first to avoid duplicates)
- create_relations: Record how entities relate to each other
- add_observations: Add facts to existing entities

Retrieving memories:
- search_nodes: Find relevant entities by keyword (supports synonyms like JS→JavaScript)
- open_nodes: Get full details of specific entities
- read_graph: Get an overview of all stored knowledge (use "summary" mode first)

Managing memories:
- merge_entities: Combine duplicate entities
- detect_conflicts: Find contradictory observations
- update_entities / update_observations: Fix incorrect data

Always check your memory before answering questions that might require past context.
```

## Streamable HTTP Usage

```bash
# 1. Initialize session
curl -i -X POST http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer mytoken' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}'

# 2. Listen for server messages
curl -N http://localhost:8080/mcp \
  -H 'Authorization: Bearer mytoken' \
  -H 'Mcp-Session-Id: <session-id>'

# 3. Call a tool
curl -s http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer mytoken' \
  -H 'Mcp-Session-Id: <session-id>' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_nodes","arguments":{"query":"idea"}}}'

# 4. Terminate session
curl -X DELETE http://localhost:8080/mcp \
  -H 'Authorization: Bearer mytoken' \
  -H 'Mcp-Session-Id: <session-id>'
```

## Security & Deployment

- Deploy behind TLS (Nginx/Caddy/Traefik), bind server to localhost
- **Simple auth**: Use `--auth-bearer $(openssl rand -hex 32)` for programmatic clients
- **OAuth 2.1**: Use `--oauth-user`/`--oauth-pass` for browser-based login (Claude Desktop Connectors). Supports PKCE (S256), dynamic client registration, and token refresh with rotation
- Forward `Authorization` header from reverse proxy to backend; set `--oauth-issuer` to the public URL when behind a proxy
- Run as non-root, open only required ports, enable rate limiting for untrusted clients

## Storage System

### Storage Types

| | SQLite (Recommended) | JSONL (Legacy) |
|---|---|---|
| **Read/Search** | 1.9x faster | Baseline |
| **Memory** | 1.9x more efficient | Baseline |
| **File Size** | Larger | 3x smaller |
| **Startup** | Slower | 55x faster |
| **Features** | FTS5, ACID, WAL, concurrent reads | Human-readable |
| **Best For** | >100 entities | <50 entities |

### Migration

```bash
# Automatic (default): just use your existing JSONL path
mms --memory /path/to/memory.json  # auto-migrates to .db

# Manual migration
mms --migrate /path/to/memory.json --migrate-to /path/to/memory.db

# Dry run
mms --migrate /path/to/memory.json --dry-run
```

## Knowledge Graph Structure

* **Entities**: Nodes with a name, type, and list of observations (each with optional metadata: source, confidence, tags)
* **Relations**: Directed edges between entities with a relation type in active voice
* **Observations**: Atomic facts associated with entities, supporting time-decay ranking based on access patterns

## Usage Examples

### Creating Entities

```json
{
  "entities": [
    {
      "name": "John Smith",
      "entityType": "person",
      "observations": ["Software engineer", "Lives in San Francisco", "Enjoys hiking"]
    },
    {
      "name": "Acme Corp",
      "entityType": "company",
      "observations": ["Founded in 2010", "Tech startup"]
    }
  ]
}
```

### Creating Relations

```json
{
  "relations": [
    { "from": "John Smith", "to": "Acme Corp", "relationType": "works at" }
  ]
}
```

### Searching with Graph Traversal

Search for "John" returns:
- **Direct hits**: Entities matching "John" with observation snippets
- **Related entities**: Entities connected to "John" via relations (e.g., "Acme Corp" via "works at")

### Merging Duplicate Entities

```json
{
  "sourceName": "React.js",
  "targetName": "React"
}
```

Merges all observations and relations from "React.js" into "React", then deletes "React.js".

### Detecting Conflicts

```json
{
  "entityName": "John Smith"
}
```

Returns potential duplicates (>60% prefix overlap) and contradictions (antonym keyword pairs like "likes/dislikes").

## Development

```bash
make fmt          # Format code
make check        # Static analysis (gofmt + go vet)
go test ./...     # Run tests
make build        # Build binary

# Full verification
make fmt && make check && go test ./... && make build
```

### Requirements

* Go 1.24+
* github.com/mark3labs/mcp-go v0.19.0+
* modernc.org/sqlite (pure Go SQLite, no CGO)

### Versioning

Version is managed via the `VERSION` file. Override with `make build VERSION=1.2.3` or `go build -ldflags "-X main.version=1.2.3"`.

## License

MIT License. See [LICENSE](LICENSE) for details.
