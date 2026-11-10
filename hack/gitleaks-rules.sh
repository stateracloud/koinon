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
# Updating the rules is a reviewed bump: change GITLEAKS_VERSION (and the CI checksum and GITLEAKS_MODULE_SUM below), run this
# script, read the diff, run `go test ./internal/tracing/scrub`, commit both.
#
# Usage: hack/gitleaks-rules.sh [--src DIR]   (DIR: an unpacked gitleaks checkout of that version)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=hack/versions.env disable=SC1091
. "$root/hack/versions.env"
# The gitleaks source comes from the Go module proxy, not a GitHub tag archive: GitHub does not
# promise byte-stable archives, so a sha256 of one can break CI with no change on our side. A module
# zip is immutable in the proxy and its h1: hash is a digest of the file contents, so the pin below
# is content-addressed. `go mod download` also checks it against the public checksum database
# (sum.golang.org), and we compare it with the pin. The tree needs the whole module (cmd/generate
# plus config, cmd/generate/config/rules, and its go.mod), so a few raw files would not do.
# Bump with GITLEAKS_VERSION: go mod download -json github.com/gitleaks/gitleaks/v8@v<version> (field Sum).
GITLEAKS_MODULE_SUM=h1:47Qm4HBVPbTTKH85chTXyHuVM5i7JT0vv3DjAApVay8=

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
	mod=github.com/gitleaks/gitleaks/v8@v${GITLEAKS_VERSION}
	info=$(cd "$work" && GOFLAGS='' go mod download -json "$mod") || { echo "go mod download $mod failed" >&2; exit 1; }
	got=$(printf '%s\n' "$info" | sed -n 's/^[[:space:]]*"Sum": "\(.*\)",\{0,1\}$/\1/p')
	dir=$(printf '%s\n' "$info" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",\{0,1\}$/\1/p')
	[[ $got == "$GITLEAKS_MODULE_SUM" ]] || { echo "gitleaks $GITLEAKS_VERSION: module hash $got, pinned $GITLEAKS_MODULE_SUM" >&2; exit 1; }
	cp -R "$dir"/. "$work"/
	chmod -R u+w "$work"
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
