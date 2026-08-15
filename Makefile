BINARY  := binpack
PKG     := github.com/motleyhand/binpack
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.version=$(VERSION) \
	-X $(PKG)/internal/version.commit=$(COMMIT) \
	-X $(PKG)/internal/version.date=$(DATE)

.DEFAULT_GOAL := check

.PHONY: build
build: ## Build the binary into ./bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: test
test: ## Run the tests
	go test -race ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format the code
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum in both modules
	go mod tidy
	# test/differential consumes the main module via a replace directive, so a
	# dependency change here leaves it stale and fails its CI job on an
	# otherwise unrelated PR. Tidying both together is the only reliable way
	# to not rediscover that from a red check.
	cd test/differential && go mod tidy

.PHONY: smoke
smoke: build ## Run the built binary once
	./bin/$(BINARY) version

.PHONY: test-differential
test-differential: ## Check the fit predicate against the real scheduler
	cd test/differential && go test ./...

.PHONY: check
check: lint test build smoke ## Everything CI runs (except test-differential)

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk -F':.*?## ' '{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'
