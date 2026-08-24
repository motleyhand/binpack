BINARY  := binpack
PKG     := github.com/motleyhand/binpack
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# The linter version, named here because the two workflows name it too and
# nothing was holding the three together. `make lint` runs whatever is on PATH
# — on the runners that is the binary golangci-lint-action seeded, locally it
# is whatever the contributor installed — so this is the number the project
# expects, and the target says so when the installed one differs.
# internal/cli's TestTheLinterVersionAgreesEverywhere keeps the copies equal.
GOLANGCI_VERSION := v2.12.2

# Pinned for the reason the linter is: a scan whose tool version floats reports
# a different set on two machines. The advisory *database* is fetched at run
# time and is meant to move; the tool reading it is not.
GOVULNCHECK_VERSION := v1.7.0

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
	@# A warning, not a refusal. Linting with a different build from the one
	@# that will judge the pull request is a real way to waste an afternoon —
	@# rules are added and removed between minors — but refusing to run would
	@# make installing one exact version a condition of contributing.
	@installed=$$(golangci-lint version --short 2>/dev/null); \
	if [ -n "$$installed" ] && [ "v$$installed" != "$(GOLANGCI_VERSION)" ]; then \
		echo "warning: golangci-lint v$$installed is installed; CI pins $(GOLANGCI_VERSION)" >&2; \
	fi
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

.PHONY: vuln
vuln: ## Check the dependencies and the toolchain against the Go vulnerability database
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: check-workflows
check-workflows: ## Check the GitHub workflows parse and stay consistent
	python3 hack/check-workflows.py

.PHONY: check-image-user
check-image-user: ## Check the image declares a numeric non-root user
	python3 hack/check-image-user.py

.PHONY: check
# Deliberately without `vuln`. release.yaml runs `make check` on every tag, and
# govulncheck answers from the advisory database as it stands at that moment
# rather than from the tree — so the identical commit passes on the pull
# request and fails on the tag, blocking a release on somebody else's
# disclosure. It would do the same to a contributor's first `make check`. The
# scan runs on a schedule instead, in .github/workflows/vulnerabilities.yaml,
# where being red is information rather than a blocked queue.
check: lint test build smoke check-workflows check-image-user ## Everything CI runs (except test-differential)

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk -F':.*?## ' '{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'
