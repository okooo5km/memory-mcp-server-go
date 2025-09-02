## Go build Makefile (standardized)

APP_NAME ?= memory-mcp-server-go
BUILD_DIR ?= .build
# Single source of truth for version (no git required)
VERSION_FILE ?= VERSION
VERSION ?= $(shell cat $(VERSION_FILE) 2>/dev/null || echo 0.0.0)
GO ?= go
CGO_ENABLED ?= 0

# ldflags: strip symbols and inject version
LD_FLAGS := -s -w -X main.version=$(VERSION)
GO_BUILD_FLAGS ?= -trimpath -ldflags '$(LD_FLAGS)'

# Target matrix
GOOSARCHES ?= \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64 \
	windows/arm64

# Formatting exclusions and file list
EXCLUDE_DIRS ?= .cache .build vendor
FIND_EXCLUDES := $(foreach d,$(EXCLUDE_DIRS),-not -path './$(d)/*')
GO_SOURCES := $(shell find . -type f -name '*.go' $(FIND_EXCLUDES))

.PHONY: all build build-all clean dist help fmt vet tidy deps verify check

all: build

# Local build for current platform
build: $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GO_BUILD_FLAGS) -o $(BUILD_DIR)/$(APP_NAME) .

# Ensure build directory exists
$(BUILD_DIR):
	@mkdir -p $(BUILD_DIR)

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)

# Format source code (in-place)
fmt:
	@echo "Formatting Go sources..."
	@if [ -n "$(GO_SOURCES)" ]; then \
		gofmt -s -w $(GO_SOURCES); \
	fi
	$(GO) fmt ./...

# Static analysis
vet:
	$(GO) vet ./...

# Sync go.mod/go.sum
tidy:
	$(GO) mod tidy

# Pre-fetch modules
deps:
	$(GO) mod download

# Verify dependencies against go.sum
verify:
	$(GO) mod verify

# Quick sanity checks (format + vet without modifying files)
check:
	@echo "Checking formatting..."; \
	CHANGED=$$(gofmt -s -l $(GO_SOURCES) || true); \
	if [ -n "$$CHANGED" ]; then \
		echo "The following files need formatting:"; \
		echo "$$CHANGED"; \
		exit 1; \
	fi; \
	$(GO) vet ./...

# Cross-compile for common OS/ARCH targets
build-all: $(BUILD_DIR)
	@set -euo pipefail; \
	CACHE_ROOT=$$(pwd)/.cache; \
	GOCACHE=$${GOCACHE:-$$CACHE_ROOT/gobuild}; \
	GOMODCACHE=$${GOMODCACHE:-$$CACHE_ROOT/gomod}; \
	mkdir -p $$GOCACHE $$GOMODCACHE; \
	for target in $(GOOSARCHES); do \
		os=$${target%/*}; arch=$${target#*/}; \
		case $$os in windows) ext=.exe;; *) ext=;; esac; \
		out="$(BUILD_DIR)/$(APP_NAME)-$$os-$$arch$$ext"; \
		echo "Building $$os/$$arch -> $$out"; \
		GOCACHE=$$GOCACHE GOMODCACHE=$$GOMODCACHE \
		CGO_ENABLED=$(CGO_ENABLED) GOOS=$$os GOARCH=$$arch \
		$(GO) build $(GO_BUILD_FLAGS) -o "$$out" .; \
	done; \
	echo "Binaries created in $(BUILD_DIR)/"

# Create compressed artifacts from build-all outputs
dist: build-all
	@set -e; mkdir -p $(BUILD_DIR)/dist; \
	for f in $(BUILD_DIR)/$(APP_NAME)-*; do \
		base=$${f##*/}; \
		case $$base in \
			*.exe) (cd $(BUILD_DIR) && zip -9 -q "dist/$${base%.*}.zip" "$$base") ;; \
			*) (cd $(BUILD_DIR) && tar -czf "dist/$$base.tgz" "$$base") ;; \
		esac; \
	done; \
	echo "Artifacts in $(BUILD_DIR)/dist"

# Show help
help:
	@echo "$(APP_NAME) - Make targets"
	@echo "Version: $(VERSION)"
	@echo
	@echo "Targets:"
	@echo "  build        Build for current platform"
	@echo "  build-all    Build for common OS/ARCH (cross-compile)"
	@echo "  dist         Package binaries into zip/tgz"
	@echo "  clean        Remove build artifacts"
	@echo
	@echo "Variables (override with make VAR=value):"
	@echo "  VERSION      Version string for -ldflags (default: git describe or 0.2.2)"
	@echo "  CGO_ENABLED  0 for pure Go builds (default: 0)"
	@echo "  GO           Go command (default: go)"
SHELL := /usr/bin/env bash
