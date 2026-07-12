.PHONY: build build-all clean test docker docker-push help tailwind tailwind-install tailwind-watch upx upx-install

BINARY_NAME := twitch-miner-go
MODULE := github.com/KraineOpasen/bukerov-twitch-miner-go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
GOFLAGS := -trimpath -buildvcs=false
DOCKER_REPO ?= ghcr.io/kraineopasen/bukerov-twitch-miner-go

# UPX configuration
UPX_VERSION := 5.0.2
ifeq ($(OS),Windows_NT)
    UPX_BIN := bin/upx.exe
    UPX_RELEASE := upx-$(UPX_VERSION)-win64
    UPX_EXT := .zip
else
    UNAME_S := $(shell uname -s)
    UNAME_M := $(shell uname -m)
    UPX_BIN := bin/upx
    UPX_EXT := .tar.xz
    ifeq ($(UNAME_M),aarch64)
        UPX_RELEASE := upx-$(UPX_VERSION)-arm64_linux
    else ifeq ($(UNAME_M),arm64)
        UPX_RELEASE := upx-$(UPX_VERSION)-arm64_linux
    else
        UPX_RELEASE := upx-$(UPX_VERSION)-amd64_linux
    endif
endif

# Tailwind configuration
TAILWIND_VERSION := v4.1.18
TAILWIND_INPUT := internal/web/static/css/input.css
TAILWIND_OUTPUT := internal/web/static/css/app.css

# Detect OS and architecture for Tailwind CLI download
ifeq ($(OS),Windows_NT)
    TAILWIND_BIN := bin/tailwindcss.exe
    TAILWIND_RELEASE := tailwindcss-windows-x64.exe
else
    UNAME_S := $(shell uname -s)
    UNAME_M := $(shell uname -m)
    TAILWIND_BIN := bin/tailwindcss
    ifeq ($(UNAME_S),Darwin)
        ifeq ($(UNAME_M),arm64)
            TAILWIND_RELEASE := tailwindcss-macos-arm64
        else
            TAILWIND_RELEASE := tailwindcss-macos-x64
        endif
    else
        ifeq ($(UNAME_M),aarch64)
            TAILWIND_RELEASE := tailwindcss-linux-arm64
        else
            TAILWIND_RELEASE := tailwindcss-linux-x64
        endif
    endif
endif

# Install Tailwind CLI
tailwind-install:
	@mkdir -p bin
	@echo "Downloading Tailwind CSS CLI $(TAILWIND_VERSION)..."
	curl -sLo $(TAILWIND_BIN) https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/$(TAILWIND_RELEASE)
	chmod +x $(TAILWIND_BIN)
	@echo "Tailwind CSS CLI installed to $(TAILWIND_BIN)"

# Build Tailwind CSS (production)
tailwind: $(TAILWIND_BIN)
	$(TAILWIND_BIN) -i $(TAILWIND_INPUT) -o $(TAILWIND_OUTPUT) --minify

# Watch mode for Tailwind development
tailwind-watch: $(TAILWIND_BIN)
	$(TAILWIND_BIN) -i $(TAILWIND_INPUT) -o $(TAILWIND_OUTPUT) --watch

$(TAILWIND_BIN):
	$(MAKE) tailwind-install

# Install UPX
upx-install:
	@mkdir -p bin
	@echo "Downloading UPX $(UPX_VERSION)..."
ifeq ($(OS),Windows_NT)
	curl -sLo bin/upx$(UPX_EXT) https://github.com/upx/upx/releases/download/v$(UPX_VERSION)/$(UPX_RELEASE)$(UPX_EXT)
	cd bin && unzip -o upx$(UPX_EXT) && mv $(UPX_RELEASE)/upx.exe . && rm -rf $(UPX_RELEASE) upx$(UPX_EXT)
else
	curl -sLo bin/upx$(UPX_EXT) https://github.com/upx/upx/releases/download/v$(UPX_VERSION)/$(UPX_RELEASE)$(UPX_EXT)
	cd bin && tar -xf upx$(UPX_EXT) && mv $(UPX_RELEASE)/upx . && rm -rf $(UPX_RELEASE) upx$(UPX_EXT)
endif
	@echo "UPX installed to $(UPX_BIN)"

$(UPX_BIN):
	$(MAKE) upx-install

# Compress binary with UPX
upx: $(UPX_BIN)
	$(UPX_BIN) --best --lzma $(BINARY_NAME)

# Build for current platform (includes Tailwind)
build: tailwind
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/miner

# Build and compress with UPX
build-compressed: build upx

# Build Go only (skip Tailwind - use when CSS is already built)
build-go:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/miner

# Build for all platforms
build-all: build-linux build-linux-arm64 build-windows build-darwin build-darwin-arm64

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-linux-amd64 ./cmd/miner

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-linux-arm64 ./cmd/miner

build-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-windows-amd64.exe ./cmd/miner

build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-darwin-amd64 ./cmd/miner

build-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-darwin-arm64 ./cmd/miner

# Run tests
test:
	go test -v -race ./...

# Run linter
lint:
	golangci-lint run

# Build Docker image
docker:
	docker build -t $(DOCKER_REPO):$(VERSION) -t $(DOCKER_REPO):latest .

# Push Docker image
docker-push: docker
	docker push $(DOCKER_REPO):$(VERSION)
	docker push $(DOCKER_REPO):latest

# Clean build artifacts
clean:
	rm -f $(BINARY_NAME)
	rm -rf dist/
	go clean

# Generate sample config
generate-config: build
	./$(BINARY_NAME) -generate-config

# Show help
help:
	@echo "Twitch Channel Points Miner - Build Targets"
	@echo ""
	@echo "  build            Build for current platform (includes Tailwind)"
	@echo "  build-compressed Build and compress with UPX (smallest size)"
	@echo "  build-go         Build Go binary only (skip Tailwind)"
	@echo "  build-all        Build for all platforms (linux, windows, darwin)"
	@echo "  upx              Compress existing binary with UPX"
	@echo "  upx-install      Install UPX binary"
	@echo "  tailwind         Build Tailwind CSS (production minified)"
	@echo "  tailwind-watch   Watch mode for Tailwind development"
	@echo "  tailwind-install Install Tailwind CLI binary"
	@echo "  test             Run tests"
	@echo "  lint             Run linter"
	@echo "  docker           Build Docker image"
	@echo "  docker-push      Build and push Docker image"
	@echo "  clean            Clean build artifacts"
	@echo "  generate-config  Generate sample configuration"
	@echo ""
	@echo "Variables:"
	@echo "  VERSION         Version string (default: git tag or 'dev')"
	@echo "  DOCKER_REPO     Docker repository (default: ghcr.io/kraineopasen/bukerov-twitch-miner-go)"
