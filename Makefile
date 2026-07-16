.PHONY: all build dev clean deps help \
       plugin plugin-docker plugin-arm64 plugin-amd64 \
       bifrost-dynamic bifrost-dynamic-docker \
       deploy deploy-docker verify

# ─── Configuration ───────────────────────────────────────────────────────────

PLUGIN_NAME   := mcpfilter
PLUGIN_DIR    := plugins/mcpfilter
OUTPUT_DIR    := build
OUTPUT        := $(OUTPUT_DIR)/$(PLUGIN_NAME).so

# Bifrost submodule path
BIFROST_DIR   := lib/bifrost
BIFROST_HTTP  := $(BIFROST_DIR)/transports/bifrost-http

# Go version (must match Bifrost)
GO_VERSION    := 1.26.4

# Docker image for cross-compilation
DOCKER_IMAGE  := golang:$(GO_VERSION)-alpine3.23

# Host detection
HOST_OS   := $(shell uname -s | tr '[:upper:]' '[:lower:]')
HOST_ARCH := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

# ─── Help ────────────────────────────────────────────────────────────────────

.DEFAULT_GOAL := help

help: ## Show available targets
	@echo ''
	@echo '$(PLUGIN_NAME) plugin'
	@echo ''
	@echo 'Usage: make <target>'
	@echo ''
	@echo 'Plugin targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-28s %s\n", $$1, $$2}' $(MAKEFILE_LIST) | grep -E '(plugin|bifrost|deploy|verify|deps|clean)'
	@echo ''
	@echo 'Workflow:'
	@echo '  1. make deps                  # download Go modules'
	@echo '  2. make plugin                # build .so for current arch'
	@echo '  3. make bifrost-dynamic       # rebuild Bifrost with plugin support'
	@echo '  4. make deploy                # copy .so into container volume'
	@echo ''

# ─── Dependencies ────────────────────────────────────────────────────────────

deps: ## Download and tidy Go modules
	@echo 'Downloading dependencies...'
	cd $(PLUGIN_DIR) && go mod download && go mod tidy
	@echo 'Dependencies ready'

# ─── Plugin build (native) ──────────────────────────────────────────────────

plugin: deps ## Build plugin for current host platform
	@mkdir -p $(OUTPUT_DIR)
	@echo 'Building $(PLUGIN_NAME) for $(HOST_OS)/$(HOST_ARCH)...'
	cd $(PLUGIN_DIR) && \
		CGO_ENABLED=1 GOOS=$(HOST_OS) GOARCH=$(HOST_ARCH) \
			go build -buildmode=plugin \
				-ldflags="-w -s" \
				-trimpath \
				-o $(CURDIR)/$(OUTPUT) .
	@echo 'Plugin built: $(OUTPUT)'
	@file $(OUTPUT) 2>/dev/null || true

dev: deps ## Build plugin for development (fast, no strip)
	@mkdir -p $(OUTPUT_DIR)
	@echo 'Building $(PLUGIN_NAME) (dev, no optimizations)...'
	cd $(PLUGIN_DIR) && \
		CGO_ENABLED=1 go build -buildmode=plugin -o $(CURDIR)/$(OUTPUT) .
	@echo 'Plugin built: $(OUTPUT)'

# ─── Plugin build (Docker cross-compilation) ─────────────────────────────────

plugin-docker: deps ## Build plugin via Docker (auto-detects arch)
	@mkdir -p $(OUTPUT_DIR)
	@echo 'Building $(PLUGIN_NAME) in Docker ($(HOST_ARCH))...'
	docker run --rm \
		-v "$(CURDIR):/work" \
		-w /work/$(PLUGIN_DIR) \
		-e CGO_ENABLED=1 \
		-e GOOS=linux \
		-e GOARCH=$(HOST_ARCH) \
		$(DOCKER_IMAGE) \
		sh -c "apk add --no-cache gcc musl-dev && \
			go build -buildmode=plugin -ldflags='-w -s' -trimpath \
				-o /work/$(OUTPUT) ."
	@echo 'Plugin built: $(OUTPUT)'

plugin-arm64: deps ## Build plugin for Linux ARM64 via Docker
	@mkdir -p $(OUTPUT_DIR)
	@echo 'Building $(PLUGIN_NAME) for linux/arm64...'
	docker run --rm \
		--platform linux/arm64 \
		-v "$(CURDIR):/work" \
		-w /work/$(PLUGIN_DIR) \
		-e CGO_ENABLED=1 \
		-e GOOS=linux \
		-e GOARCH=arm64 \
		$(DOCKER_IMAGE) \
		sh -c "apk add --no-cache gcc musl-dev && \
			go build -buildmode=plugin -ldflags='-w -s' -trimpath \
				-o /work/$(OUTPUT_DIR)/$(PLUGIN_NAME)-arm64.so ."
	@echo 'Plugin built: $(OUTPUT_DIR)/$(PLUGIN_NAME)-arm64.so'

plugin-amd64: deps ## Build plugin for Linux AMD64 via Docker
	@mkdir -p $(OUTPUT_DIR)
	@echo 'Building $(PLUGIN_NAME) for linux/amd64...'
	docker run --rm \
		--platform linux/amd64 \
		-v "$(CURDIR):/work" \
		-w /work/$(PLUGIN_DIR) \
		-e CGO_ENABLED=1 \
		-e GOOS=linux \
		-e GOARCH=amd64 \
		$(DOCKER_IMAGE) \
		sh -c "apk add --no-cache gcc musl-dev && \
			go build -buildmode=plugin -ldflags='-w -s' -trimpath \
				-o /work/$(OUTPUT_DIR)/$(PLUGIN_NAME)-amd64.so ."
	@echo 'Plugin built: $(OUTPUT_DIR)/$(PLUGIN_NAME)-amd64.so'

# ─── Bifrost dynamic build (required for plugin loading) ─────────────────────

bifrost-dynamic: ## Rebuild Bifrost binary with dynamic linking (local)
	@if [ ! -d "$(BIFROST_HTTP)" ]; then \
		echo 'Bifrost submodule not found at $(BIFROST_DIR)'; \
		echo '  Run: git submodule update --init --recursive'; \
		exit 1; \
	fi
	@echo 'Building Bifrost with dynamic linking (plugins enabled)...'
	cd $(BIFROST_HTTP) && \
		GOWORK=off CGO_ENABLED=1 \
			go build \
				-ldflags="-w -s -X main.Version=dynamic" \
				-a -trimpath \
				-tags "sqlite_static" \
				-o ../../tmp/bifrost-http \
				.
	@echo 'Bifrost built: $(BIFROST_DIR)/tmp/bifrost-http'
	@echo 'Replace the static binary in your container with this one'
	@file $(BIFROST_DIR)/tmp/bifrost-http 2>/dev/null || true

bifrost-dynamic-docker: ## Rebuild Bifrost with dynamic linking via Docker
	@if [ ! -d "$(BIFROST_HTTP)" ]; then \
		echo 'Bifrost submodule not found at $(BIFROST_DIR)'; \
		exit 1; \
	fi
	@echo 'Building Bifrost dynamically in Docker...'
	docker run --rm \
		-v "$(CURDIR)/$(BIFROST_DIR):/bifrost" \
		-w /bifrost/transports \
		-e CGO_ENABLED=1 \
		-e GOOS=linux \
		-e GOARCH=$(HOST_ARCH) \
		-e GOWORK=off \
		$(DOCKER_IMAGE) \
		sh -c "apk add --no-cache gcc musl-dev sqlite-dev && \
			go build \
				-ldflags='-w -s -X main.Version=dynamic' \
				-a -trimpath \
				-tags 'sqlite_static' \
				-o /bifrost/tmp/bifrost-http \
				./bifrost-http"
	@echo 'Bifrost built: $(BIFROST_DIR)/tmp/bifrost-http'

# ─── Deploy ──────────────────────────────────────────────────────────────────

deploy: plugin ## Build and copy .so to container volume mount point
	@echo 'Deploying plugin...'
	@echo ''
	@echo 'Manual steps:'
	@echo '  1. Copy $(OUTPUT) into the Bifrost container:'
	@echo '     docker cp $(OUTPUT) <container>:/tmp/$(PLUGIN_NAME).so'
	@echo ''
	@echo '  2. In Bifrost Web UI → Plugins → Add Plugin:'
	@echo '     Path: /tmp/$(PLUGIN_NAME).so'
	@echo ''
	@echo '  3. Or mount a volume:'
	@echo '     -v $(CURDIR)/$(OUTPUT):/plugins/$(PLUGIN_NAME).so:ro'
	@echo ''

deploy-docker: plugin-docker ## Build via Docker and show deploy instructions
	@$(MAKE) deploy

# ─── Verify ──────────────────────────────────────────────────────────────────

verify: ## Verify the built plugin
	@if [ ! -f "$(OUTPUT)" ]; then \
		echo 'Plugin not found: $(OUTPUT)'; \
		echo '  Run: make plugin'; \
		exit 1; \
	fi
	@echo 'Verifying $(OUTPUT)...'
	@echo ''
	@echo 'File info:'
	@file $(OUTPUT)
	@echo ''
	@echo 'Dynamic libraries:'
	@ldd $(OUTPUT) 2>/dev/null || readelf -d $(OUTPUT) 2>/dev/null | head -20 || echo '(readelf not available)'
	@echo ''
	@echo 'Exported symbols:'
	@nm -D $(OUTPUT) 2>/dev/null | grep -E ' (T|t) ' | head -20 || echo '(nm not available)'
	@echo ''
	@echo 'Verify complete'

# ─── Clean ───────────────────────────────────────────────────────────────────

clean: ## Remove build artifacts
	@echo 'Cleaning...'
	rm -rf $(OUTPUT_DIR)
	@echo 'Clean'
