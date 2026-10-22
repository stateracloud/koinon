#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Delete the E1 kind cluster created by up.sh.
set -euo pipefail
kind delete cluster --name "${CLUSTER:-koinon-e1}"
