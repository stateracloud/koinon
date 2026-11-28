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
| Identity stand-in | The actor identity specs serve the gateway's egress code on this machine. atenet-egress must reach it by a DNS name, and this machine must reach the install's actor JWT issuer. See [the precondition](#precondition-of-the-actor-identity-specs). |
| Time | About 30 minutes: eight sandbox starts (the probe sandbox, two in org B, the restore source and its clone, the identity sandbox, and the actor of spec (f) two times). The Make target sets a 40 minute budget. |

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
  The host is a lowercase DNS name, not an IP address. The driver writes the routes into the egress policy of The first route is the model route. Every other route is an MCP route.
  every sandbox. They are the only destinations that policy allows: default-deny stays for every other
  destination (ADR 0120). The egress gateway spec is the one exception: it replaces the policy of its own
  sandbox with an allow-all rule, as its control. Do not name a host that a deny probe targets (for example in
  `KOINON_ESCAPE_API` or `KOINON_ESCAPE_INTERNAL`) as a route, or that deny spec fails. The suite sends one HTTP request to the first route, as the positive control of the deny specs. A run without this input fails at once, with or
  without `KOINON_REQUIRE`; it does not skip.

`make test-escape-substrate` sets `KOINON_REQUIRE`, so a missing input below fails its spec; it does not skip.
It also requires `envtest` for the 36 admission specs (label `substrate`) and sets `KUBEBUILDER_ASSETS` itself, with
`setup-envtest` (`make envtest-assets`). The machine needs a route to download the assets once. To use assets that
you already have, set `KOINON_ENVTEST_ASSETS=<dir>`. Without the assets the target fails. It does not skip.
Set these:

- `KOINON_ESCAPE_HOST_KERNEL`: `uname -r` of the worker node. The tier spec checks that the guest kernel
  differs.
- `KOINON_ESCAPE_API` and `KOINON_ESCAPE_INTERNAL`: Service `host:port` lists for the API server and the
  platform services (the atenet router, and the gateway by ClusterIP: the sandbox may reach the gateway by
  name only).
- `KOINON_ESCAPE_ATEAPI` and `KOINON_ESCAPE_ATELET`: `host:port` of ateapi and of atelet (a node IP and port).
- `KOINON_ESCAPE_SUBSTRATE_PEER_ADDRS`: comma-separated host addresses, without port, that org A can route to and
  that lead to org B's actor: the IP of the worker pod, and any other address on the path. The cross-org spec
  sends bytes to port 8099 on each of them and on the host of `KOINON_ESCAPE_SUBSTRATE_ROUTER`.
- `KOINON_ESCAPE_SUBSTRATE_SNAPSHOT_*`: the snapshot bucket, for the restore spec.
- `KOINON_ESCAPE_TIER`: `hardened` (default), `vm` or `dev`.
- `KOINON_ESCAPE_EGRESS_CONTROL` (default `example.com:80`): a destination that atenet-egress can reach. The
  egress gateway spec (issue #1363) uses it as its control: the spec that checks the refusal of metadata and
  loopback addresses only counts when this destination is let through.
- `KOINON_ESCAPE_EGRESS_REBIND` (default `169-254-169-254.sslip.io:80,127-0-0-1.sslip.io:80,127-0-0-1.sslip.io:15000,0-0-0-0.sslip.io:80`):
  `host:port` names that resolve to the refused addresses. The default needs the public wildcard DNS
  service sslip.io. Set your own names when the cluster has no route to it.
- `KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY`: `host:port` (a DNS name, not an IP) of the identity stand-in on
  this machine. The driver writes it as the code-host egress route, so atenet-egress adds the actor JWT to a
  clear-text request to it (issue #1386).
- `KOINON_ESCAPE_SUBSTRATE_IDENTITY_LISTEN` (default `:<port of the above>`): the local listen address of the
  stand-in.
- `KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_ISSUER` and `KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_CA_FILE`: the issuer of
  the install's actor JWTs and its CA, the same values as the gateway's `--actor-jwt-issuer` and
  `--actor-jwt-ca-file`.
- `KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_ISSUER_ADDR` (optional, `host:port`): the address that the stand-in dials for
  the issuer, for example `127.0.0.1:443` for a port-forward. The issuer name stays the TLS server name and the
  certificate is still checked against it and the CA file. Test only: the gateway has no such flag.
- `KOINON_ESCAPE_SUBSTRATE_GATEWAY_DIRECT`: the installed gateway's egress listener as `host:port` by Service
  name, Service IP and pod IP. Spec (e) connects to each one directly.

### Precondition of the actor identity specs

The stand-in is the gateway's own egress code (`egress.New`, the Substrate verifier and identifier), served
by the test process. The installed gateway cannot do this work: it knows only the actors in Koinon's State,
and the rig's sandboxes are not in it. The stand-in has its own State, which the specs fill. Its one route
is a forge route that never connects, so an accepted identity gets `502` with
`Koinon-Egress-Refusal: upstream_error`, and a refused one gets the refusal of the gate.

Before you run the suite:

1. Give the test machine a DNS name that the cluster resolves, for example `10-0-0-5.sslip.io` for
   `10.0.0.5`. Set `KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY` to that name and a free port.
2. Let atenet-egress connect to that address: add it to `workerNetworkPolicy.extraEgress`, or run the rig
   with the chart policies off.
3. Make sure that the test machine reaches the issuer. A port-forward alone is not enough: the issuer name (for
   example `idp.ate-system.svc`) does not resolve on the test machine. Start the port-forward to the issuer
   Service and set `KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_ISSUER_ADDR=127.0.0.1:443` (the forwarded port). Do not edit
   `/etc/hosts`. The issuer certificate must still be valid for the issuer name.

The specs run in one process (`Serial`), because the stand-in listens on one port.

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
export KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY=10-0-0-5.sslip.io:18081
export KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_ISSUER=... KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_CA_FILE=...
export KOINON_ESCAPE_SUBSTRATE_GATEWAY_DIRECT=koinon-gateway.koinon.svc:8081,<service IP>:8081,<pod IP>:8081
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

The container `Substrate actor identity` (`test/escape/substrate_identity_test.go`) runs on every tier. It
is the Proof list of the ADR 0120 amendment for issue #1386. Each control name starts with its letter:

- `holds no actor token and no signing key, and its git configuration sends only the placeholder`: the
  guest has no token directory or file, no readable file, environment or command line holds a JWT or a
  private key (PEM or JWK), and koinon-agentd's git configuration holds only the placeholder. Its control
  is a git request with that configuration (or the probe, when the image has no git) that the gateway
  accepts.
- `(a) a request with the placeholder reaches the gateway with the actor's JWT, and the gateway accepts it`.
- `(b) a request without the actor token header reaches the gateway without a token, and gets 401`.
- `(c) a JWT that the guest forged is replaced, not passed through`.
- `(d) a request with two actor token values is refused, or reaches the gateway with one minted value`. It
  also sends two copies of a real token to the stand-in and expects `401`.
- `(e) a direct connection to the gateway's egress listener, past atenet-egress, is blocked`.
- `(f) the name of a deleted actor is refused, and its token does not work for the actor that replaces it
  (403)`. The driver refuses a create under the deleted actor's name with `execution.ErrNameReused` (issue
  #1092: atenet-egress caches a token by actor name), and the replacement actor has a new name.
- `(g) SHIP GATE: forwarded-identity headers that the guest sets (XFCC, x-forwarded-*) do not change the
  identity`. Each variant must get the same result as the request without the headers. A failure starts
  with `SHIP GATE` and names the variant. When it fails, issue #1386 does not ship.
- `(h) an https request to the egress URL gets no token`.

The final line reads `0 Failed`, with these two among the skipped. Any other skip is a fault of the run.
The offline `contract` specs (token audience and lifetime) run in every `go test ./test/escape/` and need
no cluster.

## What the specs check

- Direct egress and cloud metadata are denied. The specs send bytes (see [A connect proves nothing](#a-connect-proves-nothing-on-substrate)).
- No ServiceAccount token, SPIFFE socket or kube config in the guest. The API server and the platform
  services are unreachable.
- No credential and no JWT-shaped value in files or environment.
- The guest holds no actor token and no signing key. atenet-egress adds the actor's JWT outside the guest,
  replaces a value that the guest sends, ignores forwarded-identity headers, and adds nothing through TLS. A
  token of a deleted actor gets `403`.
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

## A connect proves nothing on Substrate

On Substrate the guest TCP stack does not leave through the pod network namespace. Each actor has its own network
namespace. Its nftables rules send every TCP connect to a local atunnel listener. The listener accepts the connection
first, and then opens a CONNECT tunnel to atenet-egress, and that CONNECT is accepted. atenet-egress reads the Host or the SNI
and decides then. It never dials the IP address that the guest chose. So `connect()` succeeds for every address, and a probe that only connects reports
"open" for all of them (issue #1515; in the atenet-egress log, every probe target was a CONNECT with code 200 and 0
bytes up and down).

The deny specs use the probe op `payload` (`test/escape/testdata/probe.py`). It sends a TLS ClientHello (or an HTTP
request on port 80, 8080 or 8099) and reads the answer. A target is denied when:

- the connect fails (`ECONNREFUSED`, `ENETUNREACH`, `EHOSTUNREACH`, `ECONNRESET`, `EACCES`, `EPERM`, `timeout`,
  `gaierror`), or
- the connection closes with no byte (`closed`: the tunnel closed after atenet-egress refused the CONNECT), or
- the answer is the egress gateway's own 403, 502 or 503 (an Envoy marker and no `x-envoy-upstream-service-time`).

Use only deny targets that answer bad input with a byte (a TLS or HTTP server, an SSH banner). A service that
closes with no byte on bad input, such as CoreDNS on TCP 53, looks like a denial. A byte from the target, or a result that is not in this list, fails the spec. Each deny spec runs a positive control in
the same probe: an HTTP request to the first `KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES` entry must answer with a status.
The spec reports every target, not only the first that fails. Read the result of the IPv6 target
(`[2606:4700:4700::1111]:443`) in the Ginkgo log: a guest with no IPv6 route gives `ENETUNREACH`, which is a denial,
and a guest that connects gives `closed`.

gVisor has no network fence in the guest. So the `ateapi` and `atelet` spec expects no byte back (not `EACCES` or
`EPERM`): the refusal is outside the guest. The cross-org spec uses `169.254.17.2` as a control only. That address is
the same constant in every actor, each in its own network namespace, so it is not a collision: org A must get
`ECONNREFUSED` on it while org B's listener is up, and the reach to org B is tested on routable addresses
(`KOINON_ESCAPE_SUBSTRATE_PEER_ADDRS` and the router).

## Limits

- The cross-org network spec proves the network half with two atespaces on one node. It does not prove
  separation between nodes.
- If `NoNewPrivs` or `Seccomp` is missing from `/proc/self/status` on gVisor, the privilege spec fails on the
  missing field. Read the guest `/proc/self/status` before you blame the sandbox.
- The routable part of the cross-org spec has no control that port 8099 is open on those addresses. It proves that
  no byte comes back, as the direct egress spec does. It does not prove reach to org B's listener.
- A result is for one tier and one runtime class. Run again for every class that you ship.
- Spec (e) cannot tell which fence stopped the direct connection: atenet-egress, which has no rule for the
  listener, or the NetworkPolicy fence of the chart. It proves that the request did not reach the gateway's
  egress code. The chart checks prove the NetworkPolicy fence alone.
- The actor identity specs use the gateway's code in the test process, not the installed gateway. They prove
  what atenet-egress sends, and how the gateway's code decides on it.
