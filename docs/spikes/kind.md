# Spike S1: OpenShell on kind

Plan item S1 (`docs/plans/v0.1.md`): can a single-node kind cluster carry Agent Sandbox v1.0.7
and OpenShell 0.1.5 with a `runc` RuntimeClass, so that E1 (quickstart e2e) and E2 (escape
suite on `runc`) can run on GitHub's free `ubuntu-latest` runners?

Run locally on 2026-10-22. **Nothing in this document was run on a GitHub runner**; the
workflow `spike-kind.yml` was a draft with `workflow_dispatch` only, never triggered, and was
deleted in koinon-private#561.

## What was used

| Piece | Version | Pin |
| --- | --- | --- |
| kind | v0.33.0 | runner: binary SHA-256 in the workflow |
| Node image | `kindest/node:v1.37.0` | digest `sha256:a1ed56cf…` (kind v0.33.0 default) |
| Agent Sandbox | v1.0.7, `sandbox-with-extensions.yaml` | manifest SHA-256 `136c10c9…` (same as the home lab) |
| kube-network-policies | v1.1.2 manifest (image v1.1.1) | manifest SHA-256 `a8345bd4…` |
| OpenShell chart | 0.1.5 | OCI digest `sha256:e88977ce…` (same as the home lab) |
| OpenShell CLI | 0.1.5, `aarch64-apple-darwin` locally, `x86_64-unknown-linux-musl` in the workflow | SHA-256 from the release's `openshell-checksums-sha256.txt` |
| metrics-server | v0.9.0 (optional, for `kubectl top`) | `--kubelet-insecure-tls` |
| Sandbox image | `docker.io/library/busybox:1.37.0` | tag |

Local host: OrbStack (not Docker Desktop), Linux VM kernel `7.0.14-orbstack`, arm64, 10 CPUs and
15.7 GiB shared with other workloads. The `kindest/node` image was already in the local image
cache, so the kind step below does not include pulling it.

## Steps

Scripts are in `hack/spike/kind/`:

- `up.sh`: kind cluster `koinon-s1` (one node), kube-network-policies, Agent Sandbox, RuntimeClass
  `runc`, OpenShell (values in `openshell-values.yaml`), metrics-server (`METRICS=0` skips it).
  Every `kubectl`/`helm` call names `--context kind-koinon-s1`.
- `smoke.sh`: copies the chart's mTLS client bundle (`openshell-client-tls`) into a temporary
  `XDG_CONFIG_HOME`, port-forwards `svc/openshell`, registers the gateway with
  `openshell gateway add https://127.0.0.1:18081 --local`, then:
  1. `sandbox create --from busybox:1.37.0 --detach -- /bin/sleep 3600` and waits for `Ready`;
  2. `sandbox exec --no-tty -- echo ok` must print `ok`;
  3. a line piped into `sandbox exec --no-tty -- cat` must come back unchanged;
  4. a plain pod labelled `openshell.ai/boundary-role=workload` in the sandbox namespace must lose
     all egress (OpenShell's `openshell-sandbox-workloads` NetworkPolicy), an unlabelled control
     pod must keep it.
- `down.sh`: `kind delete cluster --name koinon-s1`.

## Results (local)

| # | Step | Result |
| --- | --- | --- |
| 1 | Cluster plus Agent Sandbox v1.0.7 plus OpenShell 0.1.5, default RuntimeClass `runc` | pass (three clean installs) |
| 2 | One sandbox, `exec --no-tty -- echo ok` | pass (`ok`, four sandboxes over three runs) |
| 2b | stdin round-trip through `cat` | pass (one line; also 20,000 lines in under 1 s) |
| 3 | Workload NetworkPolicy enforced | pass only with kube-network-policies added; **kindnet alone does not enforce it** |
| 4 | Scripts, draft workflow, this document | done |
| 5 | Cluster deleted | done |

### Wall clock (seconds)

| Step | Run 1 | Run 2 | Run 3 (final scripts) |
| --- | --- | --- | --- |
| kind create (node image cached) | 32 | 32 | 34 |
| kube-network-policies | – | (added by hand) | 5 |
| Agent Sandbox apply and rollout | 5 | 6 | 4 |
| RuntimeClass | 0 | 0 | 0 |
| OpenShell `helm --wait` (pulls gateway image, 54.5 MB) | 18 | 18 | 19 |
| metrics-server | 25 | – | 24 |
| **up.sh total** | **80** | **56** | **86** |
| First sandbox create → Ready (pulls supervisor 21 MB, sandbox 4.4 MB, busybox 1.9 MB) | 16 | 16 | 15 (gateway log; the script's loop reported 63 because of a polling bug, since fixed) |
| Further sandbox create → Ready (images cached) | 9 | 16 | 9 |
| `exec -- echo ok` | < 1 | < 1 | < 1 |
| stdin round-trip | < 1 | < 1 | < 1 |
| Delete two sandboxes (Sandbox, pods, PVCs gone) | 2 | – | – |

Cold path from nothing to a passing `exec` locally: about 100 s plus the node image pull.

### Resource use (one node, one or two sandboxes running)

- `kubectl top node`: 139–341m CPU, 1.09–1.13 GiB memory; `docker stats` on the node container:
  1.07–1.12 GiB, 440–560 PIDs, about 10–15 % of one CPU at rest.
- Largest pods: kube-apiserver 341 MiB, kube-controller-manager 76 MiB, OpenShell gateway 66 MiB,
  OpenShell supervisor 49 MiB per sandbox, Agent Sandbox controller 47 MiB, etcd 46 MiB,
  kube-network-policies 19 MiB, busybox workload 8 MiB.
- Requests on the node: 1050m CPU, 490 MiB memory. OpenShell sets no requests or limits on its
  sandbox pods by default (`resources: {}`); `sandbox create --cpu/--memory` sets them.
- Images pulled into the node: about 120 MB in total (gateway 54.5 MB, supervisor 21.2 MB,
  metrics-server 20.9 MB, Agent Sandbox controller 14.7 MB, sandbox 4.4 MB, busybox 1.9 MB).

## Changes OpenShell needs on kind

1. **RuntimeClass `runc`.** kind has no RuntimeClass objects. `up.sh` creates `runc` (handler
   `runc`) with label `openshell.ai/sandbox-attachable: "true"`, which OpenShell's resource
   admission requires (home-lab finding 1.4), and sets `server.defaultRuntimeClassName: runc`.
   Verified: the workload pod gets `runtimeClassName: runc`; the supervisor pod has none, as on
   the home lab. The chart also allows leaving `defaultRuntimeClassName` empty (the field is then
   omitted); that variant was **not** tried, because the plan names `runc` and E1/E2 should use
   the same code path as Kata.
2. **A NetworkPolicy engine.** kind's built-in kindnet (`kindnetd:v20260820`) ignores
   NetworkPolicy: a workload-labelled pod reached the API server, CoreDNS and the internet. With
   [kube-network-policies](https://github.com/kubernetes-sigs/kube-network-policies) installed
   next to kindnet, the same pod is blocked on all three while the control pod is not. Two
   properties matter for E2:
   - it runs with `--fail-open=true` and needs about 1–5 s after a pod is `Ready` before that
     pod's IP is in its nftables set; a probe in that window sees open egress. E2 must wait for
     enforcement (as `smoke.sh` does) and must not treat kind as evidence for a maintainer cluster's
     Cilium.
   - the OpenShell supervisor's in-process proxy denies egress on its own (`wget` from the
     sandbox got `Permission denied`, log `Denied staged transparent connection`), so the
     NetworkPolicy is the second fence, not the only one.
3. **Credential storage without an external store.** No `credentialDrivers` are enabled. The
   gateway then uses its default store: provider credentials and each sandbox's SSH identity are
   encrypted (AES-256-GCM) in its SQLite database, with the key-encryption key in the
   chart-generated Secret `openshell-credential-storage-key-encryption-key`. Nothing else was
   needed. (The `kubernetesSecrets` driver also exists but needs RBAC on all Secrets in a
   namespace; not used.) Providers were not exercised in this spike.
4. **Nothing else.** No CRDs beyond Agent Sandbox's, no image pull secrets (all images are public
   and multi-arch: `linux/amd64` and `linux/arm64`), the default `standard` StorageClass
   (local-path) served the 2 Gi workspace PVC, mTLS needed no extra SANs (the server certificate
   already covers `127.0.0.1` and `localhost`), and no kernel or Docker settings were changed.

## Kernel features (local)

Inside the sandbox on the OrbStack kernel 7.0.14:

- seccomp: `Seccomp: 2`, 6 filters, `NoNewPrivs: 1`, `CapEff: 0`, UID/GID 10001.
- Landlock: enforced. `/run` (mode 2777) and `/dev/shm` (1777) are writable by permissions but
  writes fail with `Permission denied`; `/tmp` and `/sandbox` (policy `read_write`, workdir)
  succeed. The default policy uses `landlock.compatibility: best_effort`, so a kernel without
  Landlock would degrade silently; ADR 0048 already requires `hard_requirement` for Koinon.
- No Kata on kind, so the VM boundary (and spike 8.12's guest-seccomp finding) cannot be tested
  here; E2 on kind covers OpenShell's own fences only, as the plan already says.

## Go/no-go for free `ubuntu-latest` runners (4 vCPU, 16 GB)

**Go**, with kube-network-policies in the cluster setup. This is a prediction from local
numbers, not a measurement on a runner.

| | Verified locally | Predicted for the runner |
| --- | --- | --- |
| Install works on kind | yes, three runs | same manifests; amd64 images exist |
| `exec --no-tty` and stdin | yes | same |
| Memory | about 1.1 GiB for the node | about 15 GiB left for E1's Koinon components, Playwright and fakes |
| CPU | about 1 core requested, under 0.4 core used at rest | 4 vCPU leaves room; E1 will be dominated by image builds and Playwright, not the cluster |
| Time to first `exec` | about 100 s with the node image cached | about 2–3 min: plus the `kindest/node` pull (355 MB compressed, 1.3 GB unpacked) and slower pulls; well within a 20-minute job |
| Landlock and seccomp | enforced on OrbStack 7.0 | Ubuntu 24.04 runner kernels enable Landlock and seccomp; the workflow prints `/sys/kernel/security/lsm` to confirm |
| NetworkPolicy | enforced with kube-network-policies, after a 1–5 s lag | same component; note Ubuntu's AppArmor restriction on unprivileged user namespaces is printed too, though nothing here needed user namespaces |

Risks to check on the first workflow run: the runner's kernel (Landlock in the LSM list), the
`kindest/node` pull time, and whether kube-network-policies' nfqueue works on the runner kernel
(it needs `nfnetlink_queue`). If any fails, the fallback the plan names (a self-hosted runner on
the home lab) applies only to E2; E1 does not depend on NetworkPolicy.

## Follow-ups

- (Dropped: the `spike-kind` workflow was deleted in koinon-private#561 without ever running; `make test-e2e-kind`
  and the `e2e-kind` workflow are the CI path now.)
- E2 must wait for NetworkPolicy enforcement before probing, and set
  `landlock.compatibility: hard_requirement` so a missing LSM fails the test instead of passing it.
- `smoke.sh` checks are written for macOS's bash 3.2, where `[[ … ]]` failures do not trip
  `set -e`; each check exits explicitly.
