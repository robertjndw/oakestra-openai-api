.DEFAULT_GOAL := help
SHELL := /bin/sh

# Tools are pinned in their own module so their dependency graph never
# leaks into go.mod. -modfile is rejected in workspace mode, hence GOWORK=off.
TOOLS_MODFILE := $(CURDIR)/tools/go.mod
GOTOOL := GOWORK=off go tool -modfile=$(TOOLS_MODFILE)

# Pure-Go SQLite means every binary builds without cgo, which keeps the
# multi-arch image builds free of cross C toolchains.
export CGO_ENABLED ?= 0

CMDS := $(notdir $(wildcard cmd/*))

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-14s %s\n", $$1, $$2}'

.PHONY: check
check: ## Everything CI runs: tidy-check, fmt-check, lint, test-race
	@$(MAKE) --no-print-directory tidy-check
	@$(MAKE) --no-print-directory fmt-check
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory test-race

.PHONY: fmt
fmt: ## Format (gofmt + goimports via golangci-lint fmt)
	$(GOTOOL) golangci-lint fmt ./...

.PHONY: fmt-check
fmt-check: ## Check formatting without writing
	$(GOTOOL) golangci-lint fmt --diff ./...

.PHONY: lint
lint: ## golangci-lint run
	$(GOTOOL) golangci-lint run ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: test
test: ## go test -shuffle=on
	go test -shuffle=on ./...

.PHONY: test-race
test-race: ## go test -race -shuffle=on (needs cgo for the race detector)
	CGO_ENABLED=1 go test -race -shuffle=on ./...

.PHONY: build
build: ## Build every cmd/* binary into bin/
	@mkdir -p bin
	@set -e; for c in $(CMDS); do \
		echo "==> build $$c"; \
		go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/$$c ./cmd/$$c; \
	done

.PHONY: tidy
tidy: ## go mod tidy (root and tools)
	go mod tidy
	cd tools && GOWORK=off go mod tidy

.PHONY: tidy-check
tidy-check: ## go mod tidy -diff (CI check)
	go mod tidy -diff
	cd tools && GOWORK=off go mod tidy -diff

.PHONY: vuln
vuln: ## govulncheck
	$(GOTOOL) govulncheck ./...

.PHONY: up
up: ## Build and start the demo stack (needs the dashboard dev stack, see compose.yaml)
	VERSION=$(VERSION) docker compose up -d --build

.PHONY: down
down: ## Stop the demo stack and drop its data volume
	docker compose down --volumes

.PHONY: smoke
smoke: ## End-to-end check against the running demo stack
	deploy/smoke.sh

.PHONY: clean
clean: ## Remove build output
	rm -rf bin
