# Step 1: install and lifecycle

Run 2026-10-21 on the spike cluster (Talos 1.14.4, Kubernetes 1.37.1, node kernel 6.18.57-talos).
Installed: Agent Sandbox v1.0.7 (`hack/spike/agent-sandbox.sh`, manifest SHA-256 verified) and
OpenShell 0.1.5 (`hack/spike/openshell.sh`, chart digest pinned, values in
`hack/spike/openshell-values.yaml`). CLI 0.1.5 (aarch64-apple-darwin, checksum verified) talks to
the gateway over mTLS through `hack/spike/port-forward.sh`.

| # | Result | Evidence |
| --- | --- | --- |
| 1.1 | pass | CRDs `sandboxes.agents.x-k8s.io` (v1beta1, served and storage) plus the three `extensions.agents.x-k8s.io` CRDs; controller rolled out, no errors in its log; `hack/spike/manifests/plain-sandbox.yaml` reached `Ready` in 3 s (12:28:06 → 12:28:09Z) |
| 1.2 | pass | `agents.x-k8s.io/v1beta1` `Sandbox` `s1` created by OpenShell in namespace `openshell-openshell-spike-a` (`openshell-{gateway_id}-{workspace}`, gateway id `openshell`) |
| 1.3 | pass | workload pod `runtimeClassName: kata`; guest `uname -r` = `6.18.35`, node = `6.18.57-talos` |
| 1.4 | pass | `sandbox stop s2`: `spec.operatingMode: Suspended`, both pods gone, PVC `workspace-s2` still `Bound` |
| 1.5 | pass | file written to `/sandbox/persist.txt` before stop, read back after start; PVC UID unchanged (`a3233da6-…`) |
| 1.6 | recorded | CLI wall clock: first create incl. image pulls 29 s; create → Ready with cached images 25 s; stop 6 s; start → Ready 22 s |

## Findings

1. **Two pods per sandbox.** OpenShell 0.1.5 runs the supervisor (proxy, policy, credential
   injection) in its own pod `os-supervisor-<id>` with no RuntimeClass, and the agent in a separate
   Kata workload pod. They are paired by label `openshell.ai/boundary-pair` and a `os-boundary-<id>`
   Service; the namespace gets NetworkPolicies `openshell-sandbox-supervisors` and
   `openshell-sandbox-workloads`. The workload's only path out is therefore enforced by
   NetworkPolicy, not by an in-VM proxy, so the CNI must enforce egress policy (the chart README says
   so). Step 8 must test this directly; the plan's in-pod assumption is out of date.
2. **Workload pod hardening.** UID/GID 10001, `runAsNonRoot`, all capabilities dropped, no
   privilege escalation, seccomp `RuntimeDefault`, `automountServiceAccountToken: false`, SA
   `openshell-sandbox`; `/sandbox` is the workspace PVC (2Gi, `proxmox-zfs`); `/run` and
   `/.openshell/*` are memory-backed. Env holds only `OPENSHELL_*` non-secret settings.
3. **Every sandbox writes to the credential driver**, even without providers: the gateway stores
   each sandbox's SSH identity through it (`crates/openshell-server/src/ssh_identity.rs`). The
   `vault` driver writes to the fixed prefix `<mount>/openshell/provider-credentials/<sha256[:40]>`
   (`crates/openshell-driver-vault/src/lib.rs:852`), needs create/read/update on
   `<mount>/data/...` and delete on `<mount>/metadata/...`, and refuses plain HTTP to non-loopback
   addresses. The spike uses a dedicated KV v2 mount `openshell-spike` (kept out of the platform's
   unrestricted ESO ClusterSecretStore) at `https://bao.apps.lab.example.invalid`, with policy
   `openshell-spike-gateway` on Kubernetes auth role `openshell-spike`.
4. **RuntimeClasses need an operator approval label.** With resource admission on (default), a
   sandbox using `kata` fails with `RuntimeClass 'kata': external resource not admitted by required
   labels` until the RuntimeClass carries `openshell.ai/sandbox-attachable: "true"`. procoduck added it
   to `kata` and `kata-qemu` in the platform repo.
5. **A sandbox's main command defines its lifetime.** `sandbox create -- <cmd>` leaves the sandbox
   `Completed` when the command exits, and `exec` is then refused; long-lived agents need
   `--detach` with a long-running main process (or a restart policy).
6. **Agent Sandbox controller misses the `restricted` Pod Security profile** (no `runAsNonRoot`,
   capabilities not dropped, no seccomp profile); the apply warns on this cluster.
7. **Install shape.** Agent Sandbox ships plain manifests, not a Helm chart. The OpenShell chart
   enables telemetry by default (`server.telemetryEnabled`); the spike turns it off.
8. **`sandbox create --no-keep` can orphan the sandbox** (seen twice: `p3` in step 3.5 and `a16b`
   in the 8.16 run, both with commands that exit at once; `r1`, which ran longer, was cleaned up).
   The CLI prints `Deleted sandbox`, the gateway forgets it, but the `Sandbox` object stays
   `Ready` and its workspace PVC stays bound. Koinon's reconciler must garbage-collect
   OpenShell-labelled `Sandbox` objects the gateway no longer lists. Upstream issue draft (not
   filed; procoduck reviews first):

   > **Kubernetes driver: `sandbox create --no-keep` with a fast-exiting command leaves the
   > Sandbox object and PVC behind**
   >
   > OpenShell 0.1.5, Kubernetes compute driver, Agent Sandbox v1.0.7, managed workspaces.
   > `openshell sandbox create --name x --no-keep -- grep ^Seccomp: /proc/self/status` prints the
   > output and `Deleted sandbox x`; `openshell sandbox list` no longer shows `x`, but
   > `kubectl get sandbox x` in the workspace namespace shows it `Ready` with both pods running and
   > `workspace-x` still bound. Reproduced twice with commands that exit within a second; a
   > command that runs a few seconds is cleaned up. Expected: the driver deletes the `Sandbox`
   > (and its storage) before the gateway drops its record, or reconciles orphans later.
9. **A node losing its network stops the sandbox.** When `spike-workers-cjgkns` was NotReady for
   about a minute at 14:35Z (`cni plugin not initialized`), OpenShell moved `p2` to `Stopped`
   and did not restart it; `sandbox start` brought it back with its workspace. Koinon's reconciler
   decides whether and when to restart an agent's sandbox after infrastructure failures.
