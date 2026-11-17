#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests the ADR tool (hack/cmd/adr): the Ginkgo suite runs the lint, the generator and the find, show and
# graph commands against throwaway ADR directories. Then the wrappers run once against the real ADRs.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"
go test ./hack/cmd/adr/...
hack/adr-index.sh --check >/dev/null
hack/adr find --area engineering-process --tag adr-process >/dev/null
echo "adr-lint-test: ok"
