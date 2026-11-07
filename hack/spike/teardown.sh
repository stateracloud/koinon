#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Remove everything the OpenShell spike created (docs/spikes/openshell.md). Prints the plan
# by default; pass --yes to execute. Run only when procoduck says the spike is over.
#
# Not removed here (procoduck's platform, listed at the end): the openshell-spike KV mount, the
# OpenBao policies and role, the RuntimeClass label and the Kata guest-seccomp change.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

run=0
[[ ${1:-} == --yes ]] && run=1
K=(kubectl --context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}")

step() {
  echo "+ $*"
  if [[ $run == 1 ]]; then "$@" || echo "  (failed, continuing)"; fi
}

# Sandboxes first, so OpenShell's own cleanup runs while the gateway and controller exist.
for ns in $("${K[@]}" get ns -l openshell.ai/managed-by=openshell -o name); do
  step "${K[@]}" delete sandboxes.agents.x-k8s.io --all -n "${ns#namespace/}" --wait=true --timeout=180s
done

step "${K[@]}" delete validatingadmissionpolicybinding koinon-spike-sandbox-pods --ignore-not-found
step "${K[@]}" delete validatingadmissionpolicy koinon-spike-sandbox-pods --ignore-not-found

step helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" uninstall openshell -n openshell --ignore-not-found
# agentgateway Kubernetes mode (5.6): its Gateway first, then the controller and its CRDs.
step "${K[@]}" delete -f "$(dirname "$0")/agentgateway-k8s/manifests.yaml" --ignore-not-found
if helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" status agw-k8s -n agentgateway-system >/dev/null 2>&1; then
  step helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" uninstall agw-k8s -n agentgateway-system
fi
if helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" status agw-k8s-crds -n agentgateway-system >/dev/null 2>&1; then
  step helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" uninstall agw-k8s-crds -n agentgateway-system
fi

for ns in $("${K[@]}" get ns -l openshell.ai/managed-by=openshell -o name) \
  namespace/openshell namespace/agentgateway-system namespace/koinon-spike; do
  step "${K[@]}" delete "$ns" --ignore-not-found --wait=true --timeout=300s
done

# Agent Sandbox controller and CRDs, from the same pinned manifest the install used.
manifest=$(mktemp "${TMPDIR:-/tmp}/teardown.XXXXXX")
trap 'rm -f "$manifest"' EXIT
curl -fsSL -o "$manifest" \
  https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.7/sandbox-with-extensions.yaml
echo "136c10c9347cb5a6740b351444cae82f404e883c3f520272257ba785f2af0da8  $manifest" | shasum -a 256 -c - >/dev/null
step "${K[@]}" delete -f "$manifest" --ignore-not-found

# Gitea: the spike org (with repo hello and its PRs) and the bot user. The admin token
# never appears in the printed plan.
admin=$(security find-generic-password -s gitea-kionon -w)
gitea_delete() {
  echo "+ gitea DELETE $1"
  if [[ $run == 1 ]]; then
    curl -fsS -o /dev/null -X DELETE -H "Authorization: token $admin" \
      "https://git.apps.lab.example.invalid/api/v1$1" || echo "  (failed, continuing)"
  fi
}
gitea_delete /repos/koinon-spike/hello
gitea_delete /orgs/koinon-spike
gitea_delete "/admin/users/koinon-spike-bot?purge=true"

# OpenBao: the spike's own secrets (the spike token can delete only these).
export BAO_ADDR=https://bao.apps.lab.example.invalid
BAO_TOKEN=$(security find-generic-password -s openbao-kionon -w)
export BAO_TOKEN
for path in gitea-bot agentgateway; do
  step bao kv metadata delete -mount=secret "koinon-spike/$path"
done

cat <<'EOF'

Left for procoduck (platform):
  - OpenBao: `bao secrets disable openshell-spike`; delete policies koinon-spike-admin and
    openshell-spike-gateway and role auth/kubernetes/role/openshell-spike; revoke the
    openbao-kionon token.
  - Platform repo: the RuntimeClass label and the Kata guest-seccomp change can stay (Koinon
    needs both); remove them only if Koinon is dropped.
  - Zot: images koinon/spike-probe, koinon/spike-approver and koinon/spike-extmcp.
EOF
[[ $run == 1 ]] || echo "(dry run: nothing was changed; pass --yes to execute)"
