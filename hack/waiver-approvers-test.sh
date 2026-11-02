#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Offline test of hack/waiver-approvers.sh.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
fail=0
eq() { if [[ $2 == "$3" ]]; then echo "ok: $1"; else echo "FAIL: $1: got '$2', want '$3'"; fail=1; fi; }

fixture='# Maintainers

Thanks to [@thanker](https://github.com/thanker) for the logo.

| Name | GitHub | Role |
| --- | --- | --- |
| A One | [@alice](https://github.com/alice) | Project lead |
| B Two | [@bob-2](https://github.com/bob-2) | Maintainer |

## Reviewers

| Name | GitHub | Area |
| --- | --- | --- |
| C Three | [@carol](https://github.com/carol) | web/ |

## Emeritus

- [@dave](https://github.com/dave)
'
eq "only the maintainers table" "$(printf '%s' "$fixture" | "$here/waiver-approvers.sh" - | paste -sd, -)" "alice,bob-2"
eq "the real file has the project lead" "$("$here/waiver-approvers.sh" "$here/../MAINTAINERS.md" | grep -cx procoduck)" "1"
eq "no table, no approvers" "$(printf '# Maintainers\n\n[@x](https://github.com/x)\n' | "$here/waiver-approvers.sh" - | wc -l | tr -d ' ')" "0"
exit $fail
