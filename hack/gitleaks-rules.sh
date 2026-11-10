#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Regenerates the trace scrubber's credential rules from gitleaks (MIT), pinned at
# GITLEAKS_VERSION in hack/versions.env (ADR 0074, decision 6; issue #56).
#
# It writes internal/tracing/scrub/gitleaksrules/rules.json, the rules the scrubber compiles, and
# nothing else: no key-shaped sample is ever committed. The scrubber's tests generate their samples
# at run time from each rule's regular expression (internal/tracing/scrub/regexgen_test.go).
#
# The script takes a copy of the pinned gitleaks source, runs the rule list through
# hack/gitleaks-rules/export.go.txt in the copy, and keeps only the JSON. Literal strings of the
# rules that look like keys (allowlist examples and the like) are dropped by the exporter.
#
# Updating the rules is a reviewed bump: change GITLEAKS_VERSION (and the CI checksum and GITLEAKS_SRC_SHA256 below), run this
# script, read the diff, run `go test ./internal/tracing/scrub`, commit both.
#
# Usage: hack/gitleaks-rules.sh [--src DIR]   (DIR: an unpacked gitleaks checkout of that version)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=hack/versions.env disable=SC1091
. "$root/hack/versions.env"
# sha256 of the v$GITLEAKS_VERSION source archive (github.com/.../archive/refs/tags); bump with GITLEAKS_VERSION.
GITLEAKS_SRC_SHA256=e90fb266d75837e75894c778bf594ab8e2787f12dce5a62651f21b893eaf9abb

src=""
while [[ $# -gt 0 ]]; do
	case $1 in
	--src) src=$2; shift 2 ;;
	--repo) root=$2; shift 2 ;; # write into this checkout instead of this one
	-h | --help) sed -n '2,19p' "$0"; exit 0 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

work=$(mktemp -d "${TMPDIR:-/tmp}/gitleaks-rules.XXXXXX")
trap 'chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT

if [[ -z $src ]]; then
	"$root/hack/fetch-pinned.sh" \
		"https://github.com/gitleaks/gitleaks/archive/refs/tags/v${GITLEAKS_VERSION}.tar.gz" \
		"$GITLEAKS_SRC_SHA256" "$work/src.tar.gz" >&2
	tar -xz -C "$work" --strip-components=1 -f "$work/src.tar.gz"
	rm -f "$work/src.tar.gz"
else
	cp -R "$src"/. "$work"/
	chmod -R u+w "$work"
fi

gen=$work/cmd/generate
grep -q 'tmpl, err := template.ParseFiles' "$gen/config/main.go" || { echo "gitleaks $GITLEAKS_VERSION: cmd/generate/config/main.go changed shape; update this script" >&2; exit 1; }

# main writes JSON instead of the TOML config.
awk '/tmpl, err := template.ParseFiles/ { exit } { print }' "$gen/config/main.go" >"$work/main.go.new"
printf '\texportRules(ruleLookUp, "%s", gitleaksConfigPath)\n}\n\nvar (\n\t_ = template.New\n\t_ = base.CreateGlobalConfig\n)\n' "$GITLEAKS_VERSION" >>"$work/main.go.new"
mv "$work/main.go.new" "$gen/config/main.go"
cp "$root/hack/gitleaks-rules/export.go.txt" "$gen/config/export.go"

mkdir -p "$work/out" "$root/internal/tracing/scrub/gitleaksrules"
(cd "$work" && GOFLAGS=-mod=mod go run ./cmd/generate/config "$work/out")

cp "$work/out/rules.json" "$root/internal/tracing/scrub/gitleaksrules/rules.json"
cp "$work/LICENSE" "$root/internal/tracing/scrub/gitleaksrules/LICENSE-gitleaks"
echo "wrote rules.json for gitleaks $GITLEAKS_VERSION"
