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

Tests are available in `storage_test.go`:

- Storage interface compatibility tests
- Migration functionality tests
- Run with `go test -v`

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

### Core Components

1. **KnowledgeGraphManager** (main.go:33-257):
   - Uses storage abstraction layer
   - Auto-detection of storage type
   - Backward compatibility with legacy API

2. **MCP Tools**:
   - create_entities, create_relations, add_observations
   - delete_entities, delete_relations, delete_observations
   - read_graph, search_nodes, open_nodes

### Command-line Arguments

```bash
./memory-mcp-server-go [options]
  -t, --transport string     Transport type: stdio or sse (default "stdio")
  -m, --memory string        Memory file path
  -p, --port int            Port for SSE transport (default 8080)
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
```

### Storage Type Detection

1. **SQLite**: `.db`, `.sqlite`, `.sqlite3` extensions
2. **Auto-migration**: JSONL files are auto-migrated to SQLite when `--auto-migrate=true`
3. **Fallback**: JSONL for backward compatibility

## Key Dependencies

- `github.com/mark3labs/mcp-go v0.19.0` - MCP SDK for Go
- `github.com/mattn/go-sqlite3 v1.14.28` - SQLite driver with CGO

## Migration

### Automatic Migration

- Enabled by default with `--auto-migrate=true`
- Detects existing JSONL files and migrates to SQLite
- Creates backup of original JSONL file

### Manual Migration

```bash
# Migrate specific file
./memory-mcp-server-go --migrate=old_data.json --migrate-to=new_data.db

# Dry run to check migration
./memory-mcp-server-go --migrate=old_data.json --dry-run

# Force overwrite existing destination
./memory-mcp-server-go --migrate=old_data.json --force
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

3. **Database Schema**:
   - Proper indexing on frequently queried columns
   - Foreign key constraints for data integrity
   - Optimized queries with prepared statements

## Important Notes

1. **CGO Dependency**: SQLite driver requires CGO, affecting cross-compilation
2. **Backward Compatibility**: All existing APIs remain unchanged
3. **Data Safety**: Automatic backups during migration
4. **Performance**: 10x+ improvement with SQLite backend
5. **Concurrent Access**: WAL mode supports multiple readers, single writer
