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
| Keys | None. The run makes its own guest-API key; the driver writes the public half into the templates it creates. Never copy the production signing seed to the machine that runs this. |
| Snapshots | The snapshot bucket with a read-only key. `make test-escape-substrate` requires it: the restore spec fails without it. |
| Time | About 25 minutes: five sandboxes start (the probe sandbox, two in org B, the restore source and its clone). The Make target sets a 40 minute budget. |

The run does not need `KUBECONFIG`. Set `KUBECONFIG=/dev/null`.

## Variables

The full list, with defaults, is in the header of `test/escape/substrate_harness_test.go`. The required ones:

- `KOINON_ESCAPE_SUBSTRATE_CONTROL`: ateapi `host:port`.
- `KOINON_ESCAPE_SUBSTRATE_ROUTER`: atenet router `host:port`. The router serves TLS on 443 only and wants a client
  certificate whose SPIFFE ID is on its allow-list: also set `KOINON_ESCAPE_SUBSTRATE_ROUTER_CERT` (key and chain in
  one PEM file) and, when the CA differs from the ateapi CA, `KOINON_ESCAPE_SUBSTRATE_ROUTER_CA` (issue #1446).
- `KOINON_ESCAPE_SUBSTRATE_IMAGE`: the actor image.
- `KOINON_ESCAPE_SUBSTRATE_RUNTIME_CLASS`: the runtime class of the tier.
- `KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES`: the gateway routes, a comma-separated list of `host:port`. The driver
  refuses to create a sandbox without a gateway route (issue #1444), and the suite cannot start without one
  (issue #1508). Use the DNS name of the gateway Service, for example `koinon-gateway.koinon-system.svc:8080`.
  The host is a lowercase DNS name, not an IP address. The driver writes the routes into the egress policy of
  every sandbox. They are the only destinations that policy allows: default-deny stays for every other
  destination (ADR 0120). The egress gateway spec is the one exception: it replaces the policy of its own
  sandbox with an allow-all rule, as its control. Do not name a host that a deny probe targets (for example in
  `KOINON_ESCAPE_API` or `KOINON_ESCAPE_INTERNAL`) as a route, or that deny spec fails. The suite sends no traffic to them. A run without this input fails at once, with or
  without `KOINON_REQUIRE`; it does not skip.

`make test-escape-substrate` sets `KOINON_REQUIRE`, so a missing input below fails its spec; it does not skip.
Set these:

- `KOINON_ESCAPE_HOST_KERNEL`: `uname -r` of the worker node. The tier spec checks that the guest kernel
  differs.
- `KOINON_ESCAPE_API` and `KOINON_ESCAPE_INTERNAL`: Service `host:port` lists for the API server and the
  platform services (the atenet router, and the gateway by ClusterIP: the sandbox may reach the gateway by
  name only).
- `KOINON_ESCAPE_ATEAPI` and `KOINON_ESCAPE_ATELET`: `host:port` of ateapi and of atelet (a node IP and port).
- `KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_*`: the snapshot bucket, for the restore spec.
- `KOINON_ESCAPE_TIER`: `hardened` (default), `vm` or `dev`.
- `KOINON_ESCAPE_EGRESS_CONTROL` (default `example.com:80`): a destination that atenet-egress can reach. The
  egress gateway spec (issue #1363) uses it as its control: the spec that checks the refusal of metadata and
  loopback addresses only counts when this destination is let through.
- `KOINON_ESCAPE_EGRESS_REBIND` (default `169-254-169-254.sslip.io:80,127-0-0-1.sslip.io:80,127-0-0-1.sslip.io:15000,0-0-0-0.sslip.io:80`):
  `host:port` names that resolve to the refused addresses. The default needs the public wildcard DNS
  service sslip.io. Set your own names when the cluster has no route to it.

### Precondition of the egress gateway spec

The control spec fails unless atenet-egress has a route to `KOINON_ESCAPE_EGRESS_CONTROL`. The default
NetworkPolicies of the chart give atenet-egress no route to external addresses. Do one of these before you run
the suite: add the control destination to `workerNetworkPolicy.extraEgress`, or run the rig with the chart
policies off. The refusal spec also needs the DNS names above to resolve from the egress pod.

## Command

```sh
export KUBECONFIG=/dev/null
export KOINON_ESCAPE_SUBSTRATE_CONTROL=... KOINON_ESCAPE_SUBSTRATE_ROUTER=...
export KOINON_ESCAPE_SUBSTRATE_IMAGE=... KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES=koinon-gateway.koinon-system.svc:8080
export KOINON_ESCAPE_SUBSTRATE_RUNTIME_CLASS=... KOINON_ESCAPE_HOST_KERNEL="$(ssh node uname -r)"
make test-escape-substrate
```

`make test-escape-substrate` runs `go test ./test/escape/ -ginkgo.label-filter=substrate` with
`KOINON_ESCAPE_DRIVER=substrate`. It sets `KOINON_REQUIRE=escape-cluster-ips,host-kernel,substrate-targets,substrate-snapshots` (it adds to a value that you set), so a missing
cluster input is a failure.

## Expected output

On the gVisor tier, every spec passes, except two that report **not applicable** as a skip with a reason:

1. `runs in a microVM guest, not on the host kernel`: not applicable on `hardened` and `dev`
   (ADR 0075 decision 9). The `vm` tier is issue #1349.
2. `denies writes outside Landlock's read-write list`: gVisor has no Landlock, and the root is writable
   by decision (ADR 0120 section 4, issue #1150). The syscall half of the spec runs and passes. On the
   `vm` tier this spec must pass, and the harness fails it if Landlock is missing.

`holds the actor token only in the egress token file that koinon-agentd writes` runs on every tier. It
writes a real actor token with koinon-agentd's command, checks that the gateway's verifier accepts it, and
fails if the token or another JWT-shaped value is in any other place that it scans: a file under the
guest roots (`/dev/shm` included), or the environment or command line of a process. It runs only the
write command, not git or a harness. The agent user can
read the file: this is the interim of the ADR 0120 amendment of 2026-11-26, and issue #1386 is its exit.

The final line reads `0 Failed`, with these two among the skipped. Any other skip is a fault of the run.
The offline `contract` specs (token audience and lifetime) run in every `go test ./test/escape/` and need
no cluster.

## What the specs check

- Direct egress and cloud metadata are denied.
- No ServiceAccount token, SPIFFE socket or kube config in the guest. The API server and the platform
  services are unreachable.
- No credential and no JWT-shaped value in files or environment.
- No capabilities, `no_new_privs` and a seccomp filter. Privileged syscalls fail.
- On the gVisor tiers, the agent runs as the sandbox user: the real, effective and saved uid and gid
  (`getresuid`, `getresgid`) are 10001, and the supplementary groups are empty (issue #1387). The agent cannot open
  `koinon-guest-launcher` or `policy-gvisor.json` for writing, and cannot create a file in their
  directories. On the `vm` tier this spec reports **not applicable**: the payload may run as root in the
  microVM (issue #1496).
- The guest agent and the guest server are closed to the agent user.
- Another org's actor on the same node is unreachable, and its sandbox is invisible to this org's driver.
- A restore with a wrong digest returns `ErrInvalid`, a restore into another org's workspace returns
  `ErrNotFound`, and the right digest restores.

## Limits

- The cross-org network spec proves the network half with two atespaces on one node. It does not prove
  separation between nodes.
- If `NoNewPrivs` or `Seccomp` is missing from `/proc/self/status` on gVisor, the privilege spec fails on the
  missing field. Read the guest `/proc/self/status` before you blame the sandbox.
- A result is for one tier and one runtime class. Run again for every class that you ship.
