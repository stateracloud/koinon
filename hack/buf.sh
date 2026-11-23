#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The one way to run buf (#1198): the pinned `go tool` of the module hack/tools/buf (#1169), at the version
# of BUF_VERSION in hack/versions.env. `make lint-proto`, `make breaking`, `make generate`, the preflight gate
# `proto` and the proto and generate jobs of ci.yml all call this script. No caller uses a buf from the PATH.
#
#   hack/buf.sh <buf arguments...>
#
# Runs in the current directory (buf reads buf.yaml there); only the modfile is found by the script's path.
# Env: GO names the go binary (default go). Test hook: KOINON_TEST_BUF names a stand-in binary
# (hack/generate-test.sh, hack/buf-test.sh).
# Bump procedure (the root tidy gate does not cover this modfile): go get -tool -modfile=hack/tools/buf/go.mod
# github.com/bufbuild/buf/cmd/buf@vX.Y.Z, then BUF_VERSION in hack/versions.env.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if [[ -n ${KOINON_TEST_BUF:-} ]]; then
	exec "$KOINON_TEST_BUF" "$@"
fi
modfile=$(cd "$(dirname "$0")" && pwd)/tools/buf/go.mod
exec "${GO:-go}" tool -modfile="$modfile" buf "$@"
