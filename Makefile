# Memory MCP Server Go Makefile
# Targets all major desktop platforms with pure Go SQLite

APP_NAME=memory-mcp-server-go
BUILD_DIR=.build
VERSION=0.2.0
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all clean build-all dev help

all: build-all

# Local development build (current platform)
dev: $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)


# Ensure build directory exists
$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)

# Build for all platforms with pure Go SQLite
build-all: $(BUILD_DIR)
	@echo "Building all platforms with pure Go SQLite support..."
	@echo "Temporarily switching to pure Go SQLite driver..."
	@if ! grep -q "modernc.org/sqlite" go.mod; then \
		go get modernc.org/sqlite@latest; \
	fi
	@# Create temporary main file with pure Go SQLite
	@sed 's|_ "github.com/mattn/go-sqlite3"|_ "modernc.org/sqlite"|' main.go > main_purego.go
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 ./main_purego.go
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 ./main_purego.go
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64 ./main_purego.go
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64 ./main_purego.go
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe ./main_purego.go
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-windows-arm64.exe ./main_purego.go
	@# Create macOS universal binary
	lipo -create -output $(BUILD_DIR)/$(APP_NAME)-darwin-universal \
		$(BUILD_DIR)/$(APP_NAME)-darwin-amd64 \
		$(BUILD_DIR)/$(APP_NAME)-darwin-arm64
	@rm -f main_purego.go
	@echo "All platform binaries with SQLite support created in $(BUILD_DIR)/"

# Create distribution archives
.PHONY: dist
dist: build-all
	mkdir -p $(BUILD_DIR)/dist
	# macOS Intel (x86_64)
	cp $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 $(BUILD_DIR)/dist/$(APP_NAME)
	cd $(BUILD_DIR)/dist && zip -r ../$(APP_NAME)-macos-x86_64.zip $(APP_NAME) && rm $(APP_NAME)
	
	# macOS Apple Silicon (arm64)
	cp $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 $(BUILD_DIR)/dist/$(APP_NAME)
	cd $(BUILD_DIR)/dist && zip -r ../$(APP_NAME)-macos-arm64.zip $(APP_NAME) && rm $(APP_NAME)
	
	# macOS Universal
	cp $(BUILD_DIR)/$(APP_NAME)-darwin-universal $(BUILD_DIR)/dist/$(APP_NAME)
	cd $(BUILD_DIR)/dist && zip -r ../$(APP_NAME)-macos-universal.zip $(APP_NAME) && rm $(APP_NAME)
	
	# Linux AMD64
	cp $(BUILD_DIR)/$(APP_NAME)-linux-amd64 $(BUILD_DIR)/dist/$(APP_NAME)
	cd $(BUILD_DIR)/dist && tar -czf ../$(APP_NAME)-linux-amd64.tar.gz $(APP_NAME) && rm $(APP_NAME)
	
	# Linux ARM64
	cp $(BUILD_DIR)/$(APP_NAME)-linux-arm64 $(BUILD_DIR)/dist/$(APP_NAME)
	cd $(BUILD_DIR)/dist && tar -czf ../$(APP_NAME)-linux-arm64.tar.gz $(APP_NAME) && rm $(APP_NAME)
	
	# Windows AMD64
	cp $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe $(BUILD_DIR)/dist/$(APP_NAME).exe
	cd $(BUILD_DIR)/dist && zip -r ../$(APP_NAME)-windows-amd64.zip $(APP_NAME).exe && rm $(APP_NAME).exe
	
	# Windows ARM64
	cp $(BUILD_DIR)/$(APP_NAME)-windows-arm64.exe $(BUILD_DIR)/dist/$(APP_NAME).exe
	cd $(BUILD_DIR)/dist && zip -r ../$(APP_NAME)-windows-arm64.zip $(APP_NAME).exe && rm $(APP_NAME).exe
	
	rmdir $(BUILD_DIR)/dist
	@echo "Distribution archives created in $(BUILD_DIR)/"

# Show help
help:
	@echo "Memory MCP Server Go - Build System"
	@echo "Version: $(VERSION)"
	@echo ""
	@echo "Available targets:"
	@echo "  all              Build for all platforms with pure Go SQLite"
	@echo "  build-all        Build for all platforms with pure Go SQLite"
	@echo "  dev              Build for current platform (development)"
	@echo "  clean            Clean build artifacts"
	@echo "  dist             Create distribution archives"
	@echo ""
	@echo "Notes:"
	@echo "  - All builds use pure Go SQLite (modernc.org/sqlite) for simplicity"
	@echo "  - Supports automatic JSONL->SQLite migration when needed"
	@echo "  - Cross-platform builds work without CGO dependencies"