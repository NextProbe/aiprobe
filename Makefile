BINARY  := bin/aiprobe
PKG := .

# The version is not decoration: the analysis history keys its cache on it, so two
# builds that produce different analyses must not share a version string. A clean
# tagged build keeps its tag and stays reproducible; anything else gets a build
# stamp appended, because a rebuilt working tree is a different build even when git
# cannot tell.
GIT_DESCRIBE := $(shell git describe --tags --always 2>/dev/null)
GIT_DIRTY    := $(shell git diff --quiet 2>/dev/null || echo dirty)
BUILD_STAMP  := $(shell date -u +%Y%m%d%H%M%S)

ifeq ($(GIT_DESCRIBE),)
VERSION := 0.1.0-dev+$(BUILD_STAMP)
else ifeq ($(GIT_DIRTY),dirty)
VERSION := $(GIT_DESCRIBE)+dirty.$(BUILD_STAMP)
else
VERSION := $(GIT_DESCRIBE)
endif

LDFLAGS := -X github.com/NextProbe/aiprobe/internal/cli.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: install
install: ## Install into GOBIN
	go install -ldflags "$(LDFLAGS)" $(PKG)

.PHONY: test
test: ## Run the test suite
	go test ./...

.PHONY: test-v
test-v: ## Run the test suite with verbose output
	go test -v ./...

.PHONY: cover
cover: ## Run tests with a coverage summary
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format the tree
	gofmt -w .

.PHONY: check
check: ## Verify formatting, vet, cross-compilation and tests
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	$(MAKE) cross
	go test ./...

cross: ## Vet the tree for every supported platform
	@# CI runs the real suite on all three, but that feedback arrives after a
	@# push. Windows in particular has platform-specific files and build tags, so
	@# vetting it from a developer's machine catches the breakage that a
	@# Linux-or-macOS-only loop would miss. vet compiles the tests too.
	@for target in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		printf 'vet %s/%s\n' "$$os" "$$arch"; \
		GOOS=$$os GOARCH=$$arch go vet ./... || exit 1; \
	done

.PHONY: demo
demo: build ## Print the demo report
	$(BINARY) demo

.PHONY: demo-html
demo-html: build ## Write and open the demo HTML dashboard
	$(BINARY) demo --html -o aiprobe-demo.html
	@command -v open >/dev/null 2>&1 && open aiprobe-demo.html || true

.PHONY: gen
gen: ## Regenerate the synthetic demo session and tool inventory
	go run internal/demo/gen.go

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out aiprobe-demo.html aiprobe-report.html
