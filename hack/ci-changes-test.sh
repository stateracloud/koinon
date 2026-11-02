#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/ci-changes.sh (#563): which CI jobs a change to a path turns on. Two parts:
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
# A script nobody runs in a gate: only the Go gates run (any non-doc file sets go).
expect "go" hack/ship.sh
expect "go" hack/ci-changes-test.sh
# web: web/ and api/proto (the ts client is generated from them); `go` ignores web/ files.
expect "web chartsmoke" web/src/app.ts
expect "web chartsmoke docs" web/e2e-real/login.spec.ts # docs-check.sh reads web/e2e-real/*.spec.ts
# The protos: generate, web (the generated client) and chartsmoke (api/ is in its pattern).
expect "go web generate chartsmoke" api/proto/koinon/agent/v1/agent.proto

# F3, generate: the Makefile's `generate` recipe reads each of these.
expect "go generate chartsmoke" api/admin/v1/openapi.yaml       # oapi-codegen input
expect "go generate chartsmoke" api/admin/v1/oapi-codegen.yaml  # oapi-codegen -config
expect "go generate chartsmoke" internal/adminapi/api.gen.go    # oapi-codegen output (checked for drift)
expect "go generate" third_party/openshell/proto/openshell.proto # buf.gen.openshell.yaml input
expect "go generate" buf.gen.openshell.yaml                      # buf generate --template
expect "go generate chartsmoke" internal/db/queries/agents.sql # sqlc.yaml queries
expect "go generate chartsmoke flavors" internal/db/migrations/00001_init.sql # sqlc.yaml schema (store is also built by test/flavors)
expect "go generate" sqlc.yaml
expect "generate chartsmoke web" web/package.json     # buf.gen.yaml runs web/node_modules/.bin/protoc-gen-es
expect "generate chartsmoke web" web/pnpm-lock.yaml   # the version `make web-deps` installs
expect "generate chartsmoke web" web/src/gen/koinon/x_pb.ts # buf.gen.yaml output
# Negative: a hand-written file in the same trees is not a generator input.
expect "go chartsmoke" api/admin/v1/spec.go

# F4, chart: hack/chart-check.sh sources hack/versions.env and hack/chart-check.d/*.sh.
expect "go chart" hack/chart-check.d/10-new-feature.sh
expect "go chart docs" hack/versions.env # also read by docs-check.sh
expect "go chart" hack/chart-check.sh
expect "go chart" hack/chartlint/main.go # `go run ./hack/chartlint`
expect "go chart chartsmoke docs escape" charts/koinon/values.yaml
expect "go chartsmoke" test/chartsmoke/smoke_test.go
expect "go escape" test/escape/escape_test.go

# F5, flavors: test/flavors imports these (go list -deps -test ./test/flavors/...).
expect "go chartsmoke flavors" internal/execution/run.go
expect "go chartsmoke flavors" internal/modelformat/format.go
expect "go chartsmoke flavors" internal/prices/prices.go
expect "go chartsmoke flavors" internal/testing/fakemodel/server.go
expect "go chartsmoke flavors" internal/thinking/thinking.go
expect "go chartsmoke flavors" internal/db/store/models.go
expect "go chartsmoke flavors" pricing/prices.json # pricing/ is embedded by internal/prices; its own package dir
expect "go chartsmoke flavors" internal/agentd/agentd.go # the original filter
expect "go chartsmoke flavors" templates/agent.yaml
expect "go flavors" flavors/opencode/Dockerfile
expect "go flavors" test/flavors/contract_test.go
expect "go flavors" hack/flavor-tag.sh
# Negative: a package the suite does not import (docs-check reads internal/preflight, see below).
expect "go chartsmoke" internal/zzz-not-imported/x.go

# F6, docs: hack/docs-check.sh reads internal/preflight/preflight.go (the versions on the
# Prerequisites page) and runs the tests of internal/declarative.
expect "go chartsmoke docs" internal/preflight/preflight.go
expect "go chartsmoke docs" internal/declarative/docs_test.go
expect "go docs" hack/docs-check.sh
expect "go docs" test/e2e/run.sh # docs-check.sh compares its e1 blocks to the quickstart
expect "go docs" test/e2e/quickstart.golden.sh

# CI itself, the Makefile and the Go module run everything.
all="chart chartsmoke docs escape flavors generate go web"
expect "$all" Makefile
expect "$all" go.mod
expect "$all" go.sum
expect "$all" .github/workflows/ci.yml
expect "$all" hack/ci-changes.sh
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

# generate: the Makefile's recipe names the buf templates, the sqlc config and the oapi-codegen
# config and spec; the templates and configs name the inputs, the outputs and the local plugin.
gen_inputs=(buf.yaml buf.gen.yaml sqlc.yaml)
recipe=$(sed -n '/^generate:/,/^[^[:space:]]/p' Makefile | grep -E '^[[:space:]]' || true)
[[ -n $recipe ]] || { echo "FAIL: no generate recipe found in Makefile"; fails=$((fails + 1)); }
for tok in $recipe; do
	[[ -f $tok ]] && gen_inputs+=("$tok")
done
for tmpl in buf.gen*.yaml; do
	gen_inputs+=("$tmpl")
	# directory: <input dir>, out: <output dir>, local: [..] or path (a node_modules plugin).
	while IFS= read -r v; do
		[[ -n $v ]] || continue
		if [[ $v == web/node_modules/* ]]; then
			gen_inputs+=(web/package.json web/pnpm-lock.yaml)
		else
			gen_inputs+=("$v")
		fi
	done < <(sed -n -e 's/^[[:space:]-]*directory:[[:space:]]*//p' -e 's/^[[:space:]-]*out:[[:space:]]*//p' -e 's/^[[:space:]-]*local:[[:space:]]*\(web\/[^ "]*\).*/\1/p' "$tmpl")
done
while IFS= read -r v; do # sqlc.yaml: schema, queries, out
	[[ -n $v ]] && gen_inputs+=("$v")
done < <(sed -n -e 's/^[[:space:]-]*\(schema\|queries\|out\):[[:space:]]*//p' sqlc.yaml)
while IFS= read -r cfg; do # oapi-codegen -config <file> <spec>: the config, its output, the spec
	gen_inputs+=("$cfg")
	out=$(sed -n 's/^output:[[:space:]]*//p' "$cfg")
	[[ -n $out ]] && gen_inputs+=("$out")
done < <(grep -oE -- '-config [^ ]+' <<<"$recipe" | awk '{print $2}')
# The spec is the recipe's last argument of the oapi-codegen line.
spec=$(grep 'oapi-codegen' <<<"$recipe" | awk '{print $NF}')
[[ -f $spec ]] && gen_inputs+=("$spec")
for p in "${gen_inputs[@]}"; do
	[[ -e $p ]] || { echo "FAIL: derived generator input $p does not exist (the derivation is stale)"; fails=$((fails + 1)); continue; }
	turns_on generate "$(probe "$p")"
done

# chart: hack/chart-check.sh sources `. <file>` and every hack/chart-check.d/*.sh, and runs ./hack/chartlint.
chart_inputs=(hack/chart-check.sh)
while IFS= read -r f; do chart_inputs+=("$f"); done < <(sed -n 's/^\.[[:space:]]\{1,\}\(hack\/[^[:space:]]*\).*/\1/p' hack/chart-check.sh)
for f in hack/chart-check.d/*.sh; do chart_inputs+=("$f"); done
while IFS= read -r f; do chart_inputs+=("${f#./}"); done < <(grep -oE '\./hack/[a-z0-9_/-]+' hack/chart-check.sh | sort -u)
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
[[ ${#docs_inputs[@]} -gt 3 ]] || { echo "FAIL: found only ${#docs_inputs[@]} docs-check inputs (the derivation is stale)"; fails=$((fails + 1)); }
for p in "${docs_inputs[@]}"; do
	turns_on docs "$(probe "$p")"
done

# flavors: every package directory the suite builds from (go list), and a file below it (embedded).
while IFS= read -r d; do
	[[ -n $d ]] || continue
	turns_on flavors "$d/zzz-probe.file"
done <<<"$flavors_dirs"

if [[ $fails -ne 0 ]]; then
	echo "ci-changes-test: $fails of $total checks failed"
	exit 1
fi
echo "ci-changes-test: ok ($total checks)"
