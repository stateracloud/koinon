# SPDX-License-Identifier: Apache-2.0

# Module path is also defined in go.mod and internal/project/project.go.
MODULE  := github.com/procoduck/koinon
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(MODULE)/internal/project.Version=$(VERSION)

GO   ?= go
PNPM ?= pnpm

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: web-deps
web-deps: ## Install web dependencies
	cd web && $(PNPM) install --frozen-lockfile

.PHONY: build
build: build-go build-web ## Build everything

.PHONY: build-go
build-go: ## Build Go binaries into bin/
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

.PHONY: build-web
build-web: ## Build the web UI
	cd web && $(PNPM) build

.PHONY: test
test: test-go test-web ## Run all unit tests

.PHONY: test-go
test-go: ## Run Go tests (Postgres tests need Docker)
	$(GO) test ./...

.PHONY: test-web
test-web: ## Run web tests
	cd web && $(PNPM) test

.PHONY: lint
lint: lint-go lint-web lint-proto migrate-lint ## Run all linters

.PHONY: lint-go
lint-go: ## Lint Go code
	golangci-lint run

.PHONY: lint-web
lint-web: ## Lint and typecheck the web UI
	cd web && $(PNPM) lint && $(PNPM) typecheck

.PHONY: lint-proto
lint-proto: ## Lint protobuf definitions
	buf lint

.PHONY: breaking
breaking: ## Check protobuf changes against main
	buf breaking --against '.git#branch=main'

.PHONY: migrate-lint
migrate-lint: ## Check migration file names (and, with BASE=<ref>, that merged migrations are unchanged)
	hack/migrate-lint-test.sh
	hack/migrate-lint.sh $(BASE)

.PHONY: chart-check
chart-check: ## Lint and render the Helm chart in every mode and validate it with kubeconform
	hack/chart-check.sh

.PHONY: generate
# The OpenShell client is generated after buf.gen.yaml, whose clean step empties internal/gen.
generate: ## Regenerate protobuf and sqlc code
	buf generate
	buf generate --template buf.gen.openshell.yaml
	$(GO) tool sqlc generate

# Flavor images build with docker buildx and load into the local Docker image store; nothing is
# pushed. FLAVOR_PLATFORMS=linux/amd64,linux/arm64 builds both (needs the containerd image store).
FLAVOR_TAG       ?= dev
FLAVOR_PLATFORMS ?=

.PHONY: flavor-opencode
flavor-opencode: ## Build the OpenCode flavor image (koinon/flavor-opencode:$(FLAVOR_TAG))
	docker buildx build --load $(if $(FLAVOR_PLATFORMS),--platform $(FLAVOR_PLATFORMS)) \
		--tag koinon/flavor-opencode:$(FLAVOR_TAG) flavors/opencode

.PHONY: test-flavors
test-flavors: flavor-opencode ## Build the flavor images and run the flavor contract suite
	$(GO) test -count=1 ./test/flavors/...

.PHONY: dev
dev: ## Run the inner loop against the home lab with Tilt
	tilt up

.PHONY: clean
clean: ## Remove build output
	rm -rf bin web/dist
