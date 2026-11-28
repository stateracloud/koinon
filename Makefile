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
	@case " $$(printf '%s' '$(GO_TAGS)' | tr -d "\"'\\\\" | tr ',' ' ') " in *" testing "*) echo "build-go: the testing tag makes kopia use an insecure key derivation; it is for test binaries only (#576)" >&2; exit 1;; esac
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
test-go: ## Run Go tests like CI (Postgres tests need Docker); test/harnesses needs images, see test-harnesses
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

.PHONY: tidy
tidy: ## Report finished worktrees, merged branches and old claims (APPLY=1 cleans, DOCKER=1 adds Docker; hack/cmd/worktidy)
	$(GO) tool worktidy $(if $(APPLY),--apply) $(if $(DOCKER),--docker)

.PHONY: lint-go
lint-go: ## Lint Go code (per-worktree lock and cache, #236)
	hack/golangci-lint.sh run

.PHONY: lint-web
lint-web: ## Lint and typecheck the web UI
	cd web && $(PNPM) lint && $(PNPM) typecheck

.PHONY: lint-proto
lint-proto: ## Lint protobuf definitions
	hack/buf.sh lint

.PHONY: breaking
breaking: ## Check protobuf changes against main
	hack/buf.sh breaking --against '.git#branch=main'

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
	hack/approve-pr-test.sh
	hack/answer-question-test.sh
	hack/migrate-renumber-test.sh
	hack/hooks/agent-spawn-check_test.sh
	hack/hooks/read-hooks_test.sh
	hack/hooks/sleep-chain-check_test.sh
	hack/hooks/codegraph-prompt-check_test.sh
	hack/hooks/context-cap-check_test.sh
	hack/hooks/grilling-question-check_test.sh
	hack/hooks/prose-question-stop-check_test.sh
	hack/hooks/question-checklist-check_test.sh
	hack/notes-expiry-test.sh

.PHONY: migrate-lint
migrate-lint: ## Check migration file names (and, with BASE=<ref>, that merged migrations are unchanged; on a patch line also bans RENAME/DROP/type changes: hack/migrate-patch-line naming a shipped line at the merge base, or MIGRATE_LINT_PATCH_LINE=1)
	hack/migrate-lint-test.sh
	hack/migrate-lint.sh $(BASE)

.PHONY: chart-check
chart-check: ## Lint and render the Helm chart in every mode and validate it with kubeconform
	hack/chart-check.sh

.PHONY: generate
# The OpenShell and Substrate clients, the agentgateway ExtMcp server and the googleapis ErrorInfo types are generated after buf.gen.yaml, whose clean step
# empties internal/gen and web/src/gen. hack/generate.sh restores those trees when any step fails (#349).
generate: ## Regenerate protobuf, sqlc and admin API code
	GO=$(GO) hack/generate.sh

# Harness images build with docker buildx and load into the local Docker image store; nothing is
# pushed. HARNESS_PLATFORMS=linux/amd64,linux/arm64 builds both (needs the containerd image store).
# The tag is dev-<12 hex of a hash of the harness's build context> (hack/harness-tag.sh), so worktrees
# on different branches never overwrite each other's image; test/harnesses computes the same tag.
# HARNESS_TAG=x forces one tag for every harness.
# The FLAVOR_* names are the earlier ones, read until the workflows use the new ones (#610).
HARNESS_TAG       ?= $(FLAVOR_TAG)
HARNESS_PLATFORMS ?= $(FLAVOR_PLATFORMS)
harness_tag        = $(or $(HARNESS_TAG),$(shell hack/harness-tag.sh $(1)))
# HARNESS_CACHE=<dir> keeps each harness's build cache in <dir>/<harness> (CI saves it between runs);
# it needs a buildx builder with the docker-container driver.
HARNESS_CACHE     ?= $(FLAVOR_CACHE)
comma            := ,
harness_cache      = $(if $(HARNESS_CACHE),--cache-from type=local$(comma)src=$(HARNESS_CACHE)/$(1) --cache-to type=local$(comma)dest=$(HARNESS_CACHE)/$(1)$(comma)mode=max)

# GITHUB_TOKEN (set in CI) is handed to the build as the BuildKit secret github_token, which the
# Dockerfile's GitHub release downloads use to authenticate (#145). Unset, they are anonymous.
harness_secret     = $(if $(GITHUB_TOKEN),--secret id=github_token$(comma)env=GITHUB_TOKEN)

# HARNESS_BASE=<ref>@sha256:<digest> builds on that base image instead of the Dockerfile's `ARG BASE=`
# default (#200), for example the same image through a registry mirror. It must carry the digest the
# Dockerfile pins: hack/harness-base.sh checks it and prints the --build-arg (nothing when unset).
HARNESS_BASE      ?= $(FLAVOR_BASE)
export HARNESS_BASE

HARNESSES          := opencode

.PHONY: harness-opencode
harness-opencode: ## Build the OpenCode harness image (koinon/sandbox:dev-<hash>; HARNESS_BASE=<ref@digest> to set the base)
	base_args=$$(hack/harness-base.sh opencode) && \
	docker buildx build --load $(if $(HARNESS_PLATFORMS),--platform $(HARNESS_PLATFORMS)) $(call harness_cache,opencode) $(harness_secret) $$base_args \
		--build-context common=harnesses/common --tag koinon/sandbox:$(call harness_tag,opencode) harnesses/opencode

# The substrate overlay (ADR 0119, issue #1087): the harness image plus koinon-guest-front as its
# entrypoint (the guest API, #1309) and koinon-guest-launcher as the exec prefix of every command. Only the
# substrate driver uses it; the OpenShell images above do not change.
.PHONY: guest-launcher
guest-launcher: ## Build the static guest front and guest launcher for linux/amd64 and linux/arm64 into build/guest-launcher
	for arch in amd64 arm64; do \
		mkdir -p build/guest-launcher/linux-$$arch && \
		for cmd in koinon-guest-launcher koinon-guest-front; do \
			CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags='-s -w' \
				-o build/guest-launcher/linux-$$arch/$$cmd ./cmd/$$cmd || exit 1; \
		done; \
	done

# The upstream Substrate guest server, from the commit ATE_ENV_VERSION of hack/versions.env (upstream publishes no
# image). The overlay does not use it: its guest API is koinon-guest-front (#1309).
.PHONY: env-guest
env-guest: ## Build the static Substrate guest server (ate-env-guest) for linux/amd64 and linux/arm64 into build/env-guest
	hack/build-env-guest.sh build/env-guest

# One overlay per harness: make harness-substrate-<name> (not listed by `make help`; harnesses-substrate is) for every name in HARNESSES.
# The overlay carries the guest front and the launcher, and its ENTRYPOINT is the guest front. The harness entry script
# (ADR 0048) is started by the driver through the guest, behind the launcher prefix. A later harness gets an overlay with no Makefile change.
SUBSTRATE_HARNESSES := $(addprefix harness-substrate-,$(HARNESSES))
.PHONY: $(SUBSTRATE_HARNESSES) harness-substrate-%
$(SUBSTRATE_HARNESSES): harness-substrate-%: harness-% guest-launcher ## Build a harness image with the guest front and launcher (koinon/sandbox:<tag>-substrate)
	hack/harness-substrate-build.sh koinon/sandbox:$(call harness_tag,$*) koinon/sandbox:$(call harness_tag,$*)-substrate $(HARNESS_PLATFORMS)

.PHONY: harnesses-substrate
harnesses-substrate: $(SUBSTRATE_HARNESSES) ## Build the substrate overlay of every harness image

.PHONY: harnesses
harnesses: $(addprefix harness-,$(HARNESSES)) ## Build all harness images

# The earlier target names, kept until the workflows use the new ones (#610).
.PHONY: flavors flavor-opencode test-flavors
flavors: harnesses
flavor-opencode: harness-opencode
test-flavors: test-harnesses

.PHONY: test-harnesses
test-harnesses: harnesses ## Build the harness images and run the harness contract suite
	$(MAKE) harness-suite

# The suite alone, against images that are already built (ci.yml builds them with its layer cache first).
# One -timeout for local and CI runs: Go's default of 10 minutes made the compaction spec fail for time (#980, #990).
# hack/paths-test.sh fails when the Makefile has no -timeout here or ci.yml stops calling this target.
.PHONY: harness-suite
harness-suite: ## Run the harness contract suite against built images
	KOINON_REQUIRE=$${KOINON_REQUIRE:-docker} $(GO) test -count=1 -timeout 30m ./test/harnesses/...

.PHONY: test-opencode-plugin
test-opencode-plugin: ## Type-check and unit-test Koinon's OpenCode plugins (ADR 0093; needs pnpm, no Docker)
	cd harnesses/opencode/plugins && $(PNPM) install --frozen-lockfile $(if $(KOINON_PNPM_STORE),--store-dir "$(KOINON_PNPM_STORE)") && $(PNPM) run typecheck && $(PNPM) run test

# The commands of the quickstart and of the backup guide are plain files that test/e2e runs (ADR 0108).
# The docs site is in stateracloud/koinon-docs.
.PHONY: e2e-commands-check
e2e-commands-check: ## Check the e2e command files (test/e2e/commands) and the scripts that run them
	hack/e2e-commands-check.sh

.PHONY: test-e2e-kind
test-e2e-kind: ## Quickstart e2e and the run-1 story suite on a local kind cluster koinon-e1, OpenShell driver until #1094 (KEEP=1 keeps the cluster)
	DRIVER=openshell STORIES=1 test/e2e/run.sh

.PHONY: test-e2e-kind-substrate
test-e2e-kind-substrate: ## Quickstart e2e on kind with execution.driver substrate on the gVisor class (issue #1091; KEEP=1 keeps the cluster; docs/dev/ci.md)
	DRIVER=substrate ESCAPE_OPENSHELL=0 test/e2e/run.sh

.PHONY: test-fence-kind
test-fence-kind: ## Prove the Substrate fence on its own throwaway kind cluster (API server ports rule; needs Docker, kind, helm 4; KEEP_CLUSTER=1 keeps it)
	test/e2e/kind/apiserver-egress.sh

.PHONY: test-e2e-web
test-e2e-web: ## Browser e2e (Playwright, web/e2e-real) against a real install on kind koinon-e1 (KEEP=1 keeps the cluster)
	DRIVER=openshell SUITE=web test/e2e/run.sh

# The escape suite (ADR 0025, plan item E2). The admission cases need only envtest and helm; the
# OpenShell cases need a gateway (KOINON_OPENSHELL_*, see test/escape): `make test-escape-kind` runs
# them against the one `make dev-kind-up` installs (a bare OpenShell, no Koinon policy), `make test-escape`
# against any other (export KOINON_ESCAPE_CLUSTER_DNS, _API and _INTERNAL from that cluster, else the DNS and deny specs skip). The kind e2e runs them too, against the chart install with its sandbox policy live
# (test/e2e/escape-openshell.sh, #109).
ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST       ?= $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
# KOINON_ENVTEST_ASSETS=<dir> skips setup-envtest: hack/lane-gates.sh finds the assets once with
# `make envtest-assets` and gives the directory to each envtest gate (#1296). The name is Koinon's
# own, so a KUBEBUILDER_ASSETS of another version in your shell never replaces the pin above.
envtest_assets       = $${KOINON_ENVTEST_ASSETS:-$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)}

.PHONY: envtest-assets
envtest-assets: ## Download (or find) the envtest API server binaries and print their directory
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

.PHONY: test-admission
test-admission: ## Run the escape suite's admission policy cases against envtest (needs helm)
	assets="$(envtest_assets)" && test -n "$$assets" && \
		KOINON_REQUIRE=$${KOINON_REQUIRE:-envtest,substrate-policy} KUBEBUILDER_ASSETS="$$assets" $(GO) test -count=1 -v ./test/escape/ -ginkgo.label-filter=admission

# The install modes run in shards (issue #1303): CI runs one process per shard, each with the 15 minute
# budget below. CHART_SMOKE_SHARD picks one (core, external or kms; the groups are in
# test/chartsmoke/shard_test.go); empty runs every mode serially, which needs about three budgets.
CHART_SMOKE_SHARD ?=
# Without a shard the run is serial and gets a longer budget (it took 13 minutes in a lane VM).
ifneq ($(filter-out all,$(CHART_SMOKE_SHARD)),)
CHART_SMOKE_BUDGET ?= 15m
CHART_SMOKE_GO_TIMEOUT ?= 20m
CHART_SMOKE_LABEL = -$(CHART_SMOKE_SHARD)
else
CHART_SMOKE_BUDGET ?= 25m
CHART_SMOKE_GO_TIMEOUT ?= 30m
CHART_SMOKE_LABEL =
endif

.PHONY: test-chart-smoke
test-chart-smoke: ## Start the chart's rendered Jobs and Deployments as containers (needs Docker, helm, Go). One shard of the install modes: CHART_SMOKE_SHARD=core|external|kms (default: all, serial)
	assets="$(envtest_assets)" && test -n "$$assets" && \
		mkdir -p reports/chartsmoke && rm -f reports/chartsmoke/*.json && \
		{ rc=0; \
		KOINON_CHARTSMOKE_SHARD=$${KOINON_CHARTSMOKE_SHARD:-$(CHART_SMOKE_SHARD)} KOINON_REQUIRE=$${KOINON_REQUIRE:-docker,envtest} KUBEBUILDER_ASSETS="$$assets" $(GO) test -count=1 -timeout $(CHART_SMOKE_GO_TIMEOUT) -v ./test/chartsmoke/ -ginkgo.timeout=$(CHART_SMOKE_BUDGET) -ginkgo.json-report=$(CURDIR)/reports/chartsmoke/report.json || rc=1; \
		$(GO) run ./hack/testverdict -reports reports/chartsmoke -name chart-smoke$(CHART_SMOKE_LABEL) -expect docker,envtest,volumecopy ./test/chartsmoke || rc=1; \
		exit $$rc; }

.PHONY: test-escape
test-escape: ## Run the escape suite's OpenShell cases against a gateway you point it at (KOINON_OPENSHELL_*; export KOINON_ESCAPE_CLUSTER_DNS, _API and _INTERNAL from that cluster, else the DNS and deny specs skip)
	@test -n "$$KOINON_OPENSHELL_ENDPOINT" || { echo "set KOINON_OPENSHELL_* (see test/escape)"; exit 1; }
	$(GO) test -count=1 -v -timeout 20m ./test/escape/ -ginkgo.label-filter=openshell

.PHONY: test-escape-substrate
test-escape-substrate: ## Qualify one Substrate isolation tier (default hardened = gVisor) on a cluster you point it at (KOINON_ESCAPE_SUBSTRATE_*; runbook: docs/dev/escape-substrate.md)
	assets="$(envtest_assets)" && test -n "$$assets" && \
		KOINON_ESCAPE_DRIVER=substrate KOINON_REQUIRE=$${KOINON_REQUIRE:+$$KOINON_REQUIRE,}escape-cluster-ips,host-kernel,substrate-targets,substrate-snapshots,envtest KUBEBUILDER_ASSETS="$$assets" \
		$(GO) test -count=1 -v -timeout 45m ./test/escape/ -ginkgo.label-filter=substrate -ginkgo.timeout=40m

.PHONY: test-escape-kind
test-escape-kind: ## Run the escape suite's OpenShell cases on the kind-koinon-dev gateway (after make dev-kind-up; runc, Kata cases skip; the kind e2e runs them against the chart install)
	CLUSTER=$(DEV_KIND_CLUSTER) test/escape/kind.sh

# Dev loop (ADR 0039, docs/dev/loop.md). `make dev` picks the profile from the kube context:
# kind-koinon-dev after `make dev-kind-up`, or a cluster of yours listed in hack/dev/local.yaml.
DEV_KIND_CLUSTER ?= koinon-dev

.PHONY: dev
dev: ## Run the inner loop with Tilt against the current kube context (dev-kind-up's kind, or a cluster listed in hack/dev/local.yaml)
	tilt up

.PHONY: load-gateway
load-gateway: ## Gateway load suite (issue 280): cloud only, never in CI or `make test`; see docs/dev/load-tests.md
	hack/load-gateway.sh $(LOAD_ARGS)

.PHONY: dev-kind-up
dev-kind-up: ## Create the kind cluster kind-koinon-dev with Koinon's prerequisites (CNPG, Agent Sandbox, OpenShell)
	CLUSTER=$(DEV_KIND_CLUSTER) test/e2e/kind/up.sh

.PHONY: dev-kind-down
dev-kind-down: ## Delete the kind cluster kind-koinon-dev
	CLUSTER=$(DEV_KIND_CLUSTER) test/e2e/kind/down.sh

.PHONY: clean
clean: ## Remove build output
	rm -rf bin web/dist
