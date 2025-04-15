# Memory MCP Server Go Makefile
# Targets all major desktop platforms

APP_NAME=memory-mcp-server-go
BUILD_DIR=.build
VERSION=0.1.0
# 移除 Git 检测，直接使用版本号
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all clean build-all \
	build-darwin-amd64 build-darwin-arm64 build-darwin-universal \
	build-linux-amd64 build-linux-arm64 \
	build-windows-amd64 build-windows-arm64

all: build-all

# Ensure build directory exists
$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)

# Build for all platforms
build-all: build-darwin-universal build-linux-amd64 build-linux-arm64 build-windows-amd64 build-windows-arm64

# macOS (Intel/amd64)
build-darwin-amd64: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-darwin-amd64

# macOS (Apple Silicon/arm64)
build-darwin-arm64: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64

# macOS (Universal Binary)
build-darwin-universal: build-darwin-amd64 build-darwin-arm64
	lipo -create -output $(BUILD_DIR)/$(APP_NAME)-darwin-universal \
		$(BUILD_DIR)/$(APP_NAME)-darwin-amd64 \
		$(BUILD_DIR)/$(APP_NAME)-darwin-arm64

# Linux (Intel/amd64)
build-linux-amd64: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64

# Linux (ARM64)
build-linux-arm64: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64

# Windows (Intel/amd64)
build-windows-amd64: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe

# Windows (ARM64)
build-windows-arm64: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-windows-arm64.exe

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