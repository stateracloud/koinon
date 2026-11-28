<!-- SPDX-License-Identifier: Apache-2.0 -->

# Substrate install step and preflight

Agent Substrate is a pinned cluster prerequisite, not part of the chart
([ADR 0119](../adr/0119-agent-substrate-execution-layer.md) D4). Run the installer step before
`helm install` with `execution.driver: substrate`. The release namespace may not exist at that time:
the installer step creates nothing there.

Substrate releases ship no `ate-setup` binary, and `ate-setup` applies the manifests of a source tree.
So the installer builds `ate-setup` itself, from the pinned commit. You give it a git clone of Substrate;
it trusts nothing else in that clone:

```sh
git clone https://github.com/agent-substrate/substrate /path/to/substrate
koinon-api substrate install --substrate-source /path/to/substrate \
  --kubeconfig <file> --context <context> \
  --probe-image <koinon-api image> --storage-bucket <bucket> \
  --platform generic
```

`--platform` is described [below](#platforms). On `generic` there is no vm tier, so you give no
`--substrate-worker-pool vm=...`.

## Platforms

`ate-setup` v1.0.1 has two install routes: GKE (GCS for snapshots, a Google service account, the GKE
credential provider) and kind. The kind route is mostly "not GCP": it installs an in-cluster object store
(rustfs) for snapshots and an atelet without the GKE credential-provider hostPaths, which a read-only-root
node such as Talos cannot create (upstream issue #2669). So Koinon uses it on any Kubernetes flavor and
corrects what is only right for a laptop. `--platform` chooses:

| `--platform` | `ate-setup` route | Use it for |
| --- | --- | --- |
| `generic` | `--kind` with Koinon's corrections (below), gvisor sandbox class only | Any cluster that is not kind or GKE: Talos, kubeadm, k3s, EKS and so on. The installer host needs no docker. |
| `kind` | `--kind`, unchanged (microVM assets are staged with docker on the installer host) | A local kind cluster. The installer reads the nodes first and refuses it unless every node has a `kind://` providerID, before it creates anything. |
| `gke` | no `--kind` (GCS) | GKE. `--gcp-project-id` and `--snapshot-transfer-gsa-name` go to `ate-setup` (it reads `PROJECT_ID` and `SNAPSHOT_TRANSFER_GSA_NAME` when you leave them out). |

Without `--platform` the installer reads the nodes: `kind` when every node has a `kind://` providerID,
else `generic` (with a warning when every node is a GKE node). It never detects `gke`: ask for it. `--kind` still works and means `--platform kind`; it is
deprecated.

### What `generic` corrects

`ate-setup` has no overlay hook at this pin: it picks `manifests/ate-install/kind` by name and reads the
manifests from the source tree. The installer already owns that tree (the temporary export of the pinned
commit), so it writes the corrections into the export before `ate-setup` runs: kustomize patch files that the
kind kustomization lists, and a few exact text edits where a patch cannot reach (items of an args list). An
edit that does not find its text exactly once stops the install: a changed pin is never corrected by a
half-fitting edit. The corrections:

1. The sandbox class is `gvisor` only. The microVM class stages its guest assets with
   `docker run --network container:<node>` on the installer host, for the host architecture, which only works
   on a kind node. `--sandbox-classes` other than `gvisor`, and `--setup-csi hostpath` or `both` (they reach
   into a kind node over docker), are refused on `generic`. The installer also turns the node-device probe of
   the vm tier off. Microvm asset staging from inside the cluster is follow-up work.
2. The snapshot bucket exists. `ate-setup` sets the snapshot location to `gs://<--storage-bucket>/`, but the
   rustfs bucket-init Job creates only `ate-snapshots`. The Job now creates `--storage-bucket`.
3. Storage. The object-store volume is `--rustfs-storage-size` (default 10Gi) with `--rustfs-storage-class`;
   the bundled PostgreSQL volume is `--postgres-storage-size` (default 20Gi) with `--postgres-storage-class`.
   Without a class name the cluster default StorageClass is used. The kind route has 1Gi for both.
4. Credentials. The object-store root credentials are random keys in the Secret `koinon-rustfs-credentials`
   in the Substrate namespace, not the fixed `rustfsadmin`. The installer creates the Secret once and keeps
   it. A Secret that lacks a key is an error that names the Secret; the installer never rewrites it and never
   prints a key. The installer makes no other key. The Koinon reconciler only reads the snapshot bucket
   (`--substrate-snapshot-*`), so give it its own read-only key for that bucket, made by hand with the
   store's own tools. Never give it the root key: that key can write and delete every snapshot.
5. atelet logs at `info` (the kind overlay sets `debug`) and has no local-registry replacement.

The kind route also brings its observability and sizing: an `otel-system` namespace with an OpenTelemetry collector
and Prometheus, `OTEL_TRACES_SAMPLER=parentbased_always_on` on the api server, and a CPU request of 250m and
limit of 2 on PostgreSQL. `generic` keeps them. With `--setup-csi nfs`, `ate-setup` checks the kernel of the
installer host (`/proc/filesystems`), not that of the nodes. The root credentials of rustfs come from the Secret
on every start; the installer cannot tell that a recreated Secret no longer matches data written with older keys.

Volume claims are immutable once created. Before anything is created, the installer refuses a changed class or a
smaller size for the existing `rustfs-data` claim, and any changed size or class of the PostgreSQL volume. The
bucket-init Job deletes itself 5 minutes after it finishes, so a run with another bucket creates a new Job.

`ate-setup --kind` starts every deploy with `docker info`. On `generic` the installer puts a small `docker`
script first in the `PATH` of `ate-setup` that answers that one call and fails any other docker call with a
message. Exit condition: upstream plans a generic install
([agent-substrate/substrate #1782](https://github.com/agent-substrate/substrate/issues/1782)); Koinon moves to
it and drops the corrections and the script. There is no upstream Helm chart yet (#79), and Koinon does not fork.

A test renders the real pinned tree with the generic overlay and checks the objects that `kubectl kustomize`
makes (`TestGenericOverlayRendersThePinnedTree`): no `rustfsadmin`, the Secret references, the bucket-init Job and
its bucket, `10Gi` and `20Gi`, and atelet at `--log-level=info`. It needs a clone of Substrate that holds the
pinned commit in `KOINON_SUBSTRATE_SOURCE`, and `kubectl`. Without them it skips with the requirement names
`substrate-source` and `kubectl`; `KOINON_REQUIRE` turns the skip into a failure. A second test
(`TestRenderChecksFindWhatTheyLookFor`) runs the same checks on a small tree of the same shape and needs
`kubectl` only.

The storage preflight of `generic` fails, before anything is created, when a named StorageClass does not
exist, or when a volume has no named class and the cluster has no default class.

## Egress gateway corrections

The egress gateway of Substrate (`atenet-egress`, Envoy) is the same on every platform. The installer edits its
manifest in the exported tree before `ate-setup` reads it (`internal/substrateinstall/egress.go`). Each edit fails
the install when the pinned text is not there the expected number of times.

1. **No upstream TLS session reuse** (always on, issue [#1486](https://github.com/stateracloud/koinon/issues/1486)).
   Envoy keeps one upstream TLS session cache for each forward-proxy cluster. The cache is not keyed by SNI and keeps
   only the newest session. Cloudflare can resume a session of one host under the SNI of another host. Envoy checks
   the certificate again and refuses the call with `expected SAN api.anthropic.com, got npmjs.org`. A failed resume
   stores no new session, so every later call to that host fails for all actors until the pod restarts. The cache is
   also shared by all actors. The installer sets `max_session_keys: 0` in the `UpstreamTlsContext` of
   `egress_forward_proxy` and `egress_forward_proxy_grpc`. Exit condition: remove the edit when a Substrate release sets
   it, or keys the sessions by SNI.
2. **IPv4-first DNS** (`--egress-dns-v4-preferred`, default off). Substrate resolves with `dns_lookup_family: ALL`.
   On a cluster without an IPv6 route, a call can fail at random with `503 Network is unreachable`. The option sets
   `V4_PREFERRED`. It is a setting of the installer: Substrate is not part of the chart, so the chart has no value for it.

`TestEgressCorrectionFitsThePinnedTree` checks the real pinned manifest. It needs a clone of Substrate in
`KOINON_SUBSTRATE_SOURCE`, like the generic overlay test.

### Koinon's own gateways (issue #1486 item 3)

No Koinon gateway reuses an upstream TLS session across hosts:

- Go gateways (`koinon-gateway`: egress, model and MCP paths): none sets `tls.Config.ClientSessionCache`, and Go
  resumes a client session only when that field is set. `internal/gateway/egress/egress.go:184` and
  `internal/gateway/actortoken/substrate.go:293` build the `tls.Config` with `MinVersion` and `RootCAs` only.
  `internal/mcpgateway/upstream.go:298` and `:315` copy a transport and set roots, `MinVersion` or
  `InsecureSkipVerify` only. `internal/modelgateway/gateway.go:107` clones `http.DefaultTransport`, which has no
  `TLSClientConfig` session cache. A test would have to set a cache first: the code has none.
- agentgateway fallback (v1.7.x): the backend TLS client (`crates/agentgateway/src/http/backendtls.rs:322`) is a
  rustls `ClientConfig` with default resumption. The rustls session store keys sessions by server name, so a session
  of one host is never offered to another. This was read in the agentgateway `v1.7.0` source, not run against
  Cloudflare. agentgateway is deleted in ADR 0097 step 3.

## Options

Options passed to `ate-setup` under the names the pinned `ate-setup` defines:

| `koinon-api substrate install` | `ate-setup` | When you need it |
| --- | --- | --- |
| `--platform generic\|gke\|kind` | `--kind` or none | See [Platforms](#platforms). |
| `--postgres-storage-class <name>` | `--ateapi-postgres-storage-class` | The cluster has no default StorageClass, or you want another class: the bundled Postgres claim stays Pending without one. |
| `--postgres-storage-size`, `--rustfs-storage-class`, `--rustfs-storage-size` | none (Koinon patch) | `generic` only. See [What `generic` corrects](#what-generic-corrects). |
| `--gcp-project-id`, `--snapshot-transfer-gsa-name` | the same names | `gke` only. |
| `--setup-csi nfs\|none` | `--setup-csi` | A CSI driver should be installed with Substrate (default `none`). `hostpath` and `both` are for kind only; `generic` refuses them. |
| `--gvisor-mirror <base-url>` | none (Koinon step) | The installer host cannot reach `storage.googleapis.com`. See [gVisor tarball in the cluster object store](#gvisor-tarball-in-the-cluster-object-store). |
| `--egress-dns-v4-preferred` | none (Koinon edit) | The cluster has no IPv6 route and egress calls fail at random with 503. See [Egress gateway corrections](#egress-gateway-corrections). |
| `--expected-jwt-issuer <url>` | `--ateapi-expected-jwt-issuer` | See [Service-account issuer](#service-account-issuer). |

`--kubeconfig` and `--context` are required. The installer uses them for its own API calls and for every
`ate-setup` call, and it never acts on the ambient current context. `--substrate-probes=false` is refused:
the probe pods are the prerequisite checks.

The pin is in one place: `internal/preflight/substratepin.go` (version and commit of the release tag).
The installer:

1. Checks that the pinned commit exists in the clone and that git resolves it to the pinned SHA.
2. Runs `git -c core.fsmonitor= archive <commit>` into a new temporary directory. It does not use the
   working tree of the clone.
3. Builds `ate-setup` there with `go build -trimpath`, with the pinned version and commit set in the build.
4. Requires that `ate-setup --version` reports the pinned version and exactly the pinned commit. A
   `-dirty` suffix or any other commit is refused.
5. Runs `ate-setup` from the exported tree, and deletes the temporary directory at the end.

After the install, the preflight accepts only an api server image with exactly the pinned tag. An api
server image with no tag fails the preflight. The preflight finds the api server container by name
(`ate-api-server`), not by position.

The images are pinned by tag, not by digest. When Substrate publishes image digests with a release, they
get pinned in `substratepin.go`. The images live at `ghcr.io/agent-substrate/substrate/<component>:v1.0.1`,
named by component (`ateapi` for the container `ate-api-server`), not by Deployment. The preflight reads the
tag of the image of that container and never the image name.

Order of the step:

1. Build and check `ate-setup` (above).
2. Run the prerequisite checks that need no namespace: the Kubernetes version and whether the service-account
   issuer can be read. When one fails, nothing was created.
3. Create the probe namespace (`--substrate-probe-namespace`, default `koinon-substrate-probe`) when it is
   missing, with Pod Security `enforce`, `warn` and `audit` all `privileged` (the device probe mounts hostPath
   devices). The probe pods run there, never in a Substrate namespace: Substrate's admission policies refuse a
   bare pod in `ate-system` and in the worker namespace, and they have no exception for the probes (issue #1176).
   The installer refuses a probe namespace that is a Substrate namespace.
4. Run the probe checks (node devices, user namespaces, issuer reachability). Before each probe pod starts, the
   prober deletes stale probe pods that an earlier run left (same `component` and `part-of=koinon` labels). When
   a check fails, the installer stops and nothing of Substrate is created. When the installer created the probe
   namespace, it deletes it when it stops, after a failure or after step 10. A namespace that existed before is
   left as it is.
5. Create the Substrate namespace (Pod Security `enforce`, `warn` and `audit` all `privileged`) when it is
   missing. On the `generic` platform the object store credentials go there.
6. Run `ate-setup deploy ate-system` with pre-built release images of the pinned tag.
7. Wait for the CRDs to be Established (applying a `SandboxConfig` right after the CRDs raced in the lab).
8. Make sure the router allows `spiffe://cluster.local/ns/<substrate namespace>/sa/atenet-client`. The
   installer finds the router container by name (`atenet-router`), reads the flag in its command and
   arguments, and retries when the Deployment changed during the update. It also allows agentd's pod
   certificate, `spiffe://cluster.local/ns/<koinon namespace>/sa/<agentd service account>` (flags
   `--koinon-namespace` and `--koinon-release` name them). The router serves TLS on Service port 443 and wants
   a client certificate on this list, so set `execution.substrate.routerAddress` to
   `atenet-router.<substrate namespace>.svc:443` (issue #1446, ADR 0120 amendment). The preflight fails when the
   list lacks the agentd identity or the router Service has no such port.
   The driver fails a run at once with "TLS identity check with the atenet router failed" when the router sends a
   TLS alert or the driver rejects the router certificate. Known limit: on a real TCP connection, a router that
   closes the connection with unread client bytes can send a TCP reset, and the reset can hide the TLS alert. The
   driver cannot read the alert then. The error stays Unavailable and the driver retries until
   `GuestReadyTimeout`. If runs wait that long with no error, check the router log and the allow-list.
9. Stage the gVisor tarball (kind and generic platforms, gvisor class). See "gVisor tarball in the cluster
   object store" below.
10. Run the full preflight, without the NetworkPolicy probe. That probe starts two pods in the release
    namespace, which may not exist yet. The chart's preflight hook runs it at `helm install`.

`koinon-api substrate install` and `koinon-api preflight --driver substrate` give the probe pods the Secrets that
`--probe-image-pull-secrets` names. These Secrets must exist in the probe namespace, not only in the release
namespace: a namespace that the run creates has no Secrets. Make the probe namespace and copy them before the run,
or use a probe image that needs no pull secret. A probe namespace that you make must have Pod Security `enforce`
`privileged`: the device probe mounts hostPath devices, and the run does not change the labels of a namespace that
it did not create. How the probe namespace pulls the probe image is part of S2 (chart work).

`koinon-api preflight --driver substrate` runs only the checks. It creates and removes the probe namespace in
the same way, so it also runs after `helm install`.

### Golden snapshots

Golden snapshots are off by default in every tier (issue #1492). The Koinon template sets
`GOLDEN_SNAPSHOT_MODE_DISABLED`. To turn them on, set `execution.substrate.goldenSnapshots=true` in the chart
(the reconciler flag is `--substrate-golden-snapshots`). The setting is part of the template hash, so a
change makes new templates. An upgrade to this version, and each change of the setting, make the reconciler
recreate every Substrate sandbox, because the sandbox spec hash changes.

Turn it on only with a reason. A golden snapshot clones memory from one sandbox to another. ADR 0120
section 5.2 forbids that, and an admin who turns it on makes an exception to that rule.

Cost, from the koinon-lab experiment of 2026-11-27 (lab-notes `experiments/2026-11-27-substrate-edges`,
sections O8 and O8 vm tier). Time from create to the first successful Exec, 20 sandboxes per arm, 0 failures:

| Tier | Golden off p50 / p95 | Golden on p50 / p95 |
|---|---|---|
| gVisor | 1.03 s / 1.42 s | 1.05 s / 1.58 s |
| vm (microVM) | 2.65 s / 2.78 s | 3.50 s / 3.64 s |

- Golden mode was not faster in these runs. On the vm tier it was slower.
- A golden build takes 36-38 s. It holds a pool slot for about 21-23 s each time a template changes.
- All runs used the 40 MB/s lab disk cap. The times include an extra chmod call, so they are an upper bound.

### Permissions of the installer

The kubeconfig of the installer needs these rules:

- `namespaces`: get, create, delete (the probe namespace and the Substrate namespace)
- `nodes`: list (platform detection, the kind check and the gVisor user-namespace probe)
- `storageclasses` (group `storage.k8s.io`): list (the `generic` storage preflight)
- `secrets` in the Substrate namespace: get, create (the `generic` object-store credentials)
- `serviceaccounts` and `configmaps` in the probe namespace: get (the wait for `default` and `kube-root-ca.crt`)
- `pods`: create, get, delete, list in the probe namespace; `pods/log`: get
- `events`: list in the probe namespace (the probe reads the kubelet event of a pod that did not start)
- `customresourcedefinitions`: get
- `deployments`: get, update in the Substrate namespace
- `workerpools` (group `ate.dev`): get in the worker namespace (`--substrate-worker-namespace`)
- non-resource URL `/.well-known/openid-configuration`: get
- for the provisioner grant (ADR 0128): `clustertrustbundles` (group `certificates.k8s.io`, cluster scope): list (the CA of
  the api server); `pods` in the Substrate namespace: list; `serviceaccounts/token`: create for `ate-client` in the
  Substrate namespace; `pods/portforward`: create in the Substrate namespace

`ate-setup` needs more (it creates the Substrate objects); those rules are those of Substrate.

## What the preflight checks

- Kubernetes 1.37 or later (D8).
- With the vm tier: one node mounts `/dev/kvm`, `/dev/net/tun`, `/dev/vhost-vsock` and `/dev/vhost-net`
  (a probe pod with `CharDevice` host paths; only the node it lands on is checked, so use
  `--substrate-node-selector` to aim it at workers).
- With the gVisor class (`--substrate-gvisor`, on by default; the installer sets it from `--sandbox-classes`):
  `user.max_user_namespaces` is above 0 on every node that `--substrate-node-selector` selects (a short
  read-only probe pod per node; the check needs `nodes`: list). Without the selector it probes
  the nodes that are Ready, not cordoned and without a `NoSchedule` or `NoExecute` taint (so not the control
  plane), and lists the others as not probed. With the selector it probes the selected nodes that are Ready.
  See "Talos: user namespaces for gVisor".
- `/var/lib/ate` has a filesystem of its own on each worker node (a read-only probe pod per node, the same
  node selection as the user-namespace check). The check only warns: a node where `/var/lib/ate` shares the
  filesystem of the node root or of `/var/lib/kubelet` gets a warning. A node without `/var/lib/ate` gets a
  warning too. See "Node protection: /var/lib/ate on its own filesystem".
- Substrate's api server can validate service-account tokens. The probe pod runs in the probe namespace. See below.
- The Substrate CRDs (`workerpools.ate.dev`, `sandboxconfigs.ate.dev`, `csidriverconfigs.ate.dev`),
  a ready `ate-api-server` of exactly the pinned release, the `atenet-router` allow-list, and per tier a
  WorkerPool with all its workers ready (`status.readyReplicas`) and every sandbox class resolved.

### The chart's preflight hook

With `execution.driver: substrate`, the chart's pre-install and pre-upgrade hook Job runs
`koinon-api preflight --driver substrate --substrate-probes=false`. Thus `helm install` and `helm upgrade` stop when:

- the cluster runs Kubernetes below 1.37,
- the Substrate CRDs are missing,
- `ate-api-server` in `execution.substrate.namespace` is missing, not ready or not the pinned release,
- `atenet-router` does not allow the atenet client.

It also runs the NetworkPolicy probe in the release namespace when `preflight.networkPolicyProbe.enabled` is
true, as with the OpenShell driver.

The hook does not run the probe pods (issuer, node devices, user namespaces, `/var/lib/ate` filesystem). These pods need a probe namespace
with Pod Security `privileged`, and a hook must not create a namespace. The hook reports these four checks as
warnings. `koinon-api substrate install` runs them, and `koinon-api preflight --driver substrate` runs them
again on request. The hook does not check the WorkerPools: at pre-install the chart's own pool does not exist
yet.

The ClusterRole of the hook is read-only: `get` on the three Substrate CRDs and on the Deployments
`ate-api-server` and `atenet-router` by name (a ClusterRole, because Substrate's namespace may not exist yet),
plus the CNPG read of the chosen mode. The NetworkPolicy probe adds a Role in the release namespace only: create, get and delete pods,
get pod logs, and create and delete NetworkPolicies.

## Node protection: /var/lib/ate on its own filesystem

**v0.1 has no hard per-home cap.** A sandbox home has no size limit of its own in v0.1. The tracking issue for
per-sandbox volumes and a hard cap is [#1495](https://github.com/stateracloud/koinon/issues/1495). Until it is
done, Koinon protects the node and not the single home: a full home must fill a filesystem that only sandbox
homes use.

Why the node needs this:

- The gVisor home is a tmpfs that a file under `/var/lib/ate/actors/<uid>` backs. The microVM home is a host
  directory in the same place. `/var/lib/ate` is a hostPath that kubelet does not see.
- A write of 3 GiB into a home lands on the node filesystem. Kubelet reports 0 bytes for it in the pod stats of
  the sandbox. When `/var/lib/ate` shares the node filesystem, kubelet sees the free space fall and sets disk
  pressure. The sandbox pods report no use for the home, so the eviction does not target the large home.
- Substrate sets the hostPath of the worker in its own code, and one worker can hold up to 1000 actors. A volume
  for each worker therefore cannot cap one home.

What to do on each worker node:

1. Mount a separate disk or partition at `/var/lib/ate` before kubelet starts. Then a full home fills that
   filesystem. The root filesystem and the kubelet filesystem stay free.
2. You can also mount a separate disk at `/var/lib/ate/snapshots`. Snapshot data then cannot fill the home
   filesystem. See Substrate `docs/guides/node-storage.md`.
3. Make the filesystem large enough for the homes that you expect, plus a margin, because no cap limits one home.
   How you mount it depends on the node OS. On kubeadm nodes, use an `/etc/fstab` entry or a systemd mount unit.
   There is no tested method for Talos yet. On Talos, `/var` is the only writable filesystem, and a Talos user
   volume mounts under `/var/mnt`. Do not use a kubelet `extraMounts` entry for this: it changes the mount
   namespace of the kubelet only, and the data of the home stays on `/var`.

The check compares device numbers. A btrfs subvolume or a ZFS dataset has its own device number, but it shares
the space of its pool. The preflight does not see this case, so it reports no warning for it.

How Koinon helps you check it:

- **Preflight.** `koinon-api preflight --driver substrate` and `koinon-api substrate install` warn on each node
  where `/var/lib/ate` is on the same filesystem as the node root or as `/var/lib/kubelet`. The warning does not stop the install.
  The chart's pre-install hook does not run the probe pod, as for the other probe checks.
- **Alert.** With `metrics.prometheusRule.enabled` and `execution.driver: substrate`, the rule
  `KoinonSandboxHomeFilesystemFilling` fires when less than 15 percent of the filesystem at `/var/lib/ate` is free
  for 10 minutes (`metrics.prometheusRule.ateFilesystem`). It reads the series `node_filesystem_avail_bytes` and
  `node_filesystem_size_bytes` of node-exporter for the mount point `/var/lib/ate`. So it needs node-exporter on the
  worker nodes, and it stays silent on a node where `/var/lib/ate` is not a mount point of its own. That is the case
  that the preflight warns about. The preflight runs at install and on request only. Run
  `koinon-api preflight --driver substrate` again after you add a worker node.
- **The leak-guard byte alert does not cover this.** The byte cap of ADR 0091 (issue #1058) counts the bytes of
  MCP calls that leave a run. It does not see the disk of a node. It cannot fire on a full home, so the node-disk
  alert above is a separate rule.

## Access in Substrate: the provisioner

Only Substrate's global `owner` role can create an atespace, and Koinon makes one atespace for each org. So a
small pod of its own, the provisioner, holds that role. The reconciler and agentd hold no global role
([ADR 0128](../adr/0128-substrate-provisioner.md)).

| Component | Principal | Role |
| --- | --- | --- |
| provisioner | `system:serviceaccount:<ns>:<release>-provisioner` | `owner` in the global policy |
| reconciler | `system:serviceaccount:<ns>:<release>-reconciler` | `editor` in each org atespace |
| agentd | `system:serviceaccount:<ns>:<release>-agentd` | `viewer` in each org atespace |

- **The installer** adds the provisioner to the global access policy `default` as a revocable member, never as a
  bootstrap owner. It reads the policy, adds the member and writes it back with the uid and version it read. It
  keeps every other member. It writes nothing when the member is there, so run `koinon-api substrate install`
  again on every upgrade: an install from before the provisioner gets the grant and keeps its other members.
- `--koinon-namespace` (default `koinon`) and `--koinon-release` (default `koinon`) name the provisioner's
  ServiceAccount. Pass the namespace and the release name of your `helm install`.
- The installer acts as `ate-client`, the default bootstrap owner: it mints a short-lived token and calls ateapi
  through a port-forward to the api server pod. If your install names other bootstrap owners, pass a token of a
  global owner with `--api-token-file`. The installer checks the api server certificate before it sends the token.
  It reads the CA from the ClusterTrustBundle of Substrate's `servicedns.podcert.ate.dev/identity` signer; give another
  CA with `--api-ca-file`. When no CA is found, the install stops.
- **The chart** renders the provisioner (`execution.substrate.provisioner`, on by default): a Deployment of the
  reconciler image in `provision` mode, its own ServiceAccount, a ClusterRole that only creates TokenReviews, a
  Service and a NetworkPolicy that admits only the reconciler. The reconciler asks it for an org atespace only when
  the atespace is missing or its policy does not give the reconciler a role yet.
- Substrate has no scoped create role. When a Substrate release adds one, check whether the provisioner can be
  removed (issue #1398).

## Service-account issuer

The probe repeats what Substrate v1.0.1's api server does to validate a token, from a pod with the default
ServiceAccount of the probe namespace (source at commit `20778a6d`). The issuer and the `iss` of a token are the
same in every namespace. The probe pod does not pass the NetworkPolicies of `ate-system`: before the install there
are none, and after the install they allow the api server's own egress, not the probe's.

- `internal/oidcjwt/oidcjwt.go` `discoverKeysForIssuer` (line 606): `GET <issuer>/.well-known/openid-configuration`,
  the document's `issuer` must equal the configured issuer, then `GET` its `jwks_uri`.
- `cmd/ate-setup/internal/steps/create.go` `kubernetesJWTProvider` (line 253): for the in-cluster issuer
  `https://kubernetes.default.svc` the provider carries the pod's own `ca.crt` and `token`. For any other issuer
  it has neither: system roots, no credential.
- `internal/oidcjwt/http.go` `issuerDiscoveryTransport.RoundTrip` (line 146): the token goes on requests under the issuer and
  on `https` requests to `/openid/v1/jwks`, so a `jwks_uri` on the API server's address works.
- `internal/oidcjwt/oidcjwt.go` `Verify` (line 277): a token is accepted only when its `iss` equals the issuer.

The issuer Substrate trusts is `--expected-jwt-issuer` when you give it, else the `issuer` of the API server's
discovery document (`ate-setup` `jwtIssuer`, create.go line 229). The cluster's own issuer string is shown as
information. The check fails when:

- the discovery document or the JWKS cannot be fetched by that path from a pod. On a cluster whose own issuer
  is an address pods cannot route to (an Omni or Talos IPv6 unique-local address), the default issuer fails
  this way, and every call then fails with "invalid bearer token";
- the connection works only without TLS verification;
- the `iss` claim of the pod's own token differs from the issuer Substrate would trust ("unexpected issuer").

Supported fixes, in this order:

1. Set the API server's first `--service-account-issuer` to `https://kubernetes.default.svc`. Substrate then
   reads discovery from the in-cluster API server with the pod's token, and pods need no route to anything else.
   On Talos, set `cluster.apiServer.extraArgs`. Later `--service-account-issuer` values are only accepted for
   validation, not used to sign: keep the old value as the second one so tokens already issued stay valid.
   The issuers do not keep the audience: without `--api-audiences` Kubernetes accepts only the first issuer as
   audience, so existing tokens with the old default audience fail. Set `--api-audiences` to the new and the old
   issuer during the change. Substrate still accepts only tokens whose `iss` is the issuer it trusts.
   When the discovery document's `jwks_uri` is not reachable from pods, also set `--service-account-jwks-uri`
   to `https://kubernetes.default.svc/openid/v1/jwks`.
2. Or set it to a URL that pods reach with a certificate from the system roots.

The real requirement: pods in the Substrate namespace reach the issuer Substrate trusts, because Substrate v1.0.1
has no separate discovery URL (`cmd/ateapi/main.go` line 674 passes the issuer to `HTTPKeySource`). Kubernetes
signs tokens with the first issuer and writes the same value as `issuer` in the discovery document, so
`--expected-jwt-issuer` cannot turn an unroutable issuer into a reachable one. It is only an override that must
equal the `iss` of the cluster's tokens. In particular, `--expected-jwt-issuer=https://kubernetes.default.svc` on a
cluster whose tokens carry another `iss` fails twice: the discovery document names the other issuer (oidcjwt.go
line 618) and every token is rejected as "unexpected issuer" (line 277).

The API server serves the discovery document and the JWKS to every service account through the
`system:service-account-issuer-discovery` ClusterRoleBinding. A sidecar that serves a copy of the discovery
document on the issuer address is not a supported install.

### The Omni and Talos case: the safe patch

When the check fails because the cluster's own issuer is not reachable from a pod, its Fix text prints the
exact change (`preflight.SafeIssuerPatch`). It never prints a patch that sets the new issuer alone: the API
server would then refuse every token that carries the old audience, including the ones Omni and the
cluster's own parts hold. The change keeps the old issuer and both audiences (Talos `APIAudiences` is
`accepted.audiences` when set, else the issuer URL).

Talos and Omni, a cluster config patch (Omni refuses only `issuer.privateKey` in this document):

```yaml
apiVersion: v1alpha1
kind: KubeServiceAccountConfig
issuer:
  issuerURL: https://kubernetes.default.svc
accepted:
  issuers:
    - <old issuer>
  audiences:
    - https://kubernetes.default.svc
    - <old issuer>
```

kubeadm, the kube-apiserver flags (the first `--service-account-issuer` signs):

```text
--service-account-issuer=https://kubernetes.default.svc
--service-account-issuer=<old issuer>
--api-audiences=https://kubernetes.default.svc,<old issuer>
```

Read the audiences the cluster accepts now (`--api-audiences`, or `accepted.audiences`) first and keep every
existing entry in the list: the lists above show only the two issuers, and dropping another audience makes the
API server refuse the tokens that carry it.

Cost: one API server restart. Tokens issued before the change stay valid until they expire.

With the issuer `https://kubernetes.default.svc` the installer writes the `ate-api-authentication` ConfigMap
entry itself after `ate-setup` (`kubernetes` provider with the pod's `ca.crt` and `token`, as `kubernetesJWTProvider`
does) and restarts the api server when it changed it, so the result does not depend on how `ate-setup` ran.
This is interim: it goes when the pinned Substrate writes the entry for every install path
([#1108](https://github.com/stateracloud/koinon/issues/1108)).

When `ate-setup` fails, the installer keeps the pinned build and prints the exact `ate-setup delete ate-system`
command that removes the partial install.

## gVisor tarball in the cluster object store

Substrate ships the SandboxConfig `gvisor-default`. It points at `gs://gvisor/...`, so atelet reads the
gVisor tarball from the internet. Koinon keeps the Substrate namespaces closed to the internet (ADR 0120
section 6), so the installer puts the pinned tarball in the cluster object store and applies a SandboxConfig
that points there. Pods in the cluster then download the tarball with no purpose-made egress to the internet (the apiServerPorts rule of
ADR 0120 amendment 10 still opens TCP 443 and 6443 to any address for the control pods that call the API server). This is
step 8 of the install, on the `kind` and `generic` platforms when the install has the gvisor class. On `gke`
the `gs://` default stays: that route has no in-cluster object store.

What the installer does:

1. It reads the keys of the rustfs Deployment (a value, or a Secret reference) and opens a port-forward to the
   rustfs pod. The keys stay in the installer process. They are never written, printed or sent to a sandbox.
2. For each architecture (`amd64`, `arm64`) it reads the object
   `gvisor-assets/<arch>/<sha256><suffix>` in the snapshot bucket (`--storage-bucket`). The key has no `atespaces`
   segment, so atelet's object-store plugin serves it. It is the layout `ate-setup` uses for the microVM assets.
3. An object with the pinned SHA-256 is left as it is: a second run fetches and writes nothing.
   An object with another SHA-256 is an error. The installer never overwrites it. Delete the object if it is
   wrong, then run the installer again.
4. A missing object is fetched from the source, checked against the pinned SHA-256 before the first byte
   goes to the store, written, read back and checked again.
   The key and the SandboxConfig URL end in the same suffix: atelet chooses the decompressor from the suffix
   of the URL only. The installer finds the suffix from the magic number of the downloaded bytes (gzip
   `.tar.gz`, bzip2 `.tar.bz2`, zstd `.tar.zst`, plain tar `.tar`). Any other format is an error and nothing is
   written. A re-run looks for the pinned digest under each of the four suffixes. An
   object from an older installer, without the suffix, stays in the bucket. Nothing uses it after the next run. Delete it by hand.
5. It creates the SandboxConfig `koinon-gvisor` (class `gvisor`, annotation
   `sandboxconfig.ate.dev/is-class-default: "true"`, so it is newer than `gvisor-default` and wins). Its
   version `gvisor-<release>` has `s3://<bucket>/gvisor-assets/<arch>/<sha256><suffix>` and the same SHA-256 for each
   architecture. A new release adds a version and keeps the old ones, because a snapshot restores its memory only on
   the version it was taken with. A version that exists with other assets is an error, never a rewrite. The one exception is the version of an
   older installer whose URLs are exactly the new ones without the archive suffix. atelet refused it, so no
   snapshot depends on it. The installer replaces it in place.

Limits:

- Substrate picks the newest class default. If `ate-setup` is cleaned up and run again, `gvisor-default` is
  newer and wins. The installer then prints a warning. Delete SandboxConfig `koinon-gvisor` and run the installer
  again.
- On a cluster that already has snapshots on an older `gvisor-default` version, the pools move to
  `koinon-gvisor`, which has only the pinned release. Actors with snapshots on the older version cannot restore
  their memory. New installs are not affected.
- On the `kind` platform the bucket must exist. The platform creates only its own snapshot bucket, so a
  different `--storage-bucket` fails with a message that names the bucket.
- One tarball download has a limit of 20 minutes.

The pinned release and its checksums are in `hack/versions.env` (`GVISOR_VERSION`, `GVISOR_SHA256_AMD64`,
`GVISOR_SHA256_ARM64`) and in `internal/substrateinstall/gvisorpin.go`. A test compares the two. The
release is the one of version `v7` of `gvisor-default` in the pinned Substrate.

### Mirror and air-gapped installs

The installer host fetches the tarball, not the cluster. By default it reads
`https://storage.googleapis.com/gvisor/releases/release/<release>/<x86_64|aarch64>/gvisor.tar.zstd`. For a host without
internet, copy those two files to a server that the host reaches and pass its base URL:
`--gvisor-mirror https://mirror.example.com/gvisor`. The layout below the base is `<release>/<x86_64|aarch64>/gvisor.tar.zstd`.
The pinned SHA-256 protects the content, so a mirror needs no trust. A wrong file fails with nothing written.

To check an install: atelet logs name the asset with `s3://` and the pinned SHA-256, and no pod in the Substrate
namespaces reaches `storage.googleapis.com`.

Cost: a repeat run reads each object through the port-forward to compare its SHA-256 (about 60 MB for each architecture).

## Talos: user namespaces for gVisor

The gVisor class cannot start an agent on a node with `user.max_user_namespaces=0`. Talos sets that value
by default. gVisor (`runsc`) starts its sentry in a user namespace. On such a node the start fails with
`runsc create` exit 128 and `fork/exec .../gvisor-sentry-prewarmer: no space left on device`. The disk is
not full: the kernel returns ENOSPC because it allows no user namespace. The preflight check
"Substrate gVisor user namespaces" finds this before the first agent start. The vm tier does not need user
namespaces, and the check does not run without the gVisor class.

Talos, a machine config patch for the worker nodes (`userns.yaml`):

```yaml
machine:
  sysctls:
    user.max_user_namespaces: "11255"
```

```bash
talosctl --talosconfig <file> patch machineconfig --nodes <worker node addresses> --patch @userns.yaml
```

Omni: add the same `machine.sysctls` entry to a config patch of the worker machine set.

kubeadm and other Linux nodes:

```bash
sysctl -w user.max_user_namespaces=11255
echo 'user.max_user_namespaces = 11255' > /etc/sysctl.d/99-gvisor.conf
```

The value 11255 is the one that a Talos cluster confirmed (Kubernetes 1.37.1: with 0, every first resume failed;
with 11255 on the workers and no other change, an agent started, suspended and resumed with its state). Any
value above 0 lets the sentry start.

Cost: user namespaces widen the kernel attack surface for every pod on those nodes, because an unprivileged
process can then reach kernel code that otherwise needs privilege. Set the value on the worker nodes that run
gVisor sandboxes only, and leave the control plane nodes at 0. Use `--substrate-node-selector` so the check
reads the same nodes.

## The image cache

With `execution.driver: substrate` the chart installs a Zot pull-through registry (`execution.substrate.imageCache`,
on by default; ADR 0120 section 6, item 9). It is the one image source of atelet. Only the cache has a purpose-made route to the internet (the apiServerPorts rule, ADR 0120 amendment 10, is the exception),
and only to the upstreams in `imageCache.upstreams` (default `ghcr.io`, `docker.io`, `quay.io`) on port 443.

- **The rewrite.** The driver rewrites the registry of every actor image to
  `<release>-imagecache.<namespace>.svc:5000/<host>/...` and keeps the path, the tag and the digest. atelet checks
  the digest, so the cache cannot change the content of an image. atelet still checks the TLS certificate of the
  cache: see "CA trust of atelet" below.
- **The name is part of the template hash.** A new release name, namespace or port re-hashes every actor template.
  Choose them once.
- **TLS.** The ensure-secrets hook Job makes the Secret `<release>-imagecache-tls` (a new CA and a leaf, 10 years,
  no rotation yet), or you set `imageCache.tls.existingSecret` to a `kubernetes.io/tls` Secret with `ca.crt`.
- **CA trust of atelet (issue #1191).** Substrate has no CA option, so Koinon does it in two parts:
  - The installer (`koinon-api substrate install`, kind and generic routes) adds one kustomize patch to the atelet
    DaemonSet. It mounts the optional ConfigMap `koinon-imagecache-ca` of `ate-system` at
    `/etc/koinon/imagecache-ca` and sets `SSL_CERT_DIR` to that path and `/etc/ssl/certs` (the default directory, which the
    variable would otherwise replace). It adds no container and no image. Go adds the
    certificates of that directory to the system roots of the atelet image, so public registries and the other TLS
    trust of atelet keep working.
  - The hook Job `<release>-imagecache-ca` (`koinon-api imagecache-ca`, pre-install and pre-upgrade, after
    ensure-secrets) copies `ca.crt` of the TLS Secret into that ConfigMap. The Secret is the generated one or
    `imageCache.tls.existingSecret`. The ConfigMap holds the public CA only. The Job refuses a `ca.crt` that holds
    anything but certificates, and it never writes the private key.
  - The Job also puts a hash on the pod template of atelet (the CA) and of the cache Deployment (the certificate and
    the CA). A changed hash restarts the pod, so a new CA reaches atelet and a new certificate reaches the cache.
    A rotation takes effect on the next `helm upgrade`: change the Secret, then run `helm upgrade`. The hash does not
    cause a second restart after that. On the first install the cache Deployment does not exist yet, so the Job cannot
    stamp it. The cache starts with the current Secret, and the first `helm upgrade` adds the stamp. That upgrade
    restarts the cache pod once.
  - The pinned Zot image also reloads its server certificate when the files of the mounted Secret change, without a
    restart. The kubelet updates a Secret volume only when the mount has no `subPath`. The chart mounts it without
    `subPath`. The update can take one to two minutes. Because of this, a new leaf certificate from the same CA (for
    example from cert-manager through `imageCache.tls.existingSecret`) reaches the cache before the next
    `helm upgrade`.
  - **A CA change needs `helm upgrade` immediately.** Zot serves a leaf from the new CA after the Secret volume
    update, but atelet trusts only the CA in `koinon-imagecache-ca`. Only the hook Job writes that ConfigMap. Until
    `helm upgrade` runs, atelet refuses the certificate of the cache and every pull through the cache fails with
    `x509: certificate signed by unknown authority`. Run `helm upgrade` immediately after a CA change.
  - Every route gets the same patch. The installer adds it on the kind and generic routes through the overlay. On the
    gke route the installer does not know the overlay directory of the pinned tree, so after `ate-setup` it adds the
    same volume, mount and `SSL_CERT_DIR` to the live atelet DaemonSet (one definition in `internal/imagecacheca`,
    `EnsureTrust`). It runs on every route and does nothing where the patch is there. It adds no container and no image,
    so the admission policy is not weakened. The hook Job never adds the patch: it changes only the hash annotation of
    atelet. Every run of `koinon-api substrate install` checks the patch at its end and adds it when it is missing.
  - Admission: the Job is not an installer identity. The policy `<ns>-<release>-worker-objects` admits one UPDATE by the
    Job's ServiceAccount: the atelet DaemonSet in `ate-system`, where only the template annotation
    `koinon.dev/imagecache-ca-sha256` changes. Any other change, DaemonSet, namespace, create or delete is denied.
    The rule renders only with the image cache.
  - The installer must run before `helm install`: the Job fails, and names that step, when atelet does not exist or
    has no trust patch (a Substrate from before this change). The Job does not report success then.
  - The Job can create any ConfigMap in `ate-system` and patch the atelet DaemonSet (admission limits that patch to
    the one annotation). The RBAC is a hook object: Helm
    deletes it after the Job. `helm uninstall` leaves the ConfigMap `koinon-imagecache-ca`: delete it by hand.
  - **One Substrate install per cluster.** The ConfigMap name is fixed, because the installer does not know the release
    name and one Substrate install has one atelet. Two releases that use the cache on one cluster share the ConfigMap:
    do not do that. This limit is on purpose.
- **Air gap.** Set `imageCache.enabled=false`. Name your mirror with `imageMirrors` (a map from host to mirror
  prefix) or `imageRegistry`, and allow it with `workerNetworkPolicy.imageSource`. The chart refuses the cache and
  an operator source together.
- **Check.** `kubectl -n <namespace> get deploy <release>-imagecache`; the atelet logs name the cache host and show no
  `x509: certificate signed by unknown authority`; `kubectl -n ate-system get cm koinon-imagecache-ca` holds one
  certificate; the Zot logs show the sync of the image; no pod in `ate-system` reaches the internet except on the `apiServerPorts` (TCP 443 and 6443), which the pods of `apiServerApps` open to any address.

## Worker namespace and admission

Worker pods run in their own namespace, not in `ate-system` (ADR 0120 section 6). The chart creates it:
`execution.substrate.workerNamespace`, default `ate-workers`. The default WorkerPool
(`execution.substrate.workerPool`), its ServiceAccount and the worker NetworkPolicy live there. A
WorkerPool in another namespace needs no Substrate change (checked on kind with Substrate v1.0.1): the
controller applies the pool Deployment and a per-pool NetworkPolicy in the pool's namespace.

- The chart creates the default WorkerPool (issue #1089). It is on by default with `execution.driver: substrate`.
  `koinon-api substrate install` and ate-setup create no pool. `helm upgrade` changes the pool, and `helm uninstall`
  removes it. The default pool is `koinon-gvisor`: one replica of the pinned worker image
  `ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.1`. Each replica requests 500m CPU and 1Gi memory (the
  idle cost), with limits of 4 CPU and 8704Mi (8.5Gi). The pool needs one node that runs the gvisor class: the node
  scheduler reserves the requests, and the largest sandbox needs 4 CPU and 8.5Gi free on that node. Change the size
  with `workerPool.replicas` and `workerPool.resources` (see "Worker capacity" below). The pool carries the label
  `koinon.dev/runtime-class` with `openshell.runtimeClassName` (default `kata`): the reconciler gives this runtime class
  to every sandbox. Substrate copies the label to the workers, and the substrate driver puts each sandbox only on a
  worker with the runtime class of the sandbox. Map that class to a tier in `execution.substrate.runtimeClassTiers`.
  `defaultTier` does not apply to a sandbox that names a class, so with `defaultTier` only, every command in the
  sandbox fails ("has no isolation tier"). The chart refuses the tier `vm` on a pool that is not `microvm` (ADR 0075). Set `workerPool.sandboxClass: microvm` for a microVM pool
  (the image follows the class). Set `workerPool.enabled: false` to manage the pools yourself. The WorkerPool CRD must
  exist before `helm install`, so install Substrate first. Only the installer identities write a WorkerPool, so the
  identity that runs helm must be in `workerPolicy.installerIdentities` or `installerGroups` (for example a GitOps
  ServiceAccount), or set `workerPool.enabled: false`. The default image is the tag `v1.0.1`: the interim digest
  exemption (#1140) admits it, and the chart refuses the render when that exemption is gone. If you installed
  Substrate with `--image-repo`, set `workerPool.workerImage` or a registry mirror (`global.registryMirrors`), or the
  pool pulls from `ghcr.io`.
- **Worker capacity** (Substrate v1.0.1). Read this before you change the pool size.
  - One worker pod runs many sandboxes. Its pod **limits** are one capacity that all its sandboxes share. The worker
    keeps 64Mi of the memory limit for itself. The pod requests do not add capacity: they only tell the node
    scheduler what to reserve.
  - Each sandbox books its own limits from that capacity: the CPU and memory limits of the agent (or its requests, when
    it has no limits). A microVM sandbox also books 4Mi plus 8Mi for each GiB of its memory limit. A sandbox starts
    only on a worker with free capacity in every dimension.
  - There is no queue in Substrate and no autoscaling: `workerPool.replicas` is a fixed count. When no worker has
    room, Substrate refuses the sandbox ("no worker has room"). Koinon then shows the run as waiting for its sandbox,
    puts it back in the queue, and tries again. A run that waits longer than the sandbox wait cap of the org (1 hour
    by default) fails with the class `sandbox_unavailable`.
  - One worker must fit the largest sandbox that the sizing bounds permit: CPU limit at least
    `sandbox.resources.bounds.cpu.limit.max`, memory limit at least `sandbox.resources.bounds.memory.limit.max` plus
    64Mi (plus the microVM overhead for a microVM pool). The chart refuses the render when it does not. The check
    uses `workerPool.resources.limits`, or the `workerLimits` LimitRange default limits when `workerPool.resources`
    is null (per resource). A worker with no limit reports the allocatable of its node, which other pods share, so
    its sandboxes could overbook the node: the chart also refuses a pool with no limit. A microVM pool needs a memory
    limit of at least 8324Mi with the default bounds (8Gi + 64Mi + 4Mi + 8 × 8Mi).
  - To size for N agents that run at the same time, add up their limits. With the default agent (2 CPU, 2Gi), N
    agents need 2N CPU and 2N Gi in total over all replicas, and each replica also needs 64Mi for the worker. In a
    microVM pool, each default agent also books 20Mi (4Mi + 2 × 8Mi). For
    example, 8 default agents fit on 2 replicas with limits of 8 CPU and 8256Mi each (4 agents on each), or on 4
    replicas with the default limits of 4 CPU and 8704Mi (2 agents on each). With the default bounds, a replica
    cannot be smaller than 4 CPU and 8256Mi. Every replica adds its requests to the node cost and to the
    `workerLimits` quota.
- The chart sets resource limits in the worker namespace (`execution.substrate.workerLimits`, on by default). A
  LimitRange gives a container with no requests or limits the defaults (500m CPU and 1Gi memory as requests, 8 CPU
  and 16Gi as limits). A ResourceQuota caps the namespace at 20 pods, 64 CPU and 128Gi of requests, and 128 CPU and
  256Gi of limits. A pod over the quota is refused. These defaults suit a small pool. A worker pod hosts many
  sandboxes, so for a real pool set `workerPool.resources` with both requests and limits (a pod with
  requests above the default limit and no limit is refused), and raise `workerLimits.quota.hard`. The quota also
  counts the old pod during a rollout, so keep room for the replicas plus the surge. To remove a key, set it to
  `null`: an empty map keeps the defaults.
- A native ValidatingAdmissionPolicy (`<namespace>-<fullname>-worker-limits`) guards every LimitRange and
  ResourceQuota write in the worker namespace when `workerPolicy` and `workerLimits` are on. Only the installer identities
  (`workerPolicy.installerIdentities` or `installerGroups`) create, change or delete them. The Substrate controller
  writes none. Kubernetes' own controllers may delete them and change their metadata, not their spec. The quota
  controller writes only the quota status, which the rule does not match. If a GitOps tool applies the chart, add its
  identity to the installer values, or it cannot change the limits.
- The pool names the chart's worker ServiceAccount (`automountServiceAccountToken: false`) unless the values name one. The
  controller's own `ateworker` ServiceAccount leaves the field unset, and the admission rule refuses a
  projected token, so a pool on `ateworker` does not start. If you name your own ServiceAccount, create it in
  the worker namespace and set the field false.
- `ate-system` keeps the control plane and atelet. Only the atelet pod (DaemonSet, ServiceAccount `atelet`)
  may mount a hostPath there. Set `workerPolicy.nodeAgent.allowedHostPaths` to what your atelet needs.
- The control-plane NetworkPolicy selects pods by an explicit list of `app` labels
  (`workerNetworkPolicy.controlPlaneApps`). Confirm the list against your Substrate manifests.
- Each actor gets one Substrate `EgressPolicy` (issue #1444). The driver writes it while the actor is suspended,
  before each create, restore, start and resume: one `http` rule for each gateway host. Every actor gets the ports
  of `--gateway-url` and `--gateway-mcp-url`. Only an actor whose agent has a code-host grant also gets the port of
  `--gateway-egress-url`. When the grant changes, the reconciler changes the policy in place on its next pass, and
  the sandbox is not replaced. There is no other rule, so an actor reaches the gateway and nothing else. The gateway
  URLs must be `http://` URLs with a DNS name (no IP address), and the egress listener must not be on the host and
  port of another gateway URL: the reconciler refuses to start otherwise. `DeleteActor` also deletes the policy.
  The chart NetworkPolicy of the atenet-egress pods is the second fence.
- The control-plane namespace has one default-deny egress policy for every pod, and per-app policies allow what each
  pod needs. So no pod of `ate-system` is unfenced. `ate-idp-server`, `postgres` and `rustfs` call nothing and have
  no egress. `apiOnlyApps` (`k8s-credential-provider`) get DNS and the API server ports only (to any address). The `rustfs-bucket-init`
  Job pod has no `app` label, so `bucketInitJobs` selects it by Job name and it gets DNS and the object store S3
  port only (`objectStoreApp`, `objectStorePort`). A pod that a later Substrate version adds has no egress (not even the apiServerPorts rule) until a
  list names it. Their ingress stays Substrate's own policies (`ate-system-networkpolicies.yaml`,
  `kind/rustfs.yaml`), because NetworkPolicies add up. Confirm the lists against your Substrate manifests.
- The worker container may have extra privileges in one exact shape, and only in the worker namespace. Substrate
  sets it in `workerpool_apply.go` (lines 377-468 at the pinned commit): user and group 0, not privileged, no
  privilege escalation, drop `ALL`, add the capabilities of the sandbox class, and seccomp and AppArmor
  `Unconfined`. The policy accepts this shape only on the main container named `worker` whose image is in
  `workerPolicy.workerShapes.gvisor.images` or `workerPolicy.workerShapes.microvm.images`. The list of the image
  sets the class: a gVisor image gets only the gVisor capabilities, a micro-VM image only the micro-VM ones. One
  image is in one list only, or the render fails. The pool image (`workerPool.workerImage`, or the pinned worker
  image of the class when it is empty) is added to the list of `workerPool.sandboxClass` when the pool is enabled,
  so with the default pool the gvisor list is not empty. An init container named `worker` is never the worker
  container: Kubernetes names are unique across main and init containers, so the pod then has no worker container.
  Every other container adds no capability and uses no `Unconfined` profile. There is no capability ceiling: the class sets
  (`workerPolicy.workerShapes`) are exact. A Ginkgo spec in `internal/preflight` compares them with a vendored copy
  of the Substrate source and fails when a Substrate bump changes the shape. Update the vendored copy and the sets
  together in that bump. With an empty image list the policy denies every worker container (fail closed).
- atelet pulls actor images from one image source, such as a pull-through cache. Set `workerNetworkPolicy.imageSource`
  to a `podSelector` (with a `namespaceSelector`) for a cache in the cluster, or to `cidrs` for a cache outside it,
  and set `port`. Set one of them: the render fails when you set both, and with neither atelet has no image egress
  (except the `apiServerPorts` rule, TCP 443 and 6443 to any address, ADR 0120 amendment 10).
  Actor image references must go through `global.registryMirrors` or `global.imageRegistry` (the digest is kept).
  Choose the mirror name once: it is part of the actor template hash, so a change re-hashes every template.
  Needs main: the driver side of the actor image rewrite is not in this chart.
- The image source must be an RFC 1918 IP address (10/8, 172.16/12, 192.168/16) or a registry with TLS. A plain-HTTP registry that you
  name by host name, or at an address in another range (for example 100.64/10 or IPv6 ULA), fails in atelet with "server gave HTTP response to HTTPS client". So set `imageSource.cidrs` to the private address and
  serve the registry on it, or put TLS on the registry and name it. An in-cluster cache by `podSelector` follows the
  same rule: its address in the actor image references must be a private IP or a TLS name. Issue #1170 adds a default
  in-cluster pull-through cache on this setting.
- The two other namespaces of the Substrate setup have a NetworkPolicy fence, from the same switch
  (`workerNetworkPolicy.enabled`). `podcertificate-controller-system` has default deny for ingress and egress, and the
  controller reaches the Kubernetes API server ports (`apiServerPorts`, to any address) and nothing else
  (`workerNetworkPolicy.podcertSystem`). `otel-system` has default deny and one policy per app: the collector takes
  OTLP (4317, 4318) from `ate-system` and its read ports (8889, 8888) from Prometheus, and sends traces to Jaeger;
  Jaeger takes OTLP from the collector only; Prometheus reaches DNS, the API server, the collector and the metrics
  ports in `scrapePorts` (`workerNetworkPolicy.otelSystem`). The Prometheus and Jaeger UI ports have no ingress rule:
  use `kubectl port-forward`. Set `otelSystem.enabled=false` when your cluster has no `otel-system` namespace,
  because a NetworkPolicy in a missing namespace fails the install. Only Substrate's kind overlay makes `otel-system`:
  on the GKE route set `otelSystem.enabled=false` and point `telemetry` at `gke-managed-otel` and its collector pods.
- atelet and the other control-plane pods export OTLP to the collector in `otel-system`. The default-deny egress
  policy of `ate-system` blocked that route, and atelet logged "OpenTelemetry SDK error ... deadline exceeded".
  `workerNetworkPolicy.telemetry` opens the OTLP ports of the collector pods to the `controlPlaneApps` and nothing
  else. Worker pods also export OTLP, and their egress stays atenet-egress and DNS only (ADR 0120 section 2.4), so their
  export stays blocked: a known gap. Set `telemetry.namespace` and `telemetry.podSelector` for another collector.
- In `ate-system`, `pods/exec`, `attach` and `portforward` are allowed for the identities in
  `workerPolicy.installerIdentities` and `installerGroups` only, and each request gets the audit annotation `connect` (user, resource,
  subresource, pod). The worker namespace stays fully denied, and the proxy subresources stay denied for everyone.
  The audit annotation is written for allowed requests too. The binding also lists `Audit`, which records a refused request.
- `workerNetworkPolicy.apiServerPorts` (default 443 and 6443) lists the TCP ports that the fenced control pods open to
  the Kubernetes API server. The rule names the ports and no address, because NetworkPolicy has no peer for the API
  server that works on every CNI (ADR 0120 amendment 10, issue #1172). You do not look up an address. Check the API
  server port of your cluster (`kubectl get endpoints kubernetes -n default`) and add it when it is not 443 or 6443.
  Only the pods of `workerNetworkPolicy.apiServerApps` (default `atelet`, `ate-controller`, `ate-api-server`, `atenet-router`), the
  `apiOnlyApps`, the podcert controller and the otel Prometheus get the rule. `atenet-router` needs it (its EndpointSlice watch). `atenet-egress` does
  not use the API server and gets no route, so the pod that carries worker traffic out has no TCP route to any address.
  The cost: a compromised control pod can open these ports to any server. This includes atenet-router, which handles
  actor traffic (its `--mode=ingress` is the `routerAddress` of the driver), and atelet: a TLS registry on
  port 443 is reachable at L3, and only the image rewrite and the `SandboxConfig` source limit atelet to the one image
  source. Worker pods get no such rule.
  `test/e2e/kind/apiserver-egress.sh` (`make test-fence-kind`) proves the rule on kind. The `e2e-kind` workflow runs it after the quickstart.
- atelet needs no purpose-made internet egress (it still has the TCP 443 API server rule above) when the gVisor tarball comes from the cluster object store. The installer
  stages it (see [gVisor tarball in the cluster object store](#gvisor-tarball-in-the-cluster-object-store),
  issue #1157). The control-plane NetworkPolicy allows traffic inside `ate-system`, so atelet reaches rustfs.
  The kind check must prove that atelet reads the `s3://` SandboxConfig `koinon-gvisor` with egress denied. After
  the policies apply, restart every DaemonSet in `ate-system`, so that its pods pass the new rules.
- The installer writes the SandboxConfig `koinon-gvisor`. It must run as an identity in
  `workerPolicy.installerIdentities`, or the objects policy refuses the write.
- The preflight and install probe pods are bare pods, and the device probe has hostPath devices. The policies refuse
  them in `ate-system` and in the worker namespace, and they have no exception for them. So the probes run in the
  probe namespace (`--substrate-probe-namespace`, issue #1176). An escape spec proves that the probe pods are still
  refused in both Substrate namespaces.
- The node agent rule accepts the per-controller `daemon-set-controller` ServiceAccount only. A cluster that runs
  `kube-controller-manager` without `--use-service-account-credentials` must set `workerPolicy.podCreators` and
  adjust the node agent rule to match.
- `kubectl ate proxy` needs the router flag `--ingress-allowed-spiffe-ids`. If Koinon's gateway path needs
  the same, add the gateway's SPIFFE ID to that list.

## Snapshot bucket: encryption, credential, checksums

Substrate stores the snapshot of each sandbox in the snapshot bucket (`--storage-bucket`). Substrate does not
encrypt these objects. Per-org encryption is planned for v0.2 (ADR 0120 section 5.3). In v0.1 there is no
install check for any point in this section. The operator must do each point.

1. **Turn on server-side encryption (SSE) for the bucket.** Without it, the plain text of every file in a
   home, also a file with mode 0600, is readable in the snapshot objects. The lab bucket had no SSE, and the
   objects `fs/pages.img.zstd` (gVisor), `durable-dir-home.tar.zstd` and `memory-ranges.zstd` (vm tier) held
   plain data. Set the default encryption of the bucket before the first sandbox starts:
   - AWS S3: S3 applies `SSE-S3` to new objects by default. To use `SSE-KMS`, set the default encryption of
     the bucket (`aws s3api put-bucket-encryption`).
   - Google Cloud Storage: a default customer-managed key on the bucket (`gcloud storage buckets update
     --default-encryption-key`). Google encrypts at rest with its own key when you set none.
   - MinIO: bucket encryption with a KMS key (`mc encrypt set`).
   - rustfs (the bundled store): put the `rustfs-data` volume on an encrypted StorageClass or an encrypted
     disk. Also check the rustfs documentation for server-side encryption; this page does not verify it.

   Encryption at rest protects the stored object against loss of a disk or a bucket export. It does not
   protect against a holder of the bucket credential: the store decrypts for that holder (point 2).
2. **Limit who holds the bucket credential.** The credential reads every home, including 0600 files, and
   the root key can also write and delete every snapshot. Give the Koinon reconciler its own read-only key
   (see [What `generic` corrects](#what-generic-corrects)). Keep the root key out of CI, out of sandboxes and
   out of the Koinon namespace. Limit who can read the Secret `koinon-rustfs-credentials` in the Substrate
   namespace.
3. **Substrate has no snapshot checksums.** Substrate does not check a snapshot when it restores it. A
   damaged or changed object in the bucket is not found. Use the object versioning of your store when you need
   to find a change. Do not use object lock on the bucket: it can block the deletes that Substrate makes.
   The snapshot store is therefore trusted. Write access to it gives control of the suspended agents.
4. **Size the snapshot store.** The default is 10Gi (`--rustfs-storage-size`). That option sizes only the
   bundled rustfs store. For an external bucket, set the size or the quota in that store. Calculate the sum
   of the memory in use of all agents that can be suspended at one time, plus the home size of each of these
   agents. Use the memory limit of each agent as the worst case. Each suspend replaces the old snapshot of
   that agent, and deleting the agent empties its prefix. A snapshot is about the size of the touched memory
   that does not compress, plus the home. It is not the limit. A tag adds one more full copy, but Koinon
   makes no tag in v0.1. Lab values (Substrate v1.0.0, gVisor class, issue #1090): idle 3.4 MB; 0.5 GiB in
   use, 541 MB; 1 GiB in use, 1.08 GB; 4 GiB in use, about 4.2 GB. The case of a full store is not tested
   yet. Until it is, make the store larger than the sum from this rule, and keep free space in it.

## Substrate database: not in the Koinon backup

Substrate keeps its own state in its own database: the StatefulSet `ate-system/postgres`, database
`atepg`. The Koinon CNPG backup does not include this database. The Koinon backup restores homes only from
kopia, not from the snapshots in the Substrate bucket.

- To keep the Substrate state, the operator must back up `atepg` and the snapshot bucket, and restore
  them together. A restore of one without the other gives a mismatch between the actors and their
  snapshots. Use `pg_dump` of `atepg` or a volume snapshot of the `postgres` claim.
- Without such a backup, a lost Substrate database loses the actors and their snapshots. Koinon can still
  restore the homes from kopia and create new sandboxes.

### Storage of the Substrate database

Put the Substrate PostgreSQL on storage that does not share a disk with the snapshot store. Substrate ends
an actor lease when one PostgreSQL commit stalls for more than about 10 s. The lease renews every 10 s and
has a fixed deadline of 20 s (`lease.go`). A suspend that loses its lease fails after 20.9 s with a
`Canceled` error and writes no log line. On koinon-lab, the snapshot upload loaded the same disk as the
PostgreSQL write-ahead log. Commits stalled for 8 to 9.1 s, and the margin was 0.4 s. Use `--postgres-storage-class`
for a class that maps to a different disk than the class of `rustfs-data` (`--rustfs-storage-class`). A
class name alone does not prove this: for example, `local-path` can place both claims on one node disk.
See [Options](#options).

## Verified and unverified

Verified against the Substrate v1.0.1 source (commit `20778a6d`), its `ate-setup --help` and
the koinon-lab install notes: the CRD names and the `v1` version, the Deployment names `ate-api-server`
and `atenet-router`, the router flag `--ingress-allowed-spiffe-ids`, the SPIFFE ID
`ns/<namespace>/sa/atenet-client`, the WorkerPool status (`replicas`, `readyReplicas`, a `Resolved`
condition per sandbox class), the `ate-setup` command `deploy ate-system` and the flags used here, and
that `internal/version` takes its version and commit from `-ldflags -X`, and that the release has no `ate-setup` asset (only `kubectl-ate` with `.sha256` files).

### Unverified

On a lab cluster whose issuer is an IPv6 address pods cannot reach, the issuer probe failed with the message
above and the installer installed nothing. Everything after that check is not verified on a cluster; the names
above come from the source:

- `--namespace` other than `ate-system`: ate-setup has the flag, but the lab found that `deploy ate-system`
  names three namespaces literally. The SPIFFE ID and the checks follow the configured namespace; an
  install into another one is untested.
- An S3-compatible object store and an external Postgres: `--storage-bucket-name` is the only storage flag
  of `ate-setup`. The bundled Postgres is the default; an external one is set by environment
  (`ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING`), which this step does not pass yet.
- That `--credential-provider='{"name":"k8s.io"}'` is the provider Koinon wants.
- A full install with the new options (`--postgres-storage-class` and the others) and the in-cluster issuer
  probe on a cluster; the lab run before them stopped at the issuer probe.
- The chart's preflight hook with the Substrate driver on a real cluster (see "The chart's preflight hook").

### Known gaps

- The `generic` platform is verified against the pinned source and a rendered kustomization of the kind
  overlay (no `rustfsadmin`, the Koinon bucket, sizes and classes, `info` log level). A full install on a
  Talos cluster with a default StorageClass is not yet verified.
- microVM asset staging from inside the cluster (the vm tier on `generic`) is not done.
