#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/ci-changes.sh (old issue 563): which CI jobs a change to a path turns on. Two parts:
#   1. A table of path(s) -> the jobs that must run, a positive and a negative case for every
#      group, and one row for each input the filters once missed (audit F3-F6).
#   2. Derived cases: every input the gate scripts really read must turn its job on. The inputs are
#      read from the gate scripts and the generator configs, so a new input that a filter misses
#      fails here instead of silently skipping the job (a skipped job counts as success).
# Run it with no arguments; it needs the Go toolchain (`go list`, once).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."

# First, and on its own line so set -e stops the script when mktemp fails (old issue 476).
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-changes-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fails=0 total=0
script=hack/ci-changes.sh

# The flavors and chartsmoke suite package directories, computed once (the script's own `go list`);
# the table rows reuse them; rows at the end run the real `go list` path.
flavors_dirs=$(bash $script --flavors-dirs)
[[ -n $flavors_dirs ]] || { echo "FAIL: $script --flavors-dirs printed nothing"; exit 1; }
chartsmoke_dirs=$(bash $script --chartsmoke-dirs)
[[ -n $chartsmoke_dirs ]] || { echo "FAIL: $script --chartsmoke-dirs printed nothing"; exit 1; }

# jobs_for <paths...>: the sorted jobs that turn on, space separated.
jobs_for() {
	printf '%s\n' "$@" | CI_CHANGES_FLAVORS_DIRS=$flavors_dirs CI_CHANGES_CHARTSMOKE_DIRS=$chartsmoke_dirs bash $script --stdin | sed -n 's/^\(.*\)=true$/\1/p' | sort | tr '\n' ' '
}

# The path-gate jobs of #572 (tidy: go-tidy and go-lint, shell: lint-shell, scripts) and #579 (vuln: go-vuln) and #684 (fuzz) have their own table in
# section 1b; `expect` leaves them out of the comparison so the heavy-job table stays about the heavy jobs.
gate_jobs='tidy|vuln|fuzz|shell|scripts'

# expect <expected jobs> <path>...: the paths together must turn on exactly these jobs (the path-gate jobs aside).
expect() {
	local want got
	want=$(tr ' ' '\n' <<<"$1" | sed '/^$/d' | sort | tr '\n' ' ')
	shift
	total=$((total + 1))
	got=$(jobs_for "$@" | tr ' ' '\n' | grep -vxE "$gate_jobs" | tr '\n' ' ' || true)
	if [[ $got != "$want" ]]; then
		echo "FAIL: $* -> [${got% }] want [${want% }]"
		fails=$((fails + 1))
	fi
}

# expect_gates <expected path-gate jobs> <path>...: of tidy, vuln, fuzz, shell and scripts, exactly these turn on.
expect_gates() {
	local want got
	want=$(tr ' ' '\n' <<<"$1" | sed '/^$/d' | sort | tr '\n' ' ')
	shift
	total=$((total + 1))
	got=$(jobs_for "$@" | tr ' ' '\n' | grep -xE "$gate_jobs" | tr '\n' ' ' || true)
	if [[ $got != "$want" ]]; then
		echo "FAIL: $* -> gates [${got% }] want [${want% }]"
		fails=$((fails + 1))
	fi
}

# turns_on <job> <path>: the path must turn the job on (the derived cases).
turns_on() {
	local job=$1
	shift
	total=$((total + 1))
	if ! grep -qw -- "$job" <<<"$(jobs_for "$@")"; then
		echo "FAIL: $* does not turn on $job"
		fails=$((fails + 1))
	fi
}

# --- 1. The table. A comment names the file a job reads for each row. ------------------------------

# Documentation only: no CI job reads it (the docs site is in stateracloud/koinon-docs, ADR 0108).
expect "" README.md
expect "" docs/adr/0001-x.md
# Shell tooling and other workflows cannot affect the Go jobs (fail-closed list: hack/paths.rules);
# the scripts' own tests run in the always-on `scripts` job.
expect "" hack/ship.sh
expect "" hack/ci-changes-test.sh
expect "" hack/hooks/bash-read-check.sh
expect "" .github/workflows/e2e-kind.yml
expect "" .github/actions/fetch-pinned/action.yml
# ...but the scripts the Go jobs run, a non-script file of hack/ and an unknown path do.
expect "go" hack/go-test.sh
expect "go" hack/golangci-lint.sh
expect "go" hack/agentgateway-pin.sh
expect "go" hack/zzz-unknown.txt
expect "go" some-new-top-level-file
expect "go" hack/pgserver/main.go
expect "go web chartsmoke" web/webui.go
expect "go web chartsmoke" web/webui_stub.go
# keep must not reinclude web/*_test.go over except (#524 Bugbot); chartsmoke stays off.
expect "go web" web/webui_test.go
# web: web/ and api/proto (the ts client is generated from them); `go` ignores web/ files. The
# chart smoke builds without the webui tag, so web/ sources do not reach it.
expect "web" web/src/app.ts
expect "web" web/e2e-real/login.spec.ts
# The protos: generate and web (the generated client). The chart smoke ignores them: the generated
# Go code is committed under internal/gen, where a change matches.
expect "go web generate" api/proto/koinon/agent/v1/agent.proto
expect "go" internal/zzz-new/x.go # not in the chartsmoke or flavors build graph
expect "go" internal/zzz-new/x_test.go # tests of other packages are not in a binary or the suite's imports

# F3, generate: the Makefile's `generate` recipe reads each of these.
expect "go generate chartsmoke" api/admin/v1/openapi.yaml       # oapi-codegen input (embedded; chartsmoke graph)
expect "go generate chartsmoke" api/admin/v1/oapi-codegen.yaml  # oapi-codegen -config
expect "go generate chartsmoke" internal/adminapi/api.gen.go    # oapi-codegen output (checked for drift)
expect "go generate" third_party/openshell/proto/openshell.proto # buf.gen.openshell.yaml input
expect "go generate" buf.gen.openshell.yaml                      # buf generate --template
expect "go generate" third_party/agentgateway/proto/ext_mcp.proto # buf.gen.agentgateway.yaml input
expect "go generate" internal/db/queries/agents.sql # sqlc.yaml queries (not a Go package; store regen turns chartsmoke on)
expect "go generate chartsmoke flavors" internal/db/migrations/00001_init.sql # sqlc.yaml schema (store is also built by test/harnesses)
expect "go generate" sqlc.yaml
expect "generate web" web/package.json     # buf.gen.yaml runs web/node_modules/.bin/protoc-gen-es
expect "generate web" web/pnpm-lock.yaml   # the version `make web-deps` installs
expect "generate web" web/src/gen/koinon/x_pb.ts # buf.gen.yaml output
expect "generate" hack/generate.sh         # make generate delegates here (#349)
expect "generate go" cmd/protoc-gen-koinon-doc/doc.go
expect "generate" docs/reference/proto/proto-api.md   # buf.gen.docs.yaml output
# Negative: a hand-written file in the same trees is not a generator input.
expect "go chartsmoke" api/admin/v1/spec.go

# F4, chart: hack/chart-check.sh sources hack/versions.env and hack/chart-check.d/*.sh.
# versions.env is also a chartsmoke seed input (old issue 110).
expect "chart" hack/chart-check.d/10-new-feature.sh
expect "go chart chartsmoke" hack/versions.env
expect "chart" hack/chart-check.sh
expect "go chart" hack/chartlint/main.go # `go run ./hack/chartlint`
expect "go chart chartsmoke escape" charts/koinon/values.yaml
expect "go chartsmoke" test/chartsmoke/smoke_test.go
expect "go escape" test/escape/escape_test.go
expect "go chartsmoke escape flavors" internal/testing/testenv/testenv.go # the admission specs import it

# F5, flavors: test/harnesses imports these (go list -deps -test ./test/harnesses/...).
expect "go chartsmoke flavors" internal/execution/run.go
expect "go chartsmoke flavors" internal/modelformat/format.go
expect "go chartsmoke flavors" internal/prices/prices.go
expect "go flavors" internal/testing/fakemodel/server.go # flavors only; not in the chartsmoke graph
expect "go chartsmoke flavors" internal/thinking/thinking.go
expect "go chartsmoke flavors generate" internal/db/store/models.go # sqlc.yaml out: a hand edit of generated code must run the check
expect "go chartsmoke flavors" pricing/prices.json # pricing/ is embedded by internal/prices; its own package dir
expect "go chartsmoke flavors" internal/agentd/agentd.go # the original filter
expect "go chartsmoke flavors" templates/agent.yaml
expect "go flavors opencodeplugin" harnesses/opencode/Dockerfile
expect "go flavors" test/harnesses/contract_test.go
expect "go flavors" hack/harness-tag.sh
expect "flavors" hack/probe-tag.sh
expect "" hack/harness-repro-check.sh # its own advisory workflow (flavor-repro.yml), not a ci.yml job
expect "go flavors" hack/spike/probe/Dockerfile
# Negative: a package neither suite imports.
expect "go" internal/zzz-not-imported/x.go

# F6: a package in the smoke build graph; foreign _test.go and the e2e scripts are Go-only.
expect "go chartsmoke" internal/preflight/preflight.go
expect "go" internal/declarative/docs_test.go
expect "go" test/e2e/run.sh
expect "go" test/e2e/commands/quickstart.e1

# CI itself, the Makefile and the Go module run everything.
all="chart chartsmoke escape flavors generate go opencodeplugin web"
expect "$all" Makefile
expect "$all" go.mod
expect "$all" go.sum
expect "$all" .github/workflows/ci.yml
expect "$all" hack/ci-changes.sh
expect "$all" hack/ci-skip-heavy.sh
expect "$all" hack/paths.rules
expect "$all" hack/paths-lib.sh
# CI_CHANGES_IGNORE_PLUMBING=1 (hack/pr-merge.sh --admin) drops the CI-plumbing rule but keeps the build inputs.
expect_ignoring() { # <want flavors value> <paths...>
	local want=$1 got
	shift
	got=$(printf '%s\n' "$@" | CI_CHANGES_IGNORE_PLUMBING=1 CI_CHANGES_FLAVORS_DIRS=$flavors_dirs CI_CHANGES_CHARTSMOKE_DIRS=$chartsmoke_dirs bash $script --stdin | sed -n 's/^flavors=//p')
	total=$((total + 1))
	if [[ $got != "$want" ]]; then
		echo "FAIL: ignoring plumbing, flavors for [$*]: want $want, got $got"
		fails=$((fails + 1))
	fi
}
for f in .github/workflows/ci.yml hack/ci-changes.sh hack/ci-skip-heavy.sh hack/paths.rules hack/paths-lib.sh; do
	expect_ignoring false "$f"
done
expect_ignoring false .github/workflows/ci.yml .github/CODEOWNERS
expect_ignoring true Makefile
expect_ignoring true go.mod
expect_ignoring true go.sum
expect_ignoring true .github/workflows/ci.yml harnesses/base/Dockerfile
expect_ignoring true .github/workflows/ci.yml internal/execution/run.go
expect_ignoring true # an empty change list still fails closed
# Several paths together: the union.
expect "go chart chartsmoke escape generate web" charts/koinon/Chart.yaml sqlc.yaml web/src/a.ts

# --- 1b. The path gates of #572: go-tidy and go-lint (tidy), lint-shell (shell) and scripts. -------------------
# tidy: Go files, go.mod/go.sum and the lint config. shell: shell scripts, .github/, the tool pins.
# scripts: fail closed; only Markdown, docs/ (except docs/adr and docs/dev/ci.md, which checks read) and web/src
# cannot affect it (hack/paths.rules says why).
expect_gates "" README.md
expect_gates "" docs/design.md
expect_gates "" docs/dev/other.md
expect_gates "" web/src/app.ts
expect_gates "" web/src/gen/koinon/x_pb.ts
expect_gates "scripts" docs/adr/0001-x.md
expect_gates "scripts" docs/dev/ci.md
expect_gates "scripts" AGENTS.md
expect_gates "scripts" CONTRIBUTING.md
expect_gates "scripts" web/package.json
expect_gates "scripts" web/pnpm-lock.yaml
expect_gates "scripts" charts/koinon/values.yaml
expect_gates "scripts" test/e2e/commands/quickstart.e1
expect_gates "scripts" harnesses/opencode/Dockerfile
expect_gates "scripts" some-new-top-level-file
expect_gates "tidy scripts" internal/zzz-new/x.go
expect_gates "tidy scripts" web/webui.go
expect_gates "tidy scripts" .golangci.yml
expect_gates "shell scripts" hack/ship.sh
expect_gates "shell scripts" hack/ci-changes-test.sh
expect_gates "shell scripts" .github/workflows/e2e-kind.yml
expect_gates "shell scripts" .github/actions/fetch-pinned/action.yml
expect_gates "shell scripts" internal/agentd/savework.sh
expect_gates "tidy scripts" hack/pgserver/main.go
expect_gates "tidy vuln fuzz shell scripts" go.mod
expect_gates "tidy vuln fuzz shell scripts" go.sum
expect_gates "tidy vuln fuzz shell scripts" Makefile
expect_gates "tidy vuln fuzz shell scripts" .github/workflows/ci.yml
expect_gates "tidy vuln fuzz shell scripts" hack/paths.rules
# vuln (go-vuln, govulncheck, #579 part 2): only the module files (and CI plumbing, through `everything`).
expect_gates "tidy scripts" internal/zzz-new/x.go # a Go file alone does not run govulncheck
expect_gates "" docs/dev/ci-rules.md # a docs-only change does not
expect_gates "vuln shell scripts" hack/govulncheck.sh # the check itself
expect_gates "vuln shell scripts" .github/workflows/govulncheck.yml
expect_gates "vuln shell scripts" hack/versions.env # accepted cost: every tool bump runs govulncheck
expect_gates "scripts" hack/spike/acpdrive/go.mod # nested spike modules are not scanned
expect_gates "scripts" hack/spike/acpdrive/go.sum
expect_gates "shell scripts" hack/govulncheck-test.sh
expect_gates "shell scripts" docs/design.md hack/ship.sh # the union: one trigger is enough
# fuzz (#684): the fuzzed packages and the script, not every Go file.
expect_gates "tidy fuzz scripts" internal/tracing/scrub/jsonscrub.go
expect_gates "tidy fuzz scripts" internal/leakguard/zzz_test.go
expect_gates "fuzz shell scripts" hack/go-fuzz.sh

# The real `go list` path (no CI_CHANGES_*_DIRS): a dependency of each suite, and one neither has.
# Read the script's output fully before grepping: flavors and chartsmoke are not the last jobs, so `grep -q` in
# a pipefail pipeline would SIGPIPE the script mid-write and look like a miss.
total=$((total + 4))
out=$(printf 'internal/execution/run.go\n' | bash $script --stdin) || true
if ! grep -qx 'flavors=true' <<<"$out"; then
	echo "FAIL: go list path: internal/execution/run.go does not turn on flavors"
	fails=$((fails + 1))
fi
out=$(printf 'internal/zzz-not-imported/x.go\n' | bash $script --stdin) || true
if grep -qx 'flavors=true' <<<"$out"; then
	echo "FAIL: go list path: an unrelated package turns on flavors"
	fails=$((fails + 1))
fi
out=$(printf 'internal/preflight/preflight.go\n' | bash $script --stdin) || true
if ! grep -qx 'chartsmoke=true' <<<"$out"; then
	echo "FAIL: go list path: internal/preflight/preflight.go does not turn on chartsmoke"
	fails=$((fails + 1))
fi
out=$(printf 'internal/zzz-not-imported/x.go\n' | bash $script --stdin) || true
if grep -qx 'chartsmoke=true' <<<"$out"; then
	echo "FAIL: go list path: an unrelated package turns on chartsmoke"
	fails=$((fails + 1))
fi

# --- 2. Derived cases: the inputs the gate scripts and generator configs name. ---------------------

# probe <path>: a file inside the path when it is a directory (a change below it), else the path.
probe() {
	if [[ -d $1 ]]; then echo "${1%/}/zzz-probe.file"; else echo "$1"; fi
}

# need_min <what> <minimum> <found>: a derivation that parses too little is a failure, not a pass.
need_min() {
	total=$((total + 1))
	if [[ $3 -lt $2 ]]; then
		echo "FAIL: derived only $3 $1 (want at least $2); the derivation is stale or the parser is not portable"
		fails=$((fails + 1))
	fi
}

# generate: the Makefile's recipe names the buf templates, the sqlc config and the oapi-codegen
# config and spec; the templates and configs name the inputs, the outputs and the local plugin.
# When make generate delegates to hack/generate.sh (#349), parse that script's body too.
# Portable sed only (-E; BSD sed has no \|).
gen_inputs=(buf.yaml buf.gen.yaml sqlc.yaml)
recipe=$(sed -n '/^generate:/,/^[^[:space:]]/p' Makefile | grep -E '^[[:space:]]' || true)
# Follow a single-line recipe that is only `GO=… hack/generate.sh` (or bare hack/generate.sh).
gen_script=""
if [[ $(grep -c . <<<"$recipe" || true) -lt 4 ]]; then
	for tok in $recipe; do
		case $tok in
		hack/generate.sh | */hack/generate.sh) gen_script=$tok ;;
		esac
	done
	[[ -n $gen_script && -f $gen_script ]] && { recipe=$(cat "$gen_script"); gen_inputs+=("$gen_script"); }
fi
need_min "lines in the Makefile generate recipe" 4 "$(grep -c . <<<"$recipe" || true)"
for tok in $recipe; do
	[[ -f $tok ]] && gen_inputs+=("$tok")
done
# buf.yaml: modules: - path: <module dir> (the protos buf generate reads).
mods=0
while IFS= read -r v; do
	[[ -n $v ]] && { gen_inputs+=("$v"); mods=$((mods + 1)); }
done < <(sed -n -E 's/^[[:space:]-]*path:[[:space:]]*//p' buf.yaml)
need_min "buf.yaml modules" 1 $mods
templates=0 tmplin=0
for tmpl in buf.gen*.yaml; do
	templates=$((templates + 1))
	gen_inputs+=("$tmpl")
	# directory: <input dir>, out: <output dir>, local: [..] or path (a node_modules plugin).
	while IFS= read -r v; do
		[[ -n $v ]] || continue
		tmplin=$((tmplin + 1))
		if [[ $v == web/node_modules/* ]]; then
			gen_inputs+=(web/package.json web/pnpm-lock.yaml)
		else
			gen_inputs+=("$v")
		fi
	done < <(sed -n -E -e 's/^[[:space:]-]*directory:[[:space:]]*//p' -e 's/^[[:space:]-]*out:[[:space:]]*//p' -e 's/^[[:space:]-]*local:[[:space:]]*(web\/[^ "]*).*/\1/p' "$tmpl")
done
need_min "buf.gen*.yaml templates" 3 $templates
need_min "inputs, outputs and plugins read from the buf templates" 6 $tmplin
sqlcin=0
while IFS= read -r v; do # sqlc.yaml: schema, queries, out
	[[ -n $v ]] && { gen_inputs+=("$v"); sqlcin=$((sqlcin + 1)); }
done < <(sed -n -E 's/^[[:space:]-]*(schema|queries|out):[[:space:]]*//p' sqlc.yaml)
need_min "sqlc.yaml paths (schema, queries, out)" 3 $sqlcin
oapi=0
while IFS= read -r cfg; do # oapi-codegen -config <file> <spec>: the config, its output, the spec
	gen_inputs+=("$cfg")
	oapi=$((oapi + 1))
	out=$(sed -n -E 's/^output:[[:space:]]*//p' "$cfg")
	[[ -n $out ]] && { gen_inputs+=("$out"); oapi=$((oapi + 1)); }
done < <(grep -oE -- '-config [^ ]+' <<<"$recipe" | awk '{print $2}')
# The spec is the last argument of the oapi-codegen invocation (not a comment that
# merely mentions the tool). Strip a trailing line-continuation backslash (#349).
spec=$(grep -E '^[^#]*[[:space:]]oapi-codegen[[:space:]]' <<<"$recipe" | sed -E 's/[[:space:]]*\\$//' | awk '{print $NF}' | tr -d '"')
[[ -f $spec ]] && { gen_inputs+=("$spec"); oapi=$((oapi + 1)); }
need_min "oapi-codegen config, output and spec" 3 $oapi
need_min "generator inputs in all" 15 ${#gen_inputs[@]}
for p in "${gen_inputs[@]}"; do
	[[ -e $p ]] || { echo "FAIL: derived generator input $p does not exist (the derivation is stale)"; fails=$((fails + 1)); continue; }
	turns_on generate "$(probe "$p")"
done

# chart: hack/chart-check.sh sources `. <file>` and every hack/chart-check.d/*.sh, and runs ./hack/chartlint.
chart_inputs=(hack/chart-check.sh)
sourced=0
while IFS= read -r f; do chart_inputs+=("$f"); sourced=$((sourced + 1)); done < <(sed -n -E 's/^\.[[:space:]]+(hack\/[^[:space:]]*).*/\1/p' hack/chart-check.sh)
need_min "files sourced by chart-check.sh" 1 $sourced
parts=0
for f in hack/chart-check.d/*.sh; do chart_inputs+=("$f"); parts=$((parts + 1)); done
need_min "hack/chart-check.d assertion files" 5 $parts
runs=0
while IFS= read -r f; do chart_inputs+=("${f#./}"); runs=$((runs + 1)); done < <(grep -oE '\./hack/[a-z0-9_/-]+' hack/chart-check.sh | sort -u)
need_min "./hack tools run by chart-check.sh" 1 $runs
chart_inputs+=(charts/koinon/Chart.yaml)
for p in "${chart_inputs[@]}"; do
	[[ -e $p ]] || { echo "FAIL: derived chart input $p does not exist"; fails=$((fails + 1)); continue; }
	turns_on chart "$(probe "$p")"
done

# flavors, independent of the script: a real `go list` of the suite (import paths, not .Dir, so
# no path stripping is shared with ci-changes.sh). The packages the audit named, and the suite
# itself, must be in it; then each package directory (and a file below it) must turn flavors on.
mod=$(go list -m)
real_dirs=$(GOOS=linux go list -deps -test -f '{{.ImportPath}}' ./test/harnesses/... | awk '{print $1}' | grep -F "$mod/" | grep -v '\.test$' | sed -e "s#^$mod/##" -e 's#_test$##' | sort -u) # test/flavors_test is the external test package of test/flavors
for pkg in test/harnesses internal/execution internal/modelformat internal/prices internal/testing/fakemodel internal/thinking pricing internal/db/store; do
	total=$((total + 1))
	if ! grep -qxF "$pkg" <<<"$real_dirs"; then
		echo "FAIL: go list of test/harnesses does not contain $pkg (the audit's F5 package)"
		fails=$((fails + 1))
	fi
done
need_min "packages in the flavors build graph" 30 "$(grep -c . <<<"$real_dirs" || true)"
while IFS= read -r d; do
	[[ -n $d ]] || continue
	turns_on flavors "$d/zzz-probe.file"
done <<<"$real_dirs"
# Fail closed: a dirs list without test/flavors (a path strip that found nothing) turns flavors on.
total=$((total + 1))
out=$(printf 'internal/zzz-not-imported/x.go\n' | CI_CHANGES_FLAVORS_DIRS=$'internal/execution\ninternal/prices' CI_CHANGES_CHARTSMOKE_DIRS=$chartsmoke_dirs bash $script --stdin) || true
if ! grep -qx 'flavors=true' <<<"$out"; then
	echo "FAIL: a dirs list without test/flavors must fail closed (flavors=true)"
	fails=$((fails + 1))
fi

# chartsmoke: real `go list` of the suite plus the three cmd binaries images_test builds; seed
# inputs the graph cannot name (charts/, hack/versions.env) must also turn the job on.
cs_suite=$(GOOS=linux go list -deps -test -f '{{.ImportPath}}' ./test/chartsmoke/... | awk '{print $1}' | grep -F "$mod/" | grep -v '\.test$' | sed -e "s#^$mod/##" -e 's#_test$##' | sort -u)
cs_cmds=$(GOOS=linux go list -deps -f '{{.ImportPath}}' ./cmd/koinon-api ./cmd/koinon-agentd ./cmd/koinon-reconciler | awk '{print $1}' | grep -F "$mod/" | sed -e "s#^$mod/##" | sort -u)
cs_dirs=$(printf '%s\n%s\n' "$cs_suite" "$cs_cmds" | sort -u)
for pkg in test/chartsmoke cmd/koinon-api cmd/koinon-agentd cmd/koinon-reconciler internal/preflight web api/admin/v1; do
	total=$((total + 1))
	if ! grep -qxF "$pkg" <<<"$cs_dirs"; then
		echo "FAIL: go list of chartsmoke does not contain $pkg"
		fails=$((fails + 1))
	fi
done
need_min "packages in the chartsmoke build graph" 50 "$(grep -c . <<<"$cs_dirs" || true)"
while IFS= read -r d; do
	[[ -n $d ]] || continue
	# A .go probe: except chartsmoke ^web/ keeps only web/*.go (web/src and package.json stay off).
	turns_on chartsmoke "$d/zzz-probe.go"
done <<<"$cs_dirs"
# Seed inputs go list cannot name (old issue 110).
for p in charts/koinon/values.yaml hack/versions.env test/chartsmoke/smoke_test.go; do
	turns_on chartsmoke "$p"
done
# Fail closed: a dirs list without test/chartsmoke turns chartsmoke on.
total=$((total + 1))
out=$(printf 'internal/zzz-not-imported/x.go\n' | CI_CHANGES_FLAVORS_DIRS=$flavors_dirs CI_CHANGES_CHARTSMOKE_DIRS=$'cmd/koinon-api\ninternal/preflight' bash $script --stdin) || true
if ! grep -qx 'chartsmoke=true' <<<"$out"; then
	echo "FAIL: a dirs list without test/chartsmoke must fail closed (chartsmoke=true)"
	fails=$((fails + 1))
fi

# An empty change list is not "nothing changed": it runs everything (fail closed).
total=$((total + 1))
all=$(bash $script | sed -n 's/^\(.*\)=true$/\1/p' | sort | tr '\n' ' ')
[[ -n $all ]] || { echo "FAIL: no-argument run printed no jobs"; exit 1; }
if [[ $(printf '' | CI_CHANGES_FLAVORS_DIRS=$flavors_dirs CI_CHANGES_CHARTSMOKE_DIRS=$chartsmoke_dirs bash $script --stdin | sed -n 's/^\(.*\)=true$/\1/p' | sort | tr '\n' ' ') != "$all" ]]; then
	echo "FAIL: an empty change list must turn every job on"
	fails=$((fails + 1))
fi

# A rename lists the old path too (--no-renames): moving a chart file to docs/ still turns the chart jobs on.
total=$((total + 1))
(
	cd "$tmp"
	git init -q -b main .
	git config user.email t@example.invalid && git config user.name t
	mkdir charts docs hack # the script cds to its own checkout: give the scratch repo a copy
	cp "$OLDPWD/hack/ci-changes.sh" "$OLDPWD/hack/paths-lib.sh" "$OLDPWD/hack/paths.rules" hack/
	echo a >charts/a.yaml && echo b >docs/b.md
	git add -A && git commit -q -m base
	git mv charts/a.yaml docs/a.yaml && git commit -q -m rename
	CI_CHANGES_FLAVORS_DIRS=$flavors_dirs CI_CHANGES_CHARTSMOKE_DIRS=$chartsmoke_dirs bash hack/ci-changes.sh HEAD~1
) >"$tmp/rename.out" 2>&1 || true
if ! grep -qx 'chart=true' "$tmp/rename.out"; then
	echo "FAIL: renaming charts/a.yaml to docs/ must turn chart on: $(tr '\n' ' ' <"$tmp/rename.out")"
	fails=$((fails + 1))
fi

if [[ $fails -ne 0 ]]; then
	echo "ci-changes-test: $fails of $total checks failed"
	exit 1
fi
echo "ci-changes-test: ok ($total checks)"
