<!-- SPDX-License-Identifier: Apache-2.0 -->

# Spike: gVisor in kind on a standard GitHub runner

- Status: Open (the claim is not proved; it needs one CI run)
- Area: execution
- Tags: substrate, gvisor, kind, ci

Issue #1091 (slice 9a, epic #1082). The `dev` tier is gVisor on kind (ADR 0119 D3). The CI leg of #1091 uses
standard runners and no KVM (ADR 0119 D10). This spike asks: does `runsc` run in a kind node on a
standard `ubuntu-24.04` runner, and how long does the install take?

**Result: not proved, not refuted.** This VM cannot run kind. One workflow dispatch can answer the question.
The job is [below](#the-dispatch-job). No product code changes in this slice.

## What was proved

Nothing about gVisor. The checks that did run:

| Check | Result |
| --- | --- |
| Docker daemon on this VM | Works (Docker 29.10.0). |
| Pinned node image `kindest/node:v1.37.0@sha256:a1ed56cf...` (from `test/e2e/kind/up.sh`) | Present locally, so no pull was needed. |
| `kind create cluster` with kind v0.33.0 and that image | **Fails** after 81 s. `kubeadm init` times out: the API server never starts. |
| Cause | This VM has cgroup v1 (`Cgroup Version: 1`, `/sys/fs/cgroup` is `tmpfs`). kind warns "cgroup v1 is deprecated" and the control plane does not come up. A GitHub runner has cgroup v2, so this failure says nothing about the runner. |
| `/dev/kvm` | Not present. Not needed for gVisor in `systrap` mode. |
| `registry.k8s.io`, `quay.io`, `docker.io` | Refused by the egress proxy. `ghcr.io` and `registry-1.docker.io` answer. |
| gVisor release bucket (`storage.googleapis.com/gvisor/...`) | The path I tried answered 404. I did not find the right path. This VM could not download `runsc`. |

So this VM could not test: any gVisor start, the AppArmor restriction (the file
`/proc/sys/kernel/apparmor_restrict_unprivileged_userns` does not exist here), the install time, and any
Kubernetes 1.37 node behaviour.

## What was only read

- **Runner image.** The `ubuntu-24.04` image `20261111.370.1` has kernel `7.0.0-1017-azure`, Docker 28.0.4 and
  kind 0.33.0 (runner-images `Ubuntu2404-Readme.md`). The repo pins kind v0.33.0 in `hack/versions.env`, the same
  version.
- **Node image.** `test/e2e/kind/up.sh` pins `kindest/node:v1.37.0`, Kubernetes 1.37.0. This meets D8 (minimum
  1.37). `hack/spike/kind/up.sh` pins the same image. `e2e-kind.yml` already creates this cluster on
  `ubuntu-24.04`, so kind itself is known to work on the runner.
- **User namespaces.** gVisor starts its sentry in a user namespace. PR 1124 and #1118 added a preflight that
  reads `/proc/sys/user/max_user_namespaces` from a pod on each gVisor node (`internal/preflight/substrate_userns.go`).
  I did not read this value on a runner, and I did not run the check.
- **AppArmor.** Ubuntu 24.04 can restrict unprivileged user namespaces with
  `kernel.apparmor_restrict_unprivileged_userns=1`. When it is 1, a process outside an AppArmor profile that
  allows `userns` gets no capabilities in a new user namespace. The runner image may set the sysctl to 1 or 0:
  I could not read its value. This is the main open risk (below).
- **Docs of the repo.** `docs/dev/substrate-install.md` says the `generic` platform installs the `gvisor` class
  only, and the `kind` platform uses `ate-setup --kind` unchanged. I did not read how `ate-setup` puts `runsc`
  and the containerd shim on a kind node: `third_party/substrate` holds only protos, and the Substrate source
  tree was not in this VM.

## The dispatch job

Needs main: dispatch job. I cannot edit `.github/**`. Put this in a new workflow file, for example
`.github/workflows/gvisor-kind-spike.yml`, run it once with `workflow_dispatch`, and delete it after the numbers
are in this report. It does not use Substrate: it tests the one thing under the claim, `runsc` as a containerd
runtime inside a kind node. If it passes, the next step is `koinon-api substrate install --platform kind` on
the same cluster.

```yaml
# SPDX-License-Identifier: Apache-2.0
# Spike for #1091 slice 9a: does gVisor (runsc) run in kind on a standard runner? Dispatch only.
name: gvisor-kind-spike
on:
  workflow_dispatch:
    inputs:
      userns_restrict:
        description: "Value to set for kernel.apparmor_restrict_unprivileged_userns (empty: leave as the runner has it)"
        required: false
        default: ""
      gvisor_release:
        description: "gVisor release path under storage.googleapis.com/gvisor/releases/release/"
        required: false
        default: "latest"
permissions:
  contents: read
jobs:
  spike:
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          persist-credentials: false
      - name: Runner facts
        run: |
          set -x
          uname -r
          sysctl kernel.apparmor_restrict_unprivileged_userns || true
          sysctl user.max_user_namespaces
          stat -fc %T /sys/fs/cgroup
          cat /sys/kernel/security/lsm
          docker --version
          kind version
      - name: Set the AppArmor sysctl (only when asked)
        if: inputs.userns_restrict != ''
        run: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns="${{ inputs.userns_restrict }}"
      - name: Create kind (pinned node image)
        run: |
          set -euo pipefail
          t0=$(date +%s)
          NODE_IMAGE=$(sed -n 's/^NODE_IMAGE=//p' test/e2e/kind/up.sh)
          kind create cluster --name gv --image "$NODE_IMAGE" --kubeconfig "$RUNNER_TEMP/kc" --wait 120s
          echo "kind create: $(( $(date +%s) - t0 ))s"
      - name: Install runsc and the containerd shim in the node
        run: |
          set -euo pipefail
          t0=$(date +%s)
          base="https://storage.googleapis.com/gvisor/releases/release/${{ inputs.gvisor_release }}/x86_64"
          cd "$RUNNER_TEMP"
          for f in runsc containerd-shim-runsc-v1; do
            curl -fsSL -O "$base/$f" -O "$base/$f.sha512"
            sha512sum -c "$f.sha512"
            chmod +x "$f"
            docker cp "$f" gv-control-plane:/usr/local/bin/"$f"
          done
          docker exec gv-control-plane runsc --version
          docker exec gv-control-plane sh -c "cat >> /etc/containerd/config.toml <<'EOF'
          [plugins.\"io.containerd.cri.v1.runtime\".containerd.runtimes.runsc]
            runtime_type = \"io.containerd.runsc.v1\"
          EOF"
          docker exec gv-control-plane systemctl restart containerd
          echo "runsc install: $(( $(date +%s) - t0 ))s"
      - name: Run a pod in the gvisor RuntimeClass
        env:
          KUBECONFIG: ${{ runner.temp }}/kc
        run: |
          set -euo pipefail
          t0=$(date +%s)
          kubectl apply -f - <<'EOF'
          apiVersion: node.k8s.io/v1
          kind: RuntimeClass
          metadata:
            name: gvisor
          handler: runsc
          ---
          apiVersion: v1
          kind: Pod
          metadata:
            name: gv-probe
          spec:
            runtimeClassName: gvisor
            restartPolicy: Never
            containers:
              - name: probe
                image: busybox:1.37
                command: ["sh", "-c", "dmesg | head -3; cat /proc/version; cat /proc/sys/user/max_user_namespaces"]
          EOF
          kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/gv-probe --timeout=180s
          kubectl logs gv-probe
          echo "gvisor pod: $(( $(date +%s) - t0 ))s"
      - name: Node logs on failure
        if: failure()
        run: |
          docker exec gv-control-plane journalctl -u containerd --no-pager | tail -60 || true
          sudo dmesg | grep -i apparmor | tail -20 || true
```

Notes on the job:

- The containerd config key `io.containerd.cri.v1.runtime` is the one for containerd 2.x. The node image has
  containerd 2.x, but I did not check it. If the restart fails, read `/etc/containerd/config.toml` in the node.
- `busybox:1.37` comes from docker.io. Use the registry caches of `test/e2e/kind/registry.sh` if the pull is
  rate limited.
- Run it twice: once with `userns_restrict` empty (the runner as it is) and once with `0`. The two runs say
  whether the AppArmor sysctl matters.

## Exact commands tried here

```sh
docker info
kind create cluster --name gv-spike \
  --image kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5 \
  --kubeconfig <scratch file> --wait 90s   # exit 1 after 81 s: cgroup v1 host
kind delete cluster --name gv-spike
```

The install time was **not measured**. Fill it in from the job log (`kind create`, `runsc install`, `gvisor pod`).

## Open risks

1. **AppArmor.** If the runner has `kernel.apparmor_restrict_unprivileged_userns=1`, `runsc` may fail in the kind
   node even when `user.max_user_namespaces` is large. The preflight of #1118 reads only the second value, so
   it would pass and the first start would fail. If the first dispatch run fails and the second (value 0)
   passes, the CI job sets the sysctl with `sudo sysctl -w` before `kind create`, and the preflight should
   read the AppArmor sysctl too (new issue; no product change here).
2. **Nested sandbox.** `runsc` runs inside a privileged docker container (the kind node), not on the host.
   Some `runsc` platforms (`ptrace`, `kvm`) differ in what they need. The `systrap` default needs seccomp and
   `ptrace`-like rights, which Docker allows in a privileged node. Not tested.
3. **Landlock and seccomp in the guest.** gVisor has no `landlock_*` calls (ADR 0075). ADR 0120 phase 3 covers
   this. This spike says nothing about it.
4. **Substrate on kind.** `ate-setup --kind` may stage `runsc` itself, or may expect it on the node. Not read.
   The job above installs `runsc` by hand, so a pass proves the runtime, not the Substrate install.
5. **Pin.** `latest` is not a pin. After the first run, pin the gVisor release and its SHA-512 in
   `hack/versions.env` (ADR 0079: at least 3 days old).
6. **Cache and speed.** The pulls of `busybox` and the Substrate images can dominate the install time. Measure
   with and without the pull-through caches.

## Overlap with other lanes

None in code. The report touches only `docs/dev/`. #1087, #1088 and #1089 own `internal/` and `charts/`.
