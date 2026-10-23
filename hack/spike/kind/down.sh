#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike S1: delete the kind cluster created by up.sh.
set -euo pipefail
kind delete cluster --name "${CLUSTER:-koinon-s1}"
