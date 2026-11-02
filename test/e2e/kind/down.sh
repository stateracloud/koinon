#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Delete the E1 kind cluster created by up.sh.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
kind delete cluster --name "${CLUSTER:-koinon-e1}"
