# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: CNPG node maintenance window for single-instance drains (#103).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

cluster=(--show-only templates/postgres-cnpg.yaml)

# Default: no window (it is a temporary switch for one drain), one instance.
lacks "no nodeMaintenanceWindow by default" 'nodeMaintenanceWindow' "${cluster[@]}"
has "one CNPG instance by default" '^  instances: 1$' "${cluster[@]}"

# Set: the window reaches the Cluster spec as given.
win=("${cluster[@]}" --set postgres.cnpg.nodeMaintenanceWindow.inProgress=true --set postgres.cnpg.nodeMaintenanceWindow.reusePVC=true)
has "nodeMaintenanceWindow is rendered" '^  nodeMaintenanceWindow:$' "${win[@]}"
has "nodeMaintenanceWindow.inProgress" '^    inProgress: true$' "${win[@]}"
has "nodeMaintenanceWindow.reusePVC" '^    reusePVC: true$' "${win[@]}"

# Two instances: the PDB then allows one eviction, nothing else changes.
has "instances follows postgres.cnpg.instances" '^  instances: 2$' "${cluster[@]}" --set postgres.cnpg.instances=2
