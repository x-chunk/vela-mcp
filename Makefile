# Vela MCP development and release targets. Run "make help" for a summary.
MODULE := go.xchunk.org/vela-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
# Use the commit time rather than the build time, matching release automation.
DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)
GOFLAGS_BUILD := -buildvcs=false -trimpath -ldflags "$(LDFLAGS)"

# Pinned tools use the same Go toolchain locally and in CI. Both commands are
# overridable to use a preinstalled binary instead of go run.
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GORELEASER_VERSION := v2.18.3
GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
IMAGE ?= xchunk/vela-mcp

export CGO_ENABLED := 0

.PHONY: help build test check lint integration release-check release-snapshot docker clean

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z0-9-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build vela-mcp into ./bin with version metadata
	go build $(GOFLAGS_BUILD) -o bin/vela-mcp ./cmd/vela-mcp

# The race detector needs cgo; release binaries and the image remain static.
test: ## Run all unit tests with the race detector
	CGO_ENABLED=1 go test -race -count=1 ./...

check: ## Vet all packages, including the opt-in integration tests
	go vet -tags=integration ./...

lint: ## Run pinned golangci-lint
	$(GOLANGCI_LINT) run ./...

integration: build ## Test against a disposable Vela store (VELA_MCP_TEST_KEYS required)
	@test -n "$(VELA_MCP_TEST_KEYS)" || { echo "Set VELA_MCP_TEST_KEYS to a disposable Vela store's admin/ro/rw key file." >&2; exit 1; }
	VELA_MCP_TEST_BINARY=$(CURDIR)/bin/vela-mcp CGO_ENABLED=1 go test -race -tags=integration -count=1 -v ./test/integration

release-check: ## Validate the GoReleaser configuration without publishing
	$(GORELEASER) check

release-snapshot: ## Build six-platform release archives locally without publishing
	$(GORELEASER) release --snapshot --clean --skip=publish

docker: ## Build the local Docker image with the same release metadata
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):dev .

clean: ## Remove generated binaries and release archives
	rm -rf bin dist
