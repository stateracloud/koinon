# Step 8: escape checks

Run 2026-10-21, before step 7 (step 8 decides whether the spike stops; step 7 does not). Probe:
`hack/spike/probe/escape_probe.py` as UID 10001 in sandbox `p2` (probe image, providers
`gitea-bot` and `agentgateway-key`, policy from step 3/6). Targets were passed in by IP, so the
probe holds no cluster knowledge. Admission checks use `hack/spike/manifests/sandbox-vap.yaml`, a
spike version of ADR 0045's ValidatingAdmissionPolicy bound only to namespaces labelled
`openshell.ai/managed-by=openshell`.

| # | Result | Evidence |
| --- | --- | --- |
| 8.1 | pass | TCP to `1.1.1.1:443`, LAN `<lan-gw>:443`, node `<node>:22`, the Cilium gateway IP, Ollama: `EACCES` at `connect`; UDP to `8.8.8.8:53`: `EDESTADDRREQ` |
| 8.2 | pass | `169.254.169.254:80`, `[fd00:ec2::254]:80`: `EACCES` |
| 8.3 | pass | cluster DNS `10.96.0.10:53` over UDP and TCP: blocked. A 50-character random name resolves to synthetic `198.18.0.9` with **no upstream query**: the trusted resolver runs only after a name matches the active policy (`crates/openshell-supervisor-network/src/policy_dns/mod.rs:146-177`) |
| 8.4 | pass | `10.96.0.1:443`, `kubernetes.default.svc:443`, control plane `:6443`, kubelet `:10250`: `EACCES` |
| 8.5 | pass | Gitea Postgres, OpenBao, OpenShell gateway `:8080` and metrics `:9090`, agentgateway stats `:15020` and its LLM port by pod IP, Gitea's in-cluster service: all `EACCES`. (agentgateway's admin port listens on localhost only) |
| 8.6 | pass | sandbox `p1` and its supervisor (same workspace), `b1` in workspace `spike-b`: `EACCES` |
| 8.7 | pass | no ServiceAccount token mounted (`automountServiceAccountToken: false`); `/.openshell/pod-identity`: `EACCES` |
| 8.8 | pass | step 2 (2.1-2.6) |
| 8.9 | pass | `ptrace(PTRACE_ATTACH, 1)`: `EPERM`; `/proc/1/mem`: `EACCES`; the supervisor is in a separate pod |
| 8.10 | pass | PID 1 is `openshell-sandbox`, guest kernel `6.18.35` vs node `<node-kernel>` |
| 8.11 | pass | `mount`: `EPERM`; `AF_PACKET` raw: `EPERM`; `AF_INET` raw: not supported; `CapEff = CapPrm = 0`, `NoNewPrivs = 1`; `setuid(0)`: `EPERM`; no setuid binaries |
| 8.12 | **pass after platform fix** (first run: fail) | Landlock: writes to `/etc`, `/usr`, `/var` and via `/sandbox/../etc` denied. seccomp: `unshare(NEWUSER)`, `unshare(NEWNS)`, `bpf`, `perf_event_open` → `EPERM`, but **`keyctl` and `add_key` succeed**. Re-run 14:15Z after Kata guest seccomp was enabled on both workers: `keyctl` and `add_key` → `EPERM`, `Seccomp_filters: 6` (OpenShell's filter plus the pod's `RuntimeDefault`), 0 of 24 attempts open; sandbox start, credentialed Gitea call and chat completion unaffected (`raw/8.12-rerun.md`) |
| 8.13 | pass | `kubectl debug p2 --profile=restricted`: denied by the VAP ("ephemeral containers are not allowed on sandbox pods"). With the default profile, Pod Security `baseline` already refuses (`SYS_PTRACE`) |
| 8.14 | pass | a `Sandbox` created directly (bypassing OpenShell) without a runtime class: pod denied by the VAP. With Pod Security set to `privileged` on the namespace (temporarily), server-side dry runs of workload pods with `hostNetwork`, `hostPID`, `hostPath` or a privileged container are each denied by the VAP |
| 8.16 | pass, with the new VAP rule | `raw/8.16-annotation.md`. A plain Kata pod annotated `io.katacontainers.config.runtime.disable_guest_seccomp: "true"` runs with `Seccomp: 0` (control pod: `2`): the bypass works for anyone who can create pods in a sandbox namespace. Through OpenShell it does not: `sandbox create --annotation …` stays in gateway metadata and never reaches the `Sandbox` or pod (sandbox keeps 6 filters). With the VAP rule rejecting `io.katacontainers.*` annotations, the annotated pod is denied and normal sandboxes still start |
| 8.14+ | pass | `raw/8.14-runtimeclass.md`: with a real RuntimeClass outside the list (`koinon-spike-runc`, created and deleted with procoduck's approval), a workload pod is denied by the VAP; `kata-qemu` is admitted. A non-existent class is refused earlier by Kubernetes' RuntimeClass admission |
| 8.ES | pass | `raw/8-externalsecret.md`: SubjectAccessReview for the sandbox SA `openshell-sandbox`, the workspace `default` SA and `openshell/openshell-sandbox`: no `create` on `externalsecrets` or `clusterexternalsecrets`, no `get secrets`, no `create pods`, `sandboxes` or `serviceaccounts/token`, in the workspace, `koinon-spike`, `openshell` or `external-secrets`. (`kubectl auth can-i --as` is unusable here: impersonation is dropped on the exec-plugin auth path) |
| 8.15 | pass, stricter than planned | OpenShell's own `openshell-sandbox-workloads` NetworkPolicy gives workload pods **no egress at all** (ingress only from the supervisor on 5500). A plain pod with the workload label, outside OpenShell's in-VM enforcement, cannot reach the internet, the API server, agentgateway, the gateway, OpenBao, another sandbox or DNS |

## The 8.12 failure

OpenShell's seccomp filter is default-allow with a targeted block list
(`crates/openshell-sandbox/src/sandbox/linux/seccomp.rs`: `bpf`, `mount`, `unshare`, `setns`,
`ptrace`, `process_vm_*`, `perf_event_open`, `io_uring_setup`, `userfaultfd`, `kexec_*`, module
loading, new mount API, `memfd_create`, socket domains, …). The kernel keyring calls (`keyctl`,
`add_key`, `request_key`) are not on it. The pod asks for `seccompProfile: RuntimeDefault`, which
in containerd blocks them, but it is evidently not applied inside the Kata guest (Kata's default
`disable_guest_seccomp = true`), so OpenShell's filter is the only one.

Containment: the calls reach the **guest** kernel of a VM that serves one sandbox. Keyrings are
per guest kernel, so there is no host or cross-sandbox reach; the residual risk is a guest-kernel
keyring bug used to get root inside the VM, which then still faces the VM boundary, the
no-egress NetworkPolicy and the out-of-VM supervisor.

Decision (procoduck, 2026-10-21): recorded as a fail. procoduck enables Kata guest seccomp on the workers
and 8.12 is re-run. If it then passes, it is recorded as "pass after platform fix" and guest
seccomp becomes a Koinon install requirement; if it still fails, the stop rule applies and Claude
proposes the OpenSandbox fallback. New criterion 8.16 checks that a pod annotation cannot switch
guest seccomp off again.

### Upstream issue draft (not filed; procoduck reviews before filing)

> **seccomp: block kernel keyring syscalls (`keyctl`, `add_key`, `request_key`)**
>
> OpenShell 0.1.5, Kubernetes compute driver, Kata Containers. The sandbox seccomp filter
> (`crates/openshell-sandbox/src/sandbox/linux/seccomp.rs`) is default-allow and does not block the
> kernel keyring syscalls, which Docker's and containerd's default profiles deny because the
> keyring is not namespaced. Under Kata with the default `disable_guest_seccomp = true`, the pod's
> `RuntimeDefault` profile is not applied inside the guest, so the OpenShell filter is the only one.
> Repro: inside a sandbox, `syscall(SYS_keyctl, KEYCTL_GET_KEYRING_ID, KEY_SPEC_SESSION_KEYRING, 0)`
> succeeds. Proposal: add `keyctl`, `add_key` and `request_key` to the block list (`EPERM`).

## Other findings

1. **The supervisor pod has unrestricted egress** (`openshell-sandbox-supervisors`: `egress: [{}]`)
   and no RuntimeClass; it is the policy enforcer and holds the interception CA key. A Koinon
   NetworkPolicy could narrow it to the union of policy destinations plus the gateway, at the cost
   of re-rendering on every policy change.
2. **The cluster enforces Pod Security `baseline` by default**, which already stops `hostNetwork`
   and debug containers with added capabilities; the VAP is the layer that also covers
   `--profile=restricted` debug containers, runtime classes and sandboxes created around OpenShell.
3. A VAP CEL error is a denial under `failurePolicy: Fail`: the first draft (`request.subResource`
   without `has()`) blocked every sandbox pod until fixed. Koinon's VAP needs tests against real
   OpenShell pods before it ships.
4. **The spike VAP's runtime-class rule keys on the pod's `boundary-role` label**, which the
   pod's creator sets: a pod labelled `supervisor`, or unlabelled, may use any runtime class. Koinon's
   VAP must key on something the creator cannot choose (for example the requesting identity, the
   Agent Sandbox controller's service account) and treat every other pod in a sandbox namespace as
   untrusted.
5. Wildcard policy hosts (`**.example.com`) would make DNS lookups of arbitrary subdomains reach
   the trusted resolver; Koinon's policy renderer should avoid them for agent-controlled names.
