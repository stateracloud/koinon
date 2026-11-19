<!-- SPDX-License-Identifier: Apache-2.0 -->

# Spike: gVisor in kind on a standard GitHub runner

- Status: Complete
- Date: 2026-11-19
- Area: runtime-sandbox
- Tags: isolation, e2e, ci

Issue #1091 (slice 9a, epic #1082). The `dev` tier is gVisor on kind (ADR 0119 D3). The CI leg of #1091 uses
standard runners and no KVM (ADR 0119 D10). This spike asked: does `runsc` run in a kind node on a standard
`ubuntu-24.04` runner, and how long does the install take?

**Result: proved.** A pod in a `gvisor` RuntimeClass runs in kind on a standard runner. It needs no AppArmor
change and no larger runner.

## What was proved (in CI)

The workflow `.github/workflows/gvisor-kind-spike.yml` (PRs #1141, #1142, #1143, now on main) ran twice on
`ubuntu-24.04` with the pinned kind node image (Kubernetes 1.37) and gVisor release `20261116.0`:

| Run | `kernel.apparmor_restrict_unprivileged_userns` | Result |
| --- | --- | --- |
| 44512255633 | 1 (the runner default, not changed) | Pass. The pod ran. Its `dmesg` shows "Starting gVisor..." and `/proc/version` shows "Linux version 4.19.0-gvisor". |
| 44512260860 | 0 (set by the job) | Pass. Same result. |

Step times from the two runs (GitHub job data):

| Step | Run 1 | Run 2 |
| --- | --- | --- |
| `kind create cluster` (node image in the runner cache or pulled) | 39 s | 42 s |
| Install `runsc` and the shim in the node, restart containerd | 11 s | 11 s |
| Pod in the `gvisor` class, to `Succeeded` | 3 s | 6 s |

So gVisor adds about 11 s of install to a kind cluster. The pull of `busybox` is inside the pod time.

## Facts the install step needs

1. `runsc` from `20261116.0` needs the `gvisor-bin/` helper directory (`gvisor_sentry` and others) next to the
   binary. Without it the sandbox start fails: `sidecar "gvisor_sentry" not usable: stat
   /usr/local/bin/gvisor-bin/gvisor_sentry: no such file or directory`. Copy the whole tarball contents, not one
   binary.
2. The release layout changed. Each release ships only `x86_64/gvisor.tar.bz2` with a `.sha512` file, under
   `https://storage.googleapis.com/gvisor/releases/release/<YYYYMMDD.N>/`. The old `.../x86_64/runsc` URL
   returns 404.
3. Pin the release by date (`20261116.0`), not `latest`.
4. kind nodes use the systemd cgroup driver, so `runsc` gets a config file with `systemd-cgroup = "true"`
   (`/etc/containerd/runsc.toml`, linked by `ConfigPath` in the runtime options).
5. The containerd config key is `io.containerd.cri.v1.runtime` (containerd 2.x in the node image). The job
   appends the `runsc` runtime and restarts containerd, then waits for the node to be Ready.

The working steps are in the workflow file on main. Do not copy them from this page.

## What was tried on the lane VM (not proof)

The lane VM could not run kind: it has cgroup v1, so `kubeadm init` failed after 81 s (`kind create cluster`
with the pinned node image, kind v0.33.0). Three registries were blocked, and the VM could not fetch `runsc`
(the old URL, fact 2). This VM said nothing about the runner. The first draft of the install steps, written
here without a test, was wrong in two places: it copied one binary (fact 1) and used the old URL (fact 2).
Main ran the corrected job.

## Runner facts

The `ubuntu-24.04` image `20261111.370.1` has kernel `7.0.0-1017-azure`, Docker 28.0.4 and kind 0.33.0
(runner-images readme). The repo pins kind v0.33.0 in `hack/versions.env` and the node image
`kindest/node:v1.37.0` in `test/e2e/kind/up.sh` (Kubernetes 1.37, meets D8).

## CI job design for #1091

The substrate leg on kind is one job in a new workflow file (a local lane or main adds it; a cloud lane cannot
edit `.github/**`). It reuses the spike steps:

1. Same runner, kind version and node image as `e2e-kind.yml`. Create the cluster with `test/e2e/kind/up.sh`
   (without the OpenShell parts once #1094 removes them).
2. Install `runsc` as in the spike: the `20261116.0` tarball, the sha512 check, `gvisor-bin/` copied with the
   binary and the shim, `runsc.toml`, the containerd runtime, a containerd restart. Put the release and its
   hash in `hack/versions.env` (ADR 0079: at least 3 days old) and read them from there.
3. Install Substrate with `koinon-api substrate install --platform kind` (the `kind` platform of
   `docs/dev/substrate-install.md`), then `helm install` with `execution.driver: substrate`.
4. Run the quickstart e2e, the harness contract tests and the restore drill with the fake model server.
5. List the job in `hack/ci-required.txt` as `advisory` first (`docs/dev/ci-rules.md`, ADR 0111).
6. Keep the node-log step on failure (`journalctl -u containerd`, `crictl info`).

## Open risks

1. **Substrate on kind.** The spike proves the runtime, not the Substrate install. `ate-setup --kind` may stage
   `runsc` itself or may expect it on the node. If it stages its own copy, step 2 above is not needed. Read
   its source before the job is written.
2. **Release drift.** The release layout changed once. A new gVisor release can change it again. The pin and the
   sha512 check make this fail loudly.
3. **Landlock and seccomp in the guest.** gVisor has no `landlock_*` calls (ADR 0075). ADR 0120 phase 3
   covers this. The spike says nothing about it.
4. **Runner drift.** The AppArmor default may change in a new runner image. The #1118 preflight reads only
   `user.max_user_namespaces`. Today the AppArmor value does not matter (both runs passed), so no preflight
   change is needed now.
5. **Image pulls.** The `busybox` pull and the Substrate images can be most of a real install time. Measure with
   and without the pull-through caches of `test/e2e/kind/registry.sh`.

## Overlap with other lanes

None in code. The report touches only `docs/dev/`. #1087, #1088 and #1089 own `internal/` and `charts/`.
