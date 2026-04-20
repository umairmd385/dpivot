BINARY     := dpivot
PLUGIN     := docker-dpivot
BUILD_DIR  := ./bin
CMD        := ./cmd/dpivot
IMAGE      := dpivot/proxy
TAG        := latest
PLUGIN_DIR := $(HOME)/.docker/cli-plugins

GOFLAGS    := -trimpath -ldflags="-s -w"

.PHONY: build test test-integration install-plugin lint docker-build clean help

## build: Compile the dpivot binary to ./bin/dpivot
build:
	@mkdir -p $(BUILD_DIR)
	go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD)
	@echo "Built: $(BUILD_DIR)/$(BINARY)"

## test: Run all unit tests with the race detector
test:
	go test -race -timeout 60s ./...

## test-integration: Run integration tests (requires Docker)
test-integration:
	DOCKER_INTEGRATION=true go test -race -timeout 120s ./tests/integration/...

## install-plugin: Copy the binary to Docker CLI plugins directory
install-plugin: build
	@mkdir -p $(PLUGIN_DIR)
	cp $(BUILD_DIR)/$(BINARY) $(PLUGIN_DIR)/$(PLUGIN)
	@echo "Plugin installed: $(PLUGIN_DIR)/$(PLUGIN)"
	@echo "Run: docker dpivot --help"

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## docker-build: Build the proxy container image
docker-build:
	docker build -t $(IMAGE):$(TAG) -f docker/proxy/Dockerfile .
	@echo "Built: $(IMAGE):$(TAG)"

## clean: Remove built binaries
clean:
	rm -rf $(BUILD_DIR)

## help: Show this help message
help:
	@grep -E '^##' $(MAKEFILE_LIST) | sed 's/## //'
