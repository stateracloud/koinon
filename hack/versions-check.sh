#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails on drift between hack/versions.env (the one place tool versions live, #544) and the copies
# that cannot read it:
#   - hack/e2e-toolchain/Dockerfile: ARG <TOOL>_VERSION and <TOOL>_SHA256_<arch> for kind, ko, helm, kubectl
#   - every `version:` input of azure/setup-helm (HELM_VERSION) and golangci-lint-action
#     (GOLANGCI_LINT_VERSION) in .github/workflows
#   - the tool table in CONTRIBUTING.md
# and on a second literal of any versions.env name where a consumer should read the file instead: a
# `NAME: value` workflow env entry or a `NAME=value` assignment in hack/*.sh or test/e2e/**/*.sh.
# Also checks the file's own format. Go itself is not here (go.mod is its source).
#
# Usage: hack/versions-check.sh. VERSIONS_CHECK_ROOT=<dir> checks another tree (the test's fixtures).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=${VERSIONS_CHECK_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
cd "$root"

env_file=hack/versions.env
[[ -f $env_file ]] || { echo "versions-check: $env_file missing" >&2; exit 1; }

fails=0
fail() { echo "versions-check: FAIL $*" >&2; fails=$((fails + 1)); }

val() { sed -n "s/^$1=//p" "$env_file" | head -1; }

# 1. The file's own format: NAME=value lines, comments and blank lines only.
while IFS= read -r line; do
	[[ -z $line || $line == \#* ]] && continue
	# The same name rule as hack/versions-env.sh, the filter every workflow loads the file through.
	[[ $line =~ ^[A-Z][A-Z0-9_]*_(VERSION|SHA256_(AMD64|ARM64))=[A-Za-z0-9._+-]+$ ]] || fail "$env_file: not <TOOL>_VERSION or <TOOL>_SHA256_AMD64/ARM64 = value (no quotes or spaces): $line"
	if [[ $line =~ ^[A-Z0-9_]*_SHA256_[A-Z0-9]+=(.*)$ ]]; then
		[[ ${BASH_REMATCH[1]} =~ ^[0-9a-f]{64}$ ]] || fail "$env_file: not a sha256: $line"
	fi
done <"$env_file"
names=$(sed -n 's/^\([A-Z][A-Z0-9_]*\)=.*/\1/p' "$env_file")

# 2. The toolchain Dockerfile's ARGs.
df=hack/e2e-toolchain/Dockerfile
arg() { sed -n "s/^ARG $1=//p" "$df" | head -1; }
for t in KIND KO HELM KUBECTL; do
	want=$(val "${t}_VERSION")
	[[ -n $want && $(arg "${t}_VERSION") == "$want" ]] || fail "$df: ARG ${t}_VERSION is '$(arg "${t}_VERSION")', $env_file has '$want'"
	for a in AMD64 ARM64; do
		low=$(tr '[:upper:]' '[:lower:]' <<<"$a")
		want=$(val "${t}_SHA256_$a")
		[[ -n $want && $(arg "${t}_SHA256_$low") == "$want" ]] || fail "$df: ARG ${t}_SHA256_$low differs from ${t}_SHA256_$a in $env_file"
	done
done

# 3. `version:` inputs that cannot read the file: setup-helm and golangci-lint-action.
# pinned_inputs <workflow> <action substring>: one "line:value" per `version:` after a `uses:` of it.
pinned_inputs() {
	awk -v act="$2" '
		/uses:/ { on = index($0, act) > 0; next }
		on && /^[[:space:]]*version:/ { v = $0; sub(/^[[:space:]]*version:[[:space:]]*/, "", v); sub(/[[:space:]]*#.*/, "", v); print NR ":" v; on = 0 }
	' "$1"
}
for wf in .github/workflows/*.yml; do
	[[ -f $wf ]] || continue
	for pair in "azure/setup-helm HELM_VERSION" "golangci/golangci-lint-action GOLANGCI_LINT_VERSION"; do
		read -r act key <<<"$pair"
		want=$(val "$key")
		while IFS=: read -r n v; do
			[[ -z $n ]] && continue
			[[ $v == "$want" || $v == "\${{ env.$key }}" ]] || fail "$wf:$n: version '$v' for $act, $env_file has $key=$want"
		done < <(pinned_inputs "$wf" "$act")
	done
done

# 4. No second literal where the consumer should read the file.
for n in $names; do
	hits=$(
		grep -nE "^[[:space:]]*${n}:" .github/workflows/*.yml 2>/dev/null || true
		grep -nE "^[[:space:]]*${n}=" hack/*.sh test/e2e/*.sh test/e2e/*/*.sh 2>/dev/null | grep -v "^hack/versions-check-test.sh:" || true
	)
	[[ -z $hits ]] || fail "$n is set outside $env_file (read the file instead): $(head -1 <<<"$hits")"
done

# 4b. `sha256sum -c` input needs two spaces between hash and file ("HASH  FILE"); GNU sha256sum rejects
# one space or none as a malformed line, and only the CI run would show it.
bad=$(grep -nE 'sha256sum -c' .github/workflows/*.yml hack/*.sh test/e2e/*.sh test/e2e/*/*.sh 2>/dev/null |
	grep -E 'SHA256' | grep -vE '\$\{[A-Za-z0-9_]*SHA256[A-Za-z0-9_]*\}  [^ ]' | grep -v '^hack/versions-check' || true)
[[ -z $bad ]] || fail "sha256sum -c line without two spaces after the hash variable: $(head -1 <<<"$bad")"

# 5. CONTRIBUTING.md's tool table.
doc=CONTRIBUTING.md
row_has() { # <row label> <NAME>
	local want
	want=$(val "$2")
	# the whole version token: v4.3.10 is not v4.3.1
	grep -E "^\\| $1 " "$doc" | grep -qE -- "(^|[^A-Za-z0-9._+-])${want//./\\.}([^A-Za-z0-9._+-]|\$)" || fail "$doc: the '$1' row does not contain $want ($2 in $env_file)"
}
row_has Helm HELM_VERSION
row_has kind KIND_VERSION
row_has ko KO_VERSION
row_has gitleaks GITLEAKS_VERSION
row_has golangci-lint GOLANGCI_LINT_VERSION

if ((fails > 0)); then
	echo "versions-check: $fails mismatches. Fix the copy, or bump hack/versions.env with its checksums (CONTRIBUTING.md, 'Tool versions')." >&2
	exit 1
fi
echo "versions-check: ok ($(wc -w <<<"$names" | tr -d ' ') names)"
