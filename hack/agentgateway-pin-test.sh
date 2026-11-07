#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/agentgateway-pin.sh (#238): it reads gateway.standalone.image from a values
# file (and not another component's image block), and --check fails on a tag-only pin, a short or
# malformed digest, a workflow that does not read the pin, a workflow with a literal digest, a Go
# test digest or repository that differs from the pin, and a Go file that names an image of its own.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/koinon-test.XXXXXX") || exit 1 # explicit template: macOS mktemp -d ignores TMPDIR (old issue 466)
trap '[[ -n ${KEEP_TMP:-} ]] || rm -rf "$tmp"' EXIT
pin=$repo/hack/agentgateway-pin.sh
d1=sha256:$(printf 'a%.0s' {1..64})
d2=sha256:$(printf 'b%.0s' {1..64})

fail=0
check() { # <description> <command...>
	local d=$1
	shift
	if "$@"; then echo "ok   $d"; else echo "FAIL $d"; fail=1; fi
}
eq() { [[ $1 == "$2" ]] || { echo "  got '$1', want '$2'" >&2; return 1; }; }

values() { # <digest line>: a values file with decoy image blocks before and after the real one, inside and outside gateway:
	cat >"$tmp/values.yaml" <<EOF
gateway:
  agentgateway:
    image:
      repository: decoy.example/controller
      tag: v0.0.1
      digest: $d2
  mode: standalone
  standalone:
    image:
      repository: ghcr.io/agentgateway/agentgateway
      tag: "v1.7.0"   # quoted, with a comment
$1
    port: 8080
  kubernetes:
    image:
      repository: decoy.example/after
      tag: v0.0.2
      digest: $d2
other:
  standalone:
    image:
      repository: decoy.example/other
      tag: v9
      digest: $d2
EOF
}
workflow() { # <body>
	printf '%s\n' "$1" >"$tmp/wf.yml"
}
mkdir -p "$tmp/go/testing/agentgatewayimage" "$tmp/go/ledger"
goconst() { # <digest>
	printf 'package agentgatewayimage\n\nconst (\n\t// Digest is the pin.\n\tDigest = "%s"\n\tUpstream = "%s@" + Digest\n)\n' "$1" "${2:-ghcr.io/agentgateway/agentgateway}" >"$tmp/go/testing/agentgatewayimage/image.go"
}
run() { VALUES=$tmp/values.yaml WORKFLOW=$tmp/wf.yml GOCONST=$tmp/go/testing/agentgatewayimage/image.go GO_TREE=$tmp/go "$pin" "$@" 2>"$tmp/err"; }
quiet() { "$@" >/dev/null; }
fails() { # <stderr fragment> <args...>: the call exits non-zero and says the fragment
	local want=$1
	shift
	if run "$@" >/dev/null; then echo "  passed, want a failure" >&2; return 1; fi
	grep -qF -- "$want" "$tmp/err" || { echo "  stderr: $(cat "$tmp/err")" >&2; return 1; }
}

good_wf='run: hack/agentgateway-pin.sh --check'

values "      digest: $d1"
workflow "$good_wf"
goconst "$d1"
check "prints the standalone image, not a decoy" eq "$(run)" "ghcr.io/agentgateway/agentgateway v1.7.0 $d1"
check "--check passes on a digest pin" quiet run --check

values "      digest: \"\""
check "--check fails on an empty digest" fails "not a sha256 digest" --check
check "printing fails on a tag-only pin" fails "lacks repository, tag or digest"

values "      digest: sha256:abc"
check "--check fails on a short digest" fails "not a sha256 digest" --check

values "      digest: ${d1^^}"
check "--check fails on an upper-case digest" fails "not a sha256 digest" --check

values "      digest: $d1"
workflow 'run: crane copy src dst'
check "--check fails when the workflow does not read the pin" fails "does not read the pin" --check

workflow "$good_wf
  crane copy x@$d1 y"
check "--check fails on a literal digest in the workflow" fails "literal digest" --check

# A layout the parser does not know (4-space indentation) fails loudly, never picks a wrong value.
sed 's/^\( *\)/\1\1/' "$tmp/values.yaml" >"$tmp/v4.yaml" && mv "$tmp/v4.yaml" "$tmp/values.yaml"
check "--check fails on a re-indented values file" fails "is empty" --check
values "      digest: $d1"

workflow "$good_wf"
goconst "$d2"
check "--check fails when the Go constant differs from the pin" fails "has Digest" --check
goconst "$d1" cr.agentgateway.dev/agentgateway
check "--check fails when the Go upstream repository differs from the chart's" fails "has Upstream repository" --check
goconst "$d1"
printf 'package ledger\n\nconst img = "cr.agentgateway.dev/agentgateway:v1.7.0"\n' >"$tmp/go/ledger/x_test.go"
check "--check fails on a Go file with its own tag reference" fails "name an agentgateway image themselves" --check
printf 'package ledger\n\nconst img = "ghcr.io/x/agentgateway@%s"\n' "$d1" >"$tmp/go/ledger/x_test.go"
check "--check fails on a Go file with its own digest reference" fails "name an agentgateway image themselves" --check
rm -f "$tmp/go/ledger/x_test.go"
check "--check passes again once the Go file is gone" quiet run --check
rm -f "$tmp/go/testing/agentgatewayimage/image.go"
check "--check fails when the Go constant file is missing" fails "is missing" --check
goconst "$d1"

rm -f "$tmp/wf.yml"
check "--check fails when the workflow is missing" fails "is missing" --check

check "an unknown argument is a usage error" fails "unknown argument" --bogus

# The real files pass.
check "the repository's pin passes --check" quiet "$pin" --check

if [[ $fail != 0 ]]; then echo "agentgateway-pin-test: FAILED" >&2; exit 1; fi
echo "agentgateway-pin-test: all passed"
