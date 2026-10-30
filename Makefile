# SPDX-License-Identifier: Apache-2.0

# Module path is also defined in go.mod and internal/project/project.go.
MODULE  := github.com/stateracloud/koinon
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
build: build-web ## Build everything; koinon-api embeds the web UI
	$(MAKE) build-go GO_TAGS=webui

# GO_TAGS=webui embeds web/dist into koinon-api (build the UI first); without
# it koinon-api serves the API only, and the UI runs on the Vite dev server.
GO_TAGS ?=

.PHONY: build-go
build-go: ## Build Go binaries into bin/
	$(GO) build -tags "$(GO_TAGS)" -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

.PHONY: build-web
build-web: ## Build the web UI
	cd web && $(PNPM) build

.PHONY: test
test: test-go test-web ## Run all unit tests

.PHONY: test-go
test-go: ## Run Go tests like CI (Postgres tests need Docker); test/flavors needs images, see test-flavors
	$(GO) test $$($(GO) list ./... | grep -v '/test/flavors$$')

.PHONY: test-web
test-web: ## Run web tests
	cd web && $(PNPM) test

.PHONY: lint
lint: lint-go lint-web lint-proto migrate-lint adr-lint ## Run all linters

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

.PHONY: adr-lint
adr-lint: ## Check ADR numbers and the README index; offline tests of hack/claim.sh
	hack/adr-lint-test.sh
	hack/adr-lint.sh
	hack/claim_test.sh
	hack/migrate-renumber-test.sh

.PHONY: migrate-lint
migrate-lint: ## Check migration file names (and, with BASE=<ref>, that merged migrations are unchanged)
	hack/migrate-lint-test.sh
	hack/migrate-lint.sh $(BASE)

.PHONY: chart-check
chart-check: ## Lint and render the Helm chart in every mode and validate it with kubeconform
	hack/chart-check.sh

.PHONY: generate
# The OpenShell client is generated after buf.gen.yaml, whose clean step empties internal/gen.
generate: ## Regenerate protobuf, sqlc and admin API code
	buf generate
	buf generate --template buf.gen.openshell.yaml
	$(GO) tool sqlc generate
	$(GO) tool oapi-codegen -config api/admin/v1/oapi-codegen.yaml api/admin/v1/openapi.yaml

# Flavor images build with docker buildx and load into the local Docker image store; nothing is
# pushed. FLAVOR_PLATFORMS=linux/amd64,linux/arm64 builds both (needs the containerd image store).
# The tag is dev-<12 hex of a hash of the flavor's build context> (hack/flavor-tag.sh), so worktrees
# on different branches never overwrite each other's image; test/flavors computes the same tag.
# FLAVOR_TAG=x forces one tag for every flavor.
FLAVOR_TAG       ?=
FLAVOR_PLATFORMS ?=
flavor_tag        = $(or $(FLAVOR_TAG),$(shell hack/flavor-tag.sh $(1)))
# FLAVOR_CACHE=<dir> keeps each flavor's build cache in <dir>/<flavor> (CI saves it between runs);
# it needs a buildx builder with the docker-container driver.
FLAVOR_CACHE     ?=
comma            := ,
flavor_cache      = $(if $(FLAVOR_CACHE),--cache-from type=local$(comma)src=$(FLAVOR_CACHE)/$(1) --cache-to type=local$(comma)dest=$(FLAVOR_CACHE)/$(1)$(comma)mode=max)

FLAVORS          := opencode

.PHONY: flavor-opencode
flavor-opencode: ## Build the OpenCode flavor image (koinon/flavor-opencode:dev-<hash>)
	docker buildx build --load $(if $(FLAVOR_PLATFORMS),--platform $(FLAVOR_PLATFORMS)) $(call flavor_cache,opencode) \
		--build-context common=flavors/common --tag koinon/flavor-opencode:$(call flavor_tag,opencode) flavors/opencode

.PHONY: flavors
flavors: $(addprefix flavor-,$(FLAVORS)) ## Build all flavor images

.PHONY: test-flavors
test-flavors: flavors ## Build the flavor images and run the flavor contract suite
	$(GO) test -count=1 ./test/flavors/...

# Docs site (plan item DOC1): Starlight in docs/site. The quickstart's `sh e1=<name>` blocks are
# what test/e2e/run.sh runs; docs-check keeps the doc, the e2e script and the code in step.
.PHONY: docs
docs: ## Build the docs site into docs/site/dist
	cd docs/site && $(PNPM) install --frozen-lockfile && ASTRO_TELEMETRY_DISABLED=1 $(PNPM) build

.PHONY: docs-check
docs-check: ## Check the docs against test/e2e, the chart and the web spec (needs helm; "hack/docs-check.sh --update" rewrites the golden file)
	hack/docs-check.sh

.PHONY: test-e2e-kind
test-e2e-kind: ## Quickstart e2e on a local kind cluster koinon-e1 (KEEP=1 keeps the cluster)
	test/e2e/run.sh

.PHONY: test-e2e-web
test-e2e-web: ## Browser e2e (Playwright, web/e2e-real) against a real install on kind koinon-e1 (KEEP=1 keeps the cluster)
	SUITE=web test/e2e/run.sh

# The escape suite (ADR 0025, plan item E2). The admission cases need only envtest and helm; the
# OpenShell cases need a gateway (KOINON_OPENSHELL_*, see test/escape): `make test-escape-kind` runs
# them against the one `make dev-kind-up` installs, `make test-escape` against any other.
ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST       ?= $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2

.PHONY: test-admission
test-admission: ## Run the escape suite's admission policy cases against envtest (needs helm)
	assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" && test -n "$$assets" && \
		KUBEBUILDER_ASSETS="$$assets" $(GO) test -count=1 -v ./test/escape/ -ginkgo.label-filter=admission

.PHONY: test-escape
test-escape: ## Run the escape suite's OpenShell cases against a gateway you point it at (KOINON_OPENSHELL_*)
	@test -n "$$KOINON_OPENSHELL_ENDPOINT" || { echo "set KOINON_OPENSHELL_* (see test/escape)"; exit 1; }
	$(GO) test -count=1 -v -timeout 20m ./test/escape/ -ginkgo.label-filter=openshell

.PHONY: test-escape-kind
test-escape-kind: ## Run the escape suite's OpenShell cases on the kind-koinon-dev gateway (after make dev-kind-up; runc, Kata cases skip)
	CLUSTER=$(DEV_KIND_CLUSTER) test/escape/kind.sh

# Dev loop (ADR 0039, docs/dev/loop.md). `make dev` picks the profile from the kube context:
# homelab/homelab-koinon, or kind-koinon-dev after `make dev-kind-up`.
DEV_KIND_CLUSTER ?= koinon-dev

.PHONY: dev
dev: ## Run the inner loop with Tilt against the current kube context (home lab or dev-kind-up's kind)
	tilt up

.PHONY: dev-kind-up
dev-kind-up: ## Create the kind cluster kind-koinon-dev with Koinon's prerequisites (CNPG, Agent Sandbox, OpenShell)
	CLUSTER=$(DEV_KIND_CLUSTER) test/e2e/kind/up.sh

.PHONY: dev-kind-down
dev-kind-down: ## Delete the kind cluster kind-koinon-dev
	CLUSTER=$(DEV_KIND_CLUSTER) test/e2e/kind/down.sh

.PHONY: clean
clean: ## Remove build output
	rm -rf bin web/dist
