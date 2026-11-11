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
	@case ",$(GO_TAGS)," in *,testing,*|*" testing "*) echo "build-go: the testing tag makes kopia use an insecure key derivation; it is for test binaries only (#576)" >&2; exit 1;; esac
	$(GO) build -tags "$(GO_TAGS)" -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

.PHONY: build-web
build-web: ## Build the web UI
	cd web && $(PNPM) build

.PHONY: test
test: ## Run all unit tests; the web tests run even when the Go run failed or was incomplete (exit 3)
	@rc=0; hack/go-test.sh || rc=$$?; \
	$(MAKE) --no-print-directory test-web || rc=1; \
	exit $$rc

.PHONY: test-go
test-go: ## Run Go tests like CI (Postgres tests need Docker); test/flavors needs images, see test-flavors
	hack/go-test.sh

.PHONY: test-go-suite
test-go-suite: ## Run one parallel Go suite under ginkgo -p: make test-go-suite SUITE=./internal/agentd (one of hack/go-test.sh --list)
	@if [ -z "$$SUITE" ] || ! hack/go-test.sh --list | grep -Fxq -- "$$SUITE"; then \
		echo "test-go-suite: SUITE must be one of (hack/go-test.sh --list):" >&2; hack/go-test.sh --list | sed 's/^/  /' >&2; exit 2; fi
	hack/go-test.sh "$$SUITE"

.PHONY: test-web
test-web: ## Run web tests
	cd web && $(PNPM) test

.PHONY: lint
lint: lint-go lint-web lint-proto migrate-lint adr-lint audit-batch-lint ## Run all linters

.PHONY: lint-go
lint-go: ## Lint Go code (per-worktree lock and cache, #236)
	hack/golangci-lint.sh run

.PHONY: lint-web
lint-web: ## Lint and typecheck the web UI
	cd web && $(PNPM) lint && $(PNPM) typecheck

.PHONY: lint-proto
lint-proto: ## Lint protobuf definitions
	buf lint

.PHONY: breaking
breaking: ## Check protobuf changes against main
	buf breaking --against '.git#branch=main'

.PHONY: audit-batch-lint
audit-batch-lint: ## Check that only internal/audit writes the audit tables (the audit batch, #371)
	hack/audit-batch-lint-test.sh
	hack/audit-batch-lint.sh

.PHONY: adr-lint
adr-lint: ## Check ADR numbers and the README index; offline tests of hack/claim.sh
	hack/adr-lint-test.sh
	hack/adr-lint.sh
	hack/claim_test.sh
	hack/tmp-safety-test.sh
	hack/waiver-approvers-test.sh
	hack/migrate-renumber-test.sh
	hack/hooks/agent-spawn-check_test.sh
	hack/hooks/read-hooks_test.sh
	hack/notes-expiry-test.sh

.PHONY: migrate-lint
migrate-lint: ## Check migration file names (and, with BASE=<ref>, that merged migrations are unchanged; on a patch line also bans RENAME/DROP/type changes: hack/migrate-patch-line naming a shipped line at the merge base, or MIGRATE_LINT_PATCH_LINE=1)
	hack/migrate-lint-test.sh
	hack/migrate-lint.sh $(BASE)

.PHONY: chart-check
chart-check: ## Lint and render the Helm chart in every mode and validate it with kubeconform
	hack/chart-check.sh

.PHONY: generate
# The OpenShell client, the agentgateway ExtMcp server and the googleapis ErrorInfo types are generated after buf.gen.yaml, whose clean step
# empties internal/gen and web/src/gen. hack/generate.sh restores those trees when any step fails (#349).
generate: ## Regenerate protobuf, sqlc and admin API code
	GO=$(GO) hack/generate.sh

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

# GITHUB_TOKEN (set in CI) is handed to the build as the BuildKit secret github_token, which the
# Dockerfile's GitHub release downloads use to authenticate (#145). Unset, they are anonymous.
flavor_secret     = $(if $(GITHUB_TOKEN),--secret id=github_token$(comma)env=GITHUB_TOKEN)

# FLAVOR_BASE=<ref>@sha256:<digest> builds on that base image instead of the Dockerfile's `ARG BASE=`
# default (#200), for example the same image through a registry mirror. It must carry the digest the
# Dockerfile pins: hack/flavor-base.sh checks it and prints the --build-arg (nothing when unset).
FLAVOR_BASE      ?=
export FLAVOR_BASE

FLAVORS          := opencode

.PHONY: flavor-opencode
flavor-opencode: ## Build the OpenCode flavor image (koinon/flavor-opencode:dev-<hash>; FLAVOR_BASE=<ref@digest> to set the base)
	base_args=$$(hack/flavor-base.sh opencode) && \
	docker buildx build --load $(if $(FLAVOR_PLATFORMS),--platform $(FLAVOR_PLATFORMS)) $(call flavor_cache,opencode) $(flavor_secret) $$base_args \
		--build-context common=flavors/common --tag koinon/flavor-opencode:$(call flavor_tag,opencode) flavors/opencode

.PHONY: flavors
flavors: $(addprefix flavor-,$(FLAVORS)) ## Build all flavor images

.PHONY: test-flavors
test-flavors: flavors ## Build the flavor images and run the flavor contract suite
	KOINON_REQUIRE=$${KOINON_REQUIRE:-docker} $(GO) test -count=1 ./test/flavors/...

# The commands of the quickstart and of the backup guide are plain files that test/e2e runs (ADR 0108).
# The docs site is in stateracloud/koinon-docs.
.PHONY: e2e-commands-check
e2e-commands-check: ## Check the e2e command files (test/e2e/commands) and the scripts that run them
	hack/e2e-commands-check.sh

.PHONY: test-e2e-kind
test-e2e-kind: ## Quickstart e2e on a local kind cluster koinon-e1 (KEEP=1 keeps the cluster)
	test/e2e/run.sh

.PHONY: test-e2e-web
test-e2e-web: ## Browser e2e (Playwright, web/e2e-real) against a real install on kind koinon-e1 (KEEP=1 keeps the cluster)
	SUITE=web test/e2e/run.sh

# The escape suite (ADR 0025, plan item E2). The admission cases need only envtest and helm; the
# OpenShell cases need a gateway (KOINON_OPENSHELL_*, see test/escape): `make test-escape-kind` runs
# them against the one `make dev-kind-up` installs (a bare OpenShell, no Koinon policy), `make test-escape`
# against any other (export KOINON_ESCAPE_CLUSTER_DNS, _API and _INTERNAL from that cluster, else the DNS and deny specs skip). The kind e2e runs them too, against the chart install with its sandbox policy live
# (test/e2e/escape-openshell.sh, #109).
ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST       ?= $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2

.PHONY: envtest-assets
envtest-assets: ## Download (or find) the envtest API server binaries and print their directory
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

.PHONY: test-admission
test-admission: ## Run the escape suite's admission policy cases against envtest (needs helm)
	assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" && test -n "$$assets" && \
		KOINON_REQUIRE=$${KOINON_REQUIRE:-envtest} KUBEBUILDER_ASSETS="$$assets" $(GO) test -count=1 -v ./test/escape/ -ginkgo.label-filter=admission

.PHONY: test-chart-smoke
test-chart-smoke: ## Start the chart's rendered Jobs and Deployments as containers (needs Docker, helm, Go)
	assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" && test -n "$$assets" && \
		mkdir -p reports/chartsmoke && rm -f reports/chartsmoke/*.json && \
		{ rc=0; \
		KOINON_REQUIRE=$${KOINON_REQUIRE:-docker,envtest} KUBEBUILDER_ASSETS="$$assets" $(GO) test -count=1 -timeout 20m -v ./test/chartsmoke/ -ginkgo.timeout=15m -ginkgo.json-report=$(CURDIR)/reports/chartsmoke/report.json || rc=1; \
		$(GO) run ./hack/testverdict -reports reports/chartsmoke -name chart-smoke -expect docker,envtest ./test/chartsmoke || rc=1; \
		exit $$rc; }

.PHONY: test-escape
test-escape: ## Run the escape suite's OpenShell cases against a gateway you point it at (KOINON_OPENSHELL_*; export KOINON_ESCAPE_CLUSTER_DNS, _API and _INTERNAL from that cluster, else the DNS and deny specs skip)
	@test -n "$$KOINON_OPENSHELL_ENDPOINT" || { echo "set KOINON_OPENSHELL_* (see test/escape)"; exit 1; }
	$(GO) test -count=1 -v -timeout 20m ./test/escape/ -ginkgo.label-filter=openshell

.PHONY: test-escape-kind
test-escape-kind: ## Run the escape suite's OpenShell cases on the kind-koinon-dev gateway (after make dev-kind-up; runc, Kata cases skip; the kind e2e runs them against the chart install)
	CLUSTER=$(DEV_KIND_CLUSTER) test/escape/kind.sh

# Dev loop (ADR 0039, docs/dev/loop.md). `make dev` picks the profile from the kube context:
# kind-koinon-dev after `make dev-kind-up`, or a cluster of yours listed in hack/dev/local.yaml.
DEV_KIND_CLUSTER ?= koinon-dev

.PHONY: dev
dev: ## Run the inner loop with Tilt against the current kube context (dev-kind-up's kind, or a cluster listed in hack/dev/local.yaml)
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
