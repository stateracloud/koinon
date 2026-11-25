<!-- SPDX-License-Identifier: Apache-2.0 -->

# Qualify a Substrate tier with the escape suite

The escape suite proves that an agent in a sandbox cannot get out. Issue #1092 adds a mode that runs it
on Agent Substrate. v0.1.0 qualifies the **gVisor tier** (`hardened`, ADR 0075). The `vm` tier is
issue #1349.

The run needs a real cluster with Substrate. Nothing in this page runs in a lane VM.

## What the run needs

| Need | Detail |
|---|---|
| Cluster | A cluster with Agent Substrate installed ([substrate-install.md](substrate-install.md)). One worker node in the pool for the gVisor class, so both orgs' sandboxes share the node. |
| Runtime class | The Substrate class of the tier under test. For `hardened` it is the gVisor class of the install. |
| Image | An actor image with `sh` and `python3`, built on the `sandbox-substrate` overlay (guest front and launcher). Pull it through the mirror the install uses. |
| Reach | The machine that runs the command reaches ateapi and the atenet router. |
| Keys | The Ed25519 guest-API key file that the actor template trusts. No other secret. |
| Snapshots (optional) | The snapshot bucket with a read-only key. Without it the restore spec fails closed (see below). |
| Time | About 25 minutes: three sandboxes start, plus the cross-org and restore specs. The Make target sets a 40 minute budget. |

The run does not need `KUBECONFIG`. Set `KUBECONFIG=/dev/null`.

## Variables

The full list, with defaults, is in the header of `test/escape/substrate_harness_test.go`. The required ones:

- `KOINON_ESCAPE_SUBSTRATE_CONTROL`: ateapi `host:port`.
- `KOINON_ESCAPE_SUBSTRATE_ROUTER`: atenet router `host:port`.
- `KOINON_ESCAPE_SUBSTRATE_GUEST_KEY_FILE`: the guest-API key file.
- `KOINON_ESCAPE_SUBSTRATE_IMAGE`: the actor image.
- `KOINON_ESCAPE_SUBSTRATE_RUNTIME_CLASS`: the runtime class of the tier.

Set these for the specs that need them (a missing one fails the spec; it does not skip):

- `KOINON_ESCAPE_HOST_KERNEL`: `uname -r` of the worker node. The tier spec checks that the guest kernel
  differs.
- `KOINON_ESCAPE_API` and `KOINON_ESCAPE_INTERNAL`: Service `host:port` lists for the API server and the
  platform services (ateapi, atelet, the gateway).
- `KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_*`: the snapshot bucket, for the restore spec.
- `KOINON_ESCAPE_TIER`: `hardened` (default), `vm` or `dev`.

## Command

```sh
export KUBECONFIG=/dev/null
export KOINON_ESCAPE_SUBSTRATE_CONTROL=... KOINON_ESCAPE_SUBSTRATE_ROUTER=...
export KOINON_ESCAPE_SUBSTRATE_GUEST_KEY_FILE=... KOINON_ESCAPE_SUBSTRATE_IMAGE=...
export KOINON_ESCAPE_SUBSTRATE_RUNTIME_CLASS=... KOINON_ESCAPE_HOST_KERNEL="$(ssh node uname -r)"
make test-escape-substrate
```

`make test-escape-substrate` runs `go test ./test/escape/ -ginkgo.label-filter=substrate` with
`KOINON_ESCAPE_DRIVER=substrate`. It sets `KOINON_REQUIRE=escape-cluster-ips,host-kernel`, so a missing
cluster input is a failure.

## Expected output

On the gVisor tier, every spec passes, except two that report **not applicable** as a skip with a reason:

1. `runs in a microVM guest, not on the host kernel`: not applicable on `hardened` and `dev`
   (ADR 0075 decision 9). The `vm` tier is issue #1349.
2. `denies writes outside Landlock's read-write list`: gVisor has no Landlock, and the root is writable
   by decision (ADR 0120 section 4, issue #1150). The syscall half of the spec runs and passes. On the
   `vm` tier this spec must pass, and the harness fails it if Landlock is missing.

The final line reads `0 Failed`, with these two among the skipped. Any other skip is a fault of the run.
The offline `contract` specs (token audience and lifetime) run in every `go test ./test/escape/` and need
no cluster.

## What the specs check

- Direct egress and cloud metadata are denied.
- No ServiceAccount token, SPIFFE socket or kube config in the guest. The API server and the platform
  services are unreachable.
- No credential, no actor identity token and no JWT-shaped value in files or environment.
- No capabilities, `no_new_privs` and a seccomp filter. Privileged syscalls fail.
- The guest agent and the guest server are closed to the agent user.
- Another org's actor on the same node is unreachable, and its sandbox is invisible to this org's driver.
- A restore with a wrong digest returns `ErrInvalid`, a restore into another org's workspace returns
  `ErrNotFound`, and the right digest restores.

## Limits

- The cross-org network spec proves the network half with two atespaces on one node. It does not prove
  separation between nodes.
- A result is for one tier and one runtime class. Run again for every class that you ship.
