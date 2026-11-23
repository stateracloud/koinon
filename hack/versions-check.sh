#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails on drift between hack/versions.env (the one place tool versions live, old issue 544) and the copies
# that cannot read it:
#   - hack/e2e-toolchain/Dockerfile: ARG <TOOL>_VERSION and <TOOL>_SHA256_<arch> for kind, ko, helm, kubectl
#   - every `version:` input of azure/setup-helm (HELM_VERSION), golangci-lint-action
#     (GOLANGCI_LINT_VERSION) and bufbuild/buf-action (BUF_VERSION; every use must have one) in .github/workflows
#   - the tool table in CONTRIBUTING.md
#   - mise.toml and mise.lock (ADR 0124): the five lint tools, their versions and linux-x64 checksums
#   - .ko.yaml (#1224): the default base image of ko is cgr.dev/chainguard/static pinned by digest, and every
#     baseImageOverrides entry is pinned by digest too (a tag makes each image build ask the registry)
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
	for pair in "azure/setup-helm HELM_VERSION" "golangci/golangci-lint-action GOLANGCI_LINT_VERSION" "bufbuild/buf-action BUF_VERSION"; do
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

# 4a. Go tools run by version (`go run <module>@vX`) and buf-action without a `version:`: a literal
# version of govulncheck, gofumpt or crane anywhere in workflows or scripts, or a buf-action with no pinned
# version (it would pull "latest", ADR 0079's cooldown cannot apply), is a second copy.
hits=$(grep -nE '(govulncheck|gofumpt|crane)@v?[0-9]' .github/workflows/*.yml hack/*.sh test/e2e/*.sh test/e2e/*/*.sh 2>/dev/null | grep -v '^hack/versions-check' | grep -v '^hack/preflight-test.sh:' || true)
[[ -z $hits ]] || fail "a literal govulncheck/gofumpt/crane version (read GOVULNCHECK_VERSION/GOFUMPT_VERSION/CRANE_VERSION from $env_file): $(head -1 <<<"$hits")"
# Every buf-action step needs `version:` and `checksum:`, and (when they read the env) a step earlier
# in the same job that runs hack/versions-env.sh; without the load both expand to "" and buf-action
# installs the latest buf and skips the checksum.
for wf in .github/workflows/*.yml; do
	[[ -f $wf ]] || continue
	while IFS= read -r msg; do
		[[ -z $msg ]] && continue
		fail "$wf:$msg"
	done < <(awk '
		function finish() {
			if (!in_buf) return
			if (!hv) print ln ": bufbuild/buf-action step without a version: input (an unpinned buf pulls latest)"
			if (!hc) print ln ": bufbuild/buf-action step without a checksum: input"
			in_buf = 0
		}
		/^  [A-Za-z0-9_-]+:[[:space:]]*$/ { finish(); loaded = 0 }
		in_buf && /^[[:space:]]*- / { finish() }
		/versions-env\.sh/ { loaded = 1 }
		/uses:[[:space:]]*bufbuild\/buf-action@/ {
			finish(); in_buf = 1; ln = NR; hv = 0; hc = 0
			if (!loaded) print ln ": bufbuild/buf-action step with no earlier step in the job running hack/versions-env.sh (BUF_VERSION would be empty)"
		}
		in_buf && /^[[:space:]]*version:/ { hv = 1 }
		in_buf && /^[[:space:]]*checksum:/ { hc = 1; c = $0; sub(/^[[:space:]]*checksum:[[:space:]]*/, "", c); sub(/[[:space:]]*#.*/, "", c); if (c != "${{ env.BUF_SHA256_AMD64 }}") print NR ": buf-action checksum '"'"'" c "'"'"' is not ${{ env.BUF_SHA256_AMD64 }}" }
		END { finish() }
	' "$wf")
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

# 6. The mise trial (ADR 0124): mise.toml and mise.lock pin the five lint tools at the versions of
# hack/versions.env, and the lock's linux-x64 checksum is the *_SHA256_AMD64 of the file. Both files are
# optional until the trial starts: with neither there is nothing to compare. mise.toml without mise.lock
# (or the reverse) fails, because a version without a checksum is not a pin.
mise_tools="SHELLCHECK:koalaman/shellcheck ACTIONLINT:rhysd/actionlint ZIZMOR:zizmorcore/zizmor HADOLINT:hadolint/hadolint GITLEAKS:gitleaks/gitleaks"
if [[ -f mise.toml || -f mise.lock ]]; then
	[[ -f mise.toml ]] || fail "mise.lock exists but mise.toml is missing (ADR 0124)"
	[[ -f mise.lock ]] || fail "mise.toml exists but mise.lock is missing: run 'mise lock --platform linux-x64,linux-arm64,macos-arm64' (ADR 0124)"
fi
if [[ -f mise.toml && -f mise.lock ]]; then
	# mise.toml is an allow-list (ADR 0124 decision 5: tool versions only, no [env], [settings], [hooks] or
	# tasks). Every line that is not blank and not a comment is `min_version = "..."`, the `[tools]` header or
	# `"aqua:<owner>/<repo>" = "<version>"`, with whitespace around it and an optional trailing comment.
	# Another tool is outside the trial's scope.
	re_skip='^[[:space:]]*(#.*)?$'
	re_min='^[[:space:]]*min_version[[:space:]]*=[[:space:]]*"[^"]+"[[:space:]]*(#.*)?$'
	re_hdr='^[[:space:]]*\[tools\][[:space:]]*(#.*)?$'
	re_tool='^[[:space:]]*"aqua:([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)"[[:space:]]*=[[:space:]]*"([^"]+)"[[:space:]]*(#.*)?$'
	toml_tools=""
	lineno=0
	while IFS= read -r line || [[ -n $line ]]; do
		lineno=$((lineno + 1))
		if [[ $line =~ $re_skip || $line =~ $re_min || $line =~ $re_hdr ]]; then
			continue
		elif [[ $line =~ $re_tool ]]; then
			ok=0
			for pair in $mise_tools; do [[ ${BASH_REMATCH[1]} == "${pair#*:}" ]] && ok=1; done
			((ok)) || fail "mise.toml: line $lineno '${BASH_REMATCH[1]}' is not one of the five trial tools (a new tool is a new decision, ADR 0124)"
			toml_tools+="${BASH_REMATCH[1]} ${BASH_REMATCH[2]}"$'\n'
		else
			fail "mise.toml: line $lineno is not allowed: '$line' (only min_version, [tools] and \"aqua:owner/repo\" = \"version\" lines; ADR 0124 decision 5)"
		fi
	done <mise.toml
	# mise.lock: "T<TAB>tool" per tool table, "V<TAB>tool<TAB>version" and "C<TAB>tool<TAB>platform<TAB>checksum" per entry.
	lock_facts=$(awk '
		/^\[\[tools\./ { t = $0; sub(/^\[\[tools\./, "", t); sub(/\]\]$/, "", t); gsub(/"/, "", t); tool = t; plat = ""; print "T\t" tool; next }
		/^\[tools\..*platforms\./ {
			t = $0; sub(/^\[tools\./, "", t); sub(/\]$/, "", t); gsub(/"/, "", t)
			i = index(t, ".platforms."); tool = substr(t, 1, i - 1); plat = substr(t, i + 11); print "T\t" tool; next
		}
		/^\[/ { plat = ""; tool = ""; next }
		tool != "" && plat == "" && /^version[[:space:]]*=/ { v = $0; sub(/^[^=]*=[[:space:]]*"/, "", v); sub(/".*/, "", v); print "V\t" tool "\t" v }
		tool != "" && plat != "" && /^checksum[[:space:]]*=/ { c = $0; sub(/^[^=]*=[[:space:]]*"/, "", c); sub(/".*/, "", c); print "C\t" tool "\t" plat "\t" c }
	' mise.lock)
	while IFS=$'\t' read -r kind lock_tool _; do
		[[ $kind == T ]] || continue
		ok=0
		for pair in $mise_tools; do [[ $lock_tool == "aqua:${pair#*:}" ]] && ok=1; done
		((ok)) || fail "mise.lock: '$lock_tool' is not one of the five trial tools (a new tool is a new decision, ADR 0124)"
	done <<<"$lock_facts"
	for pair in $mise_tools; do
		t=${pair%%:*}
		repo=${pair#*:}
		want=$(val "${t}_VERSION")
		have=$(awk -F'\t' -v r="$repo" '$1 == "V" && index($2, r) { print $3 }' <<<"$lock_facts" | head -1)
		have_toml=$(awk -v r="$repo" '$1 == r { print $2 }' <<<"$toml_tools" | head -1)
		[[ -n $have_toml && $have_toml == "$want" ]] || fail "mise.toml: $repo is '$have_toml', $env_file has ${t}_VERSION=$want"
		[[ -n $have && $have == "$want" ]] || fail "mise.lock: $repo is '$have', $env_file has ${t}_VERSION=$want"
		sumwant=$(val "${t}_SHA256_AMD64")
		sum=$(awk -F'\t' -v r="$repo" '$1 == "C" && index($2, r) && $3 == "linux-x64" { print $4 }' <<<"$lock_facts" | head -1)
		[[ $sum == "sha256:$sumwant" ]] || fail "mise.lock: $repo linux-x64 checksum is '$sum', $env_file has ${t}_SHA256_AMD64=$sumwant"
		for plat in linux-arm64 macos-arm64; do
			awk -F'\t' -v r="$repo" -v p="$plat" '$1 == "C" && index($2, r) && $3 == p && $4 ~ /^sha256:[0-9a-f]{64}$/ { f = 1 } END { exit !f }' <<<"$lock_facts" ||
				fail "mise.lock: $repo has no sha256 checksum for $plat (run 'mise lock --platform linux-x64,linux-arm64,macos-arm64')"
		done
	done
fi

# 7. .ko.yaml (#1224): ko's default base image is a digest pin, not the tag `latest`, so one registry fault on a tag
# lookup cannot fail an image build, and the base only changes in a reviewed commit. The re-pin command is in the file.
ko=.ko.yaml
if [[ ! -f $ko ]]; then
	fail "$ko is missing: it pins the default base image of ko by digest (#1224)"
else
	ko_body=$(grep -v '^[[:space:]]*#' "$ko" || true)
	default_ref=$(sed -n 's/^defaultBaseImage:[[:space:]]*//p' <<<"$ko_body" | head -1)
	if [[ $default_ref =~ ^cgr\.dev/chainguard/static@sha256:[0-9a-f]{64}$ ]]; then
		grep -qF 'crane digest cgr.dev/chainguard/static' "$ko" || fail "$ko: no re-pin command ('crane digest cgr.dev/chainguard/static:latest') in a comment"
	else
		fail "$ko: defaultBaseImage is '${default_ref:-<missing>}', want cgr.dev/chainguard/static@sha256:<64 hex> (re-pin: crane digest cgr.dev/chainguard/static:latest; the digest must be 3 days old, ADR 0079, hack/dep-age-check.sh)"
	fi
	# Every base image named under baseImageOverrides is a digest pin as well.
	while IFS= read -r ref; do
		[[ $ref =~ @sha256:[0-9a-f]{64}$ ]] || fail "$ko: baseImageOverrides entry '$ref' is not pinned by digest"
	done < <(sed -n 's/^[[:space:]]\{1,\}[A-Za-z0-9._\/-]\{1,\}:[[:space:]]\{1,\}\(.*\)$/\1/p' <<<"$ko_body")
fi

if ((fails > 0)); then
	echo "versions-check: $fails mismatches. Fix the copy, or bump hack/versions.env with its checksums (CONTRIBUTING.md, 'Tool versions')." >&2
	exit 1
fi
echo "versions-check: ok ($(wc -w <<<"$names" | tr -d ' ') names)"
