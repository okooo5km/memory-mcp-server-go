# Repository Guidelines

## Project Structure & Module Organization
- `main.go`: Server entry point (MCP transports, CLI flags).
- `storage/`: Storage interfaces and backends (`sqlite.go`, `jsonl.go`, `migration.go`).
- `scripts/install.sh`: Cross-platform installer for release binaries.
- `Makefile`: Standard build, format, verify, and cross-compile targets.
- `.build/`: Output directory for local builds and packaged artifacts.
- `VERSION`: Single source of truth for release/version strings.

## Build, Test, and Development Commands
- `make build`: Build for current platform into `.build/memory-mcp-server-go`.
- `make build-all`: Cross-compile for common OS/ARCH.
- `make dist`: Package cross builds into `.build/dist`.
- `make fmt vet check`: Format sources, run `go vet`, and quick sanity checks.
- `make tidy deps verify`: Module housekeeping and dependency verification.
- Run locally: `go run . --transport stdio` or execute the binary from `.build/`.

## Feature Development Workflow

### Quick Verification (One-liner)
Before pushing, run the full verification pipeline:
```bash
make fmt && make check && go test ./... && make build
```

### Step-by-Step Workflow
1. **Write code**: Keep changes focused and incremental.
2. **Format**: `make fmt` - auto-fix formatting issues.
3. **Static analysis**: `make vet` - catch potential bugs.
4. **Sync dependencies**: `go mod tidy` - ensure `go.mod`/`go.sum` are up to date.
5. **Run tests**: `go test ./...` - verify all tests pass.
6. **Build**: `make build` - confirm the binary compiles.
7. **Commit**: Follow Conventional Commits (see below).

### CI Pipeline Mapping
| Local Command | CI Step | Purpose |
|---------------|---------|---------|
| `make deps` | Dependencies | Download Go modules |
| `go mod tidy` | Tidy check | Ensure no uncommitted module changes |
| `make check` | Lint | Formatting (`gofmt -s`) + `go vet` |
| `make build` | Build | Compile for current platform |

## Coding Style & Naming Conventions
- Use `gofmt`/`go fmt` (Tabs, idiomatic Go). Run `make fmt vet` before pushing.
- Packages: short, lowercase; files: lowercase with underscores (e.g., `sqlite_fts.go`).
- Exported types/functions: `PascalCase`; unexported: `camelCase`.
- Logging: never write human logs to stdout (reserved for MCP JSON-RPC). Use stderr.
- Prefer small, focused functions; add comments for non-obvious behavior and public APIs.

## Pre-commit Checklist (Required)
- Formatting: `make fmt` must produce no changes; CI runs `make check` which fails if `gofmt -s` finds diffs.
- Static analysis: `make vet` and `make check` must pass locally.
- Modules: `go mod tidy` should not change `go.mod`/`go.sum`. If it does, include those changes.
- Tests: `go test ./...` passes without failures.
- Quick build: `make build` succeeds and produces `.build/memory-mcp-server-go`.
- Scope: keep diffs minimal and focused; update README when flags or endpoints change.

### CI/CD Integration
- CI (`.github/workflows/ci.yml`) runs: deps -> `go mod tidy` (no diff) -> `make check` -> `make build` on pushes/PRs and tags `v*`.
- Release (`.github/workflows/release.yml`) runs: `go mod tidy` (no diff) -> `make check` -> cross-build + package before publishing.

### Troubleshooting CI Failures
| Failure | Cause | Fix |
|---------|-------|-----|
| "files need formatting" | Code not formatted | Run `make fmt` and re-commit |
| "go.mod/go.sum changed" | Dependencies out of sync | Run `go mod tidy` and commit changes |
| "go vet" errors | Static analysis issues | Fix reported issues in code |
| Build failure | Syntax or import errors | Check compiler output, fix errors |

## Testing Guidelines
- Framework: Go `testing`; table-driven tests preferred.
- Location: co-located `*_test.go` files (e.g., `storage/sqlite_test.go`).
- Run: `go test ./...` (add flags like `-run`/`-v` as needed).
- Use temp dirs/files for storage tests; avoid committing datasets. Aim for meaningful coverage on `storage/` and migrations.

## Commit & Pull Request Guidelines
- Conventional Commits style is used (emojis optional):
  - Examples: `feat(storage): add FTS search`, `fix: correct stdout logging`, `docs: update install instructions`.
  - types:
    - feat: Add new features
    - fix: Fix bugs
    - docs: Documentation changes
    - style: Code style changes (no logic impact)
    - refactor: Refactoring (neither new features nor bug fixes)
    - perf: Performance improvements
    - test: Test-related changes
    - build: Build system or dependency changes
    - ci: CI configuration or scripts
    - chore: Miscellaneous tasks
    - revert: Revert changes
- PRs should include: clear description, rationale, usage notes/flags, and linked issues. Show before/after behavior when applicable.
- Keep diffs focused; update README and this file when changing CLI, storage behavior, or build scripts.

## Security & Configuration Tips
- Configuration via flags and env (e.g., `MEMORY_FILE_PATH`). Do not print secrets.
- Default builds use pure Go SQLite (`CGO_ENABLED=0`). Respect stdout protocol constraints across transports.
