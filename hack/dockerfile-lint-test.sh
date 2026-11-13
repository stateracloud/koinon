#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# hack/dockerfile-lint.sh (#175), offline: the digest and secret rules on fixture Dockerfiles, the
# allow comment, the exit code mapping with a stub hadolint (0 clean, 1 finding, 3 when it is missing,
# 1 when KOINON_REQUIRE names it), and that today's tree passes the awk rules. It never runs the real hadolint.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

# The fixtures and the stub scripts hold a literal ${...} on purpose (a Dockerfile ARG reference, a stub's own
# variables), so single quotes are right and SC2016 does not apply to this file.
# shellcheck disable=SC2016
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
lint=$root/hack/dockerfile-lint.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dockerfile-lint-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail() { echo "dockerfile-lint-test: $*" >&2; exit 1; }

d64=$(printf 'a%.0s' $(seq 64))
mkdir -p "$tmp/bin"
# a stub hadolint: prints its arguments, reports its version, exits HADOLINT_STUB_RC
pin=$(sed -n 's/^HADOLINT_VERSION=//p' "$root/hack/versions.env")
[ -n "$pin" ] || fail "no HADOLINT_VERSION in hack/versions.env"
printf '#!/usr/bin/env bash\nif [ "${1:-}" = --version ]; then echo "Haskell Dockerfile Linter %s"; exit 0; fi\necho "hadolint $*" >>"%s/log"\nexit "${HADOLINT_STUB_RC:-0}"\n' "$pin" "$tmp" >"$tmp/bin/hadolint"
chmod +x "$tmp/bin/hadolint"

df() { # <name> <content>: writes a fixture and prints its path
	printf '%b' "$2" >"$tmp/$1"
	echo "$tmp/$1"
}

run() { # <name> <expected rc> <expected text or ""> <files...>; env VARS passed through
	local name=$1 want=$2 text=$3 rc=0
	shift 3
	HADOLINT=${HADOLINT:-$tmp/bin/hadolint} "$lint" "$@" >"$tmp/out" 2>&1 || rc=$?
	[ "$rc" -eq "$want" ] || fail "$name: exit $rc, want $want ($(cat "$tmp/out"))"
	if [ -n "$text" ]; then grep -qF -- "$text" "$tmp/out" || fail "$name: no '$text' in: $(cat "$tmp/out")"; fi
	echo "ok: $name"
}

good=$(df good "FROM docker.io/library/alpine:3.20@sha256:$d64 AS a\nFROM scratch AS s\nFROM a\nENV GOTOOLCHAIN=local TOKEN_FILE=/run/secrets/t\nARG SHA256_amd64=$d64\n")
run "a pinned Dockerfile passes" 0 "ok (1 Dockerfiles)" "$good"

# --- the digest rule
run "a tag without a digest fails" 1 "digest: base image alpine:3.20" "$(df tag 'FROM alpine:3.20\n')"
run "latest without a digest fails" 1 "digest:" "$(df latest 'FROM ubuntu\n')"
run "a short digest fails" 1 "digest:" "$(df short 'FROM alpine@sha256:abc123\n')"
run "a digest in an ARG default passes" 0 "" "$(df argdig "ARG BASE=alpine:3.20@sha256:$d64\nFROM \${BASE}\n")"
run "an ARG default without a digest fails" 1 "digest: base image \${BASE}" "$(df argtag 'ARG BASE=alpine:3.20\nFROM ${BASE}\n')"
run "an ARG without a default is the caller's" 0 "" "$(df argnone 'ARG BASE\nFROM ${BASE}\n')"
run "tag with an @\${ARG} digest passes when the ARG is a digest" 0 "" "$(df atarg "ARG D=sha256:$d64\nFROM golang:1.27@\${D}\n")"
run "tag with an @\${ARG} digest fails when the ARG is not" 1 "digest:" "$(df atbad 'ARG D=latest\nFROM golang:1.27@${D}\n')"
run "a stage chosen by ARG passes" 0 "" "$(df stagearg "FROM alpine@sha256:$d64 AS fetch-amd64\nARG TARGETARCH\nFROM fetch-\${TARGETARCH} AS fetch\n")"
run "--platform is skipped" 0 "" "$(df plat "FROM --platform=linux/amd64 alpine@sha256:$d64\n")"
run "a line continuation in FROM's neighbours does not hide it" 1 "digest:" "$(df cont 'RUN echo a \\\n  b\nFROM alpine:3\n')"

# --- the secret rule
run "ENV TOKEN fails" 1 "secret: TOKEN" "$(df envtoken "FROM alpine@sha256:$d64\nENV TOKEN=koinon_test_not-a-real-token\n")"
run "ENV with the legacy form fails" 1 "secret: API_KEY" "$(df envlegacy "FROM alpine@sha256:$d64\nENV API_KEY not-a-real-key\n")"
run "ARG with a secret name fails" 1 "secret: GITHUB_TOKEN" "$(df argtoken "FROM alpine@sha256:$d64\nARG GITHUB_TOKEN\n")"
run "a second name in one ENV fails" 1 "secret: DB_PASSWORD" "$(df env2 "FROM alpine@sha256:$d64\nENV A=1 \\\\\n    DB_PASSWORD=x\n")"
run "a _FILE name passes" 0 "" "$(df envfile "FROM alpine@sha256:$d64\nENV TOKEN_FILE=/run/secrets/t\n")"
run "an ordinary name passes" 0 "" "$(df envok "FROM alpine@sha256:$d64\nENV PI_TELEMETRY=0 GOTOOLCHAIN=local\n")"

# --- the allow comment
run "an allow with a reason passes" 0 "" "$(df allow2 '# dockerfile-lint: allow=digest a throwaway spike\nFROM alpine:3\n')"
run "an allow without a reason fails" 1 "digest:" "$(df noreason '# dockerfile-lint: allow=digest\nFROM alpine:3\n')"
run "an allow for another rule fails" 1 "digest:" "$(df otherrule '# dockerfile-lint: allow=secret why\nFROM alpine:3\n')"
run "an allow covers only the next instruction" 1 "digest:" "$(df onlynext '# dockerfile-lint: allow=digest why\nFROM alpine:3 AS a\nFROM alpine:4\n')"
run "an allow=secret passes" 0 "" "$(df allowsec "FROM alpine@sha256:$d64\n# dockerfile-lint: allow=secret a public key id\nENV KEY_TOKEN_ID=x\n")"

# --- hadolint: found, finding, missing
printf 'FROM alpine@sha256:%s\n' "$d64" >"$tmp/ok"
: >"$tmp/log"
run "a clean hadolint passes with the config" 0 "ok (1 Dockerfiles)" "$tmp/ok"
grep -qF -- "--config $root/.hadolint.yaml $tmp/ok" "$tmp/log" || fail "hadolint was not run with the config and the files: $(cat "$tmp/log")"
HADOLINT_STUB_RC=1 run "a hadolint finding fails" 1 "" "$tmp/ok"
HADOLINT=$tmp/bin/nope run "a missing hadolint is INCOMPLETE (3)" 3 "INCOMPLETE (missing hadolint)" "$tmp/ok"
HADOLINT=$tmp/bin/nope KOINON_REQUIRE=docker,hadolint run "a required hadolint fails when missing" 1 "hadolint is required" "$tmp/ok"
HADOLINT=$tmp/bin/nope run "the digest rule still fails with hadolint missing" 1 "digest:" "$tmp/tag"
HADOLINT_STUB_RC=1 run "awk findings fail before hadolint's own verdict" 1 "digest:" "$tmp/tag"
printf '#!/usr/bin/env bash\n[ "${1:-}" != --version ] || { echo "Haskell Dockerfile Linter 0.0.1"; exit 0; }\nexit 0\n' >"$tmp/bin/old"
chmod +x "$tmp/bin/old"
HADOLINT=$tmp/bin/old run "another hadolint version warns" 0 "CI pins hadolint $pin" "$tmp/ok"

# --- the tree: the awk rules on every tracked Dockerfile (hadolint itself is CI's and preflight's)
n=$(git -C "$root" ls-files -- '*Dockerfile' '*Dockerfile.*' '*.Dockerfile' ':!:hack/testdata/*' | wc -l)
[ "$n" -gt 0 ] || fail "found no Dockerfiles in the tree"
rc=0
HADOLINT=$tmp/bin/nope "$lint" >"$tmp/out" 2>&1 || rc=$?
[ "$rc" -eq 3 ] || fail "the tree fails the digest or secret rules (exit $rc): $(cat "$tmp/out")"
echo "ok: the tree passes the digest and secret rules ($n Dockerfiles)"
echo "dockerfile-lint-test: ok"
