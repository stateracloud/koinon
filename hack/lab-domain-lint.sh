#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails when a tracked file names a maintainer lab host (#693, umbrella #692). The lab's domain lives only
# in the untracked lab config (hack/lab/lab.env.example, KOINON_LAB_DOMAIN); docs, fixtures and spikes use
# lab.example.invalid. The check carries no real domain: it matches the shape of a lab host, a name under
# `.lab.<domain>` (zot.apps.lab.<domain>, ollama.lab.<domain>) or a bare `lab.<domain>/...` label key, that is
# not one of the example domains. When KOINON_LAB_DOMAIN is set (env or lab config) a tracked file naming
# that exact value fails too.
#
#   hack/lab-domain-lint.sh [<tree-ish>]     default: the working tree's tracked files (git grep)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
cd "$here/.."
# shellcheck source=hack/lab/config.sh disable=SC1091
. "$here/lab/config.sh"
lab_config_load 2>/dev/null || true

rev=${1:-}
allowed='lab\.example\.(invalid|com|org|net)'
host='(apps|ollama|testbox)\.lab\.[a-z0-9-]+\.[a-z]{2,}'
label='(^|[^A-Za-z0-9._/-])lab\.[a-z0-9-]+\.(com|net|org|io|dev|cloud|app|nl|eu|de|me|co)([^A-Za-z0-9-]|$)'

search() { # <ERE> [-F]
	if [[ -n $rev ]]; then git grep -noIE "$1" "$rev" -- . ':!hack/lab-domain-lint*.sh'; else git grep -noIE "$1" -- . ':!hack/lab-domain-lint*.sh'; fi
}

hits=$( { search "$host" || true; search "$label" || true; } | grep -Ev ":[^:]*${allowed}[^:]*\$" | sort -u || true)
if [[ -n ${KOINON_LAB_DOMAIN:-} && ${KOINON_LAB_DOMAIN} != lab.example.* ]]; then
	exact=$( { if [[ -n $rev ]]; then git grep -nIF -- "$KOINON_LAB_DOMAIN" "$rev" -- . ':!hack/lab-domain-lint*.sh'; else git grep -nIF -- "$KOINON_LAB_DOMAIN" -- . ':!hack/lab-domain-lint*.sh'; fi; } || true)
	hits=$(printf '%s\n%s\n' "$hits" "$exact" | sed '/^$/d' | sort -u)
fi

if [[ -n $hits ]]; then
	echo "lab-domain-lint: tracked files name a lab host (use lab.example.invalid; the real domain belongs in the lab config):" >&2
	# Print file:line only, never the matched text.
	printf '%s\n' "$hits" | cut -d: -f"$([[ -n $rev ]] && echo 2,3 || echo 1,2)" | sort -u >&2
	exit 1
fi
echo "lab-domain-lint: no lab host in the tree"
