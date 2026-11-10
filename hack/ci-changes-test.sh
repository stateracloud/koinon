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

# The flavors suite's package directories, computed once (the script's own `go list`); the table
# rows reuse them, three rows at the end run the real `go list` path.
flavors_dirs=$(bash $script --flavors-dirs)
[[ -n $flavors_dirs ]] || { echo "FAIL: $script --flavors-dirs printed nothing"; exit 1; }

# jobs_for <paths...>: the sorted jobs that turn on, space separated.
jobs_for() {
	printf '%s\n' "$@" | CI_CHANGES_FLAVORS_DIRS=$flavors_dirs bash $script --stdin | sed -n 's/^\(.*\)=true$/\1/p' | sort | tr '\n' ' '
}

# expect <expected jobs> <path>...: the paths together must turn on exactly these jobs.
expect() {
	local want got
	want=$(tr ' ' '\n' <<<"$1" | sed '/^$/d' | sort | tr '\n' ' ')
	shift
	total=$((total + 1))
	got=$(jobs_for "$@")
	if [[ $got != "$want" ]]; then
		echo "FAIL: $* -> [${got% }] want [${want% }]"
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

# Documentation only: docs reads docs/ and README.md (hack/docs-check.sh); nothing needs Go.
expect "docs" README.md
expect "docs" docs/adr/0001-x.md
expect "docs" docs/site/src/content/docs/quickstart.md
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
# web: web/ and api/proto (the ts client is generated from them); `go` ignores web/ files. The
# chart smoke builds without the webui tag, so web/ sources do not reach it.
expect "web" web/src/app.ts
expect "web docs" web/e2e-real/login.spec.ts # docs-check.sh reads web/e2e-real/*.spec.ts
# The protos: generate and web (the generated client). The chart smoke ignores them: the generated
# Go code is committed under internal/gen, where a change matches.
expect "go web generate" api/proto/koinon/agent/v1/agent.proto
expect "go chartsmoke" internal/zzz-new/x.go
expect "go" internal/zzz-new/x_test.go # tests of other packages are not in a binary or the suite's imports

# F3, generate: the Makefile's `generate` recipe reads each of these.
expect "go generate chartsmoke" api/admin/v1/openapi.yaml       # oapi-codegen input
expect "go generate chartsmoke" api/admin/v1/oapi-codegen.yaml  # oapi-codegen -config
expect "go generate chartsmoke" internal/adminapi/api.gen.go    # oapi-codegen output (checked for drift)
expect "go generate" third_party/openshell/proto/openshell.proto # buf.gen.openshell.yaml input
expect "go generate" buf.gen.openshell.yaml                      # buf generate --template
expect "go generate" third_party/agentgateway/proto/ext_mcp.proto # buf.gen.agentgateway.yaml input
expect "go generate chartsmoke" internal/db/queries/agents.sql # sqlc.yaml queries
expect "go generate chartsmoke flavors" internal/db/migrations/00001_init.sql # sqlc.yaml schema (store is also built by test/flavors)
expect "go generate" sqlc.yaml
expect "generate web" web/package.json     # buf.gen.yaml runs web/node_modules/.bin/protoc-gen-es
expect "generate web" web/pnpm-lock.yaml   # the version `make web-deps` installs
expect "generate web" web/src/gen/koinon/x_pb.ts # buf.gen.yaml output
# Negative: a hand-written file in the same trees is not a generator input.
expect "go chartsmoke" api/admin/v1/spec.go

# F4, chart: hack/chart-check.sh sources hack/versions.env and hack/chart-check.d/*.sh.
expect "chart" hack/chart-check.d/10-new-feature.sh
expect "go chart docs" hack/versions.env # also read by docs-check.sh
expect "chart" hack/chart-check.sh
expect "go chart" hack/chartlint/main.go # `go run ./hack/chartlint`
expect "go chart chartsmoke docs escape" charts/koinon/values.yaml
expect "go chartsmoke" test/chartsmoke/smoke_test.go
expect "go escape" test/escape/escape_test.go
expect "go chartsmoke escape flavors" internal/testing/testenv/testenv.go # the admission specs import it

# F5, flavors: test/flavors imports these (go list -deps -test ./test/flavors/...).
expect "go chartsmoke flavors" internal/execution/run.go
expect "go chartsmoke flavors" internal/modelformat/format.go
expect "go chartsmoke flavors" internal/prices/prices.go
expect "go chartsmoke flavors" internal/testing/fakemodel/server.go
expect "go chartsmoke flavors" internal/thinking/thinking.go
expect "go chartsmoke flavors generate" internal/db/store/models.go # sqlc.yaml out: a hand edit of generated code must run the check
expect "go chartsmoke flavors" pricing/prices.json # pricing/ is embedded by internal/prices; its own package dir
expect "go chartsmoke flavors" internal/agentd/agentd.go # the original filter
expect "go chartsmoke flavors" templates/agent.yaml
expect "go flavors" flavors/opencode/Dockerfile
expect "go flavors" test/flavors/contract_test.go
expect "go flavors" hack/flavor-tag.sh
expect "flavors" hack/probe-tag.sh
expect "go flavors" hack/spike/probe/Dockerfile
# Negative: a package the suite does not import (docs-check reads internal/preflight, see below).
expect "go chartsmoke" internal/zzz-not-imported/x.go

# F6, docs: hack/docs-check.sh reads internal/preflight/preflight.go (the versions on the
# Prerequisites page) and runs the tests of internal/declarative.
expect "go chartsmoke docs" internal/preflight/preflight.go
expect "go docs" internal/declarative/docs_test.go # a test file of another package is not in the smoke build
expect "docs" hack/docs-check.sh
expect "go docs" test/e2e/run.sh # docs-check.sh compares its e1 blocks to the command file
expect "go docs" test/e2e/commands/quickstart.e1

# CI itself, the Makefile and the Go module run everything.
all="chart chartsmoke docs escape flavors generate go web"
expect "$all" Makefile
expect "$all" go.mod
expect "$all" go.sum
expect "$all" .github/workflows/ci.yml
expect "$all" hack/ci-changes.sh
expect "$all" hack/ci-skip-heavy.sh
expect "$all" hack/paths.rules
expect "$all" hack/paths-lib.sh
# Several paths together: the union.
expect "go chart chartsmoke docs escape generate web" charts/koinon/Chart.yaml sqlc.yaml web/src/a.ts

# The real `go list` path (no CI_CHANGES_FLAVORS_DIRS): a dependency of the suite, and one it lacks.
total=$((total + 2))
if ! printf 'internal/execution/run.go\n' | bash $script --stdin | grep -qx 'flavors=true'; then
	echo "FAIL: go list path: internal/execution/run.go does not turn on flavors"
	fails=$((fails + 1))
fi
if printf 'internal/zzz-not-imported/x.go\n' | bash $script --stdin | grep -qx 'flavors=true'; then
	echo "FAIL: go list path: an unrelated package turns on flavors"
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
# Portable sed only (-E; BSD sed has no \|).
gen_inputs=(buf.yaml buf.gen.yaml sqlc.yaml)
recipe=$(sed -n '/^generate:/,/^[^[:space:]]/p' Makefile | grep -E '^[[:space:]]' || true)
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
# The spec is the recipe's last argument of the oapi-codegen line.
spec=$(grep 'oapi-codegen' <<<"$recipe" | awk '{print $NF}')
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

# docs: every repository path hack/docs-check.sh names (outside comments) must turn docs on.
docs_inputs=()
while IFS= read -r tok; do
	tok=${tok#./}
	tok=${tok%%[*?{]*} # up to the first glob character
	tok=${tok%/}
	[[ -n $tok && -e $tok ]] && docs_inputs+=("$tok")
done < <(grep -v '^[[:space:]]*#' hack/docs-check.sh | grep -oE '(\./)?(internal|hack|test|web|charts|docs|cmd|pkg)/[A-Za-z0-9_./*-]+' | sort -u)
docs_inputs+=(README.md)
need_min "docs-check inputs" 8 ${#docs_inputs[@]}
for p in "${docs_inputs[@]}"; do
	turns_on docs "$(probe "$p")"
done

# flavors, independent of the script: a real `go list` of the suite (import paths, not .Dir, so
# no path stripping is shared with ci-changes.sh). The packages the audit named, and the suite
# itself, must be in it; then each package directory (and a file below it) must turn flavors on.
mod=$(go list -m)
real_dirs=$(GOOS=linux go list -deps -test -f '{{.ImportPath}}' ./test/flavors/... | awk '{print $1}' | grep -F "$mod/" | grep -v '\.test$' | sed -e "s#^$mod/##" -e 's#_test$##' | sort -u) # test/flavors_test is the external test package of test/flavors
for pkg in test/flavors internal/execution internal/modelformat internal/prices internal/testing/fakemodel internal/thinking pricing internal/db/store; do
	total=$((total + 1))
	if ! grep -qxF "$pkg" <<<"$real_dirs"; then
		echo "FAIL: go list of test/flavors does not contain $pkg (the audit's F5 package)"
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
if ! printf 'internal/zzz-not-imported/x.go\n' | CI_CHANGES_FLAVORS_DIRS=$'internal/execution\ninternal/prices' bash $script --stdin | grep -qx 'flavors=true'; then
	echo "FAIL: a dirs list without test/flavors must fail closed (flavors=true)"
	fails=$((fails + 1))
fi

# An empty change list is not "nothing changed": it runs everything (fail closed).
total=$((total + 1))
all=$(bash $script | sed -n 's/^\(.*\)=true$/\1/p' | sort | tr '\n' ' ')
[[ -n $all ]] || { echo "FAIL: no-argument run printed no jobs"; exit 1; }
if [[ $(printf '' | CI_CHANGES_FLAVORS_DIRS=$flavors_dirs bash $script --stdin | sed -n 's/^\(.*\)=true$/\1/p' | sort | tr '\n' ' ') != "$all" ]]; then
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
	CI_CHANGES_FLAVORS_DIRS=$flavors_dirs bash hack/ci-changes.sh HEAD~1
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
