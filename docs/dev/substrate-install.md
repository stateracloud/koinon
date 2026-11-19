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

`ate-setup` v1.0.0 has two install routes: GKE (GCS for snapshots, a Google service account, the GKE
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
get pinned in `substratepin.go`. The images live at `ghcr.io/agent-substrate/substrate/<component>:v1.0.0`,
named by component (`ateapi` for the container `ate-api-server`), not by Deployment. The preflight reads the
tag of the image of that container and never the image name.

Order of the step:

1. Build and check `ate-setup` (above).
2. Run the prerequisite checks that need no namespace: the Kubernetes version and whether the service-account
   issuer can be read. When one fails, nothing was created.
3. Create the Substrate namespace (Pod Security `enforce`, `warn` and `audit` all `privileged`) when it is missing. The probe pods run there.
4. Run the probe checks (node devices, issuer reachability). Before each probe pod starts, the prober
   deletes stale probe pods that an earlier run left (same `component` and `part-of=koinon` labels). When a
   check fails and the installer created the namespace, the installer deletes the namespace again. A
   namespace that existed before is left as it is.
5. Run `ate-setup deploy ate-system` with pre-built release images of the pinned tag.
6. Wait for the CRDs to be Established (applying a `SandboxConfig` right after the CRDs raced in the lab).
7. Make sure the router allows `spiffe://cluster.local/ns/<substrate namespace>/sa/atenet-client`. The
   installer finds the router container by name (`atenet-router`), reads the flag in its command and
   arguments, and retries when the Deployment changed during the update.
8. Stage the gVisor tarball (kind and generic platforms, gvisor class). See "gVisor tarball in the cluster
   object store" below.
9. Run the full preflight, without the NetworkPolicy probe. That probe starts two pods in the release
   namespace, which may not exist yet. The chart's own preflight runs it after `helm install`.

The probe pods in the Substrate namespace use no image pull secrets: `--probe-image-pull-secrets` names
Secrets of the release namespace, which do not exist there. How the Substrate namespace pulls the probe
image is part of S2 (chart work).

`koinon-api preflight --driver substrate` runs only the checks.

### Permissions of the installer

The kubeconfig of the installer needs these rules:

- `namespaces`: get, create, delete
- `nodes`: list (platform detection, the kind check and the gVisor user-namespace probe)
- `storageclasses` (group `storage.k8s.io`): list (the `generic` storage preflight)
- `secrets` in the Substrate namespace: get, create (the `generic` object-store credentials)
- `pods`: create, get, delete, list in the Substrate namespace; `pods/log`: get
- `events`: list (the probe reads the kubelet event of a pod that did not start)
- `customresourcedefinitions`: get
- `deployments`: get, update in the Substrate namespace
- `workerpools` (group `ate.dev`): get
- non-resource URL `/.well-known/openid-configuration`: get

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
- Substrate's api server can validate service-account tokens from a pod in its namespace. See below.
- The Substrate CRDs (`workerpools.ate.dev`, `sandboxconfigs.ate.dev`, `csidriverconfigs.ate.dev`),
  a ready `ate-api-server` of exactly the pinned release, the `atenet-router` allow-list, and per tier a
  WorkerPool with all its workers ready (`status.readyReplicas`) and every sandbox class resolved.

## Service-account issuer

The probe repeats what Substrate v1.0.0's api server does to validate a token, from a pod with the default
ServiceAccount of the Substrate namespace (source at commit `b34fbee7`):

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

The real requirement: pods in the Substrate namespace reach the issuer Substrate trusts, because Substrate v1.0.0
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
that points there. Pods in the cluster then download the tarball with no egress to the internet. This is
step 8 of the install, on the `kind` and `generic` platforms when the install has the gvisor class. On `gke`
the `gs://` default stays: that route has no in-cluster object store.

What the installer does:

1. It reads the keys of the rustfs Deployment (a value, or a Secret reference) and opens a port-forward to the
   rustfs pod. The keys stay in the installer process. They are never written, printed or sent to a sandbox.
2. For each architecture (`amd64`, `arm64`) it reads the object
   `gvisor-assets/<arch>/<sha256>.tar.zst` in the snapshot bucket (`--storage-bucket`). The key has no `atespaces`
   segment, so atelet's object-store plugin serves it. It is the layout `ate-setup` uses for the microVM assets.
3. An object with the pinned SHA-256 is left as it is: a second run fetches and writes nothing.
   An object with another SHA-256 is an error. The installer never overwrites it. Delete the object if it is
   wrong, then run the installer again.
4. A missing object is fetched from the source, checked against the pinned SHA-256 before the first byte
   goes to the store, written, read back and checked again.
   The key and the SandboxConfig URL end in `.tar.zst`: atelet chooses the archive format from that suffix. An
   object from an older installer, without the suffix, stays in the bucket. Nothing uses it. Delete it by hand.
5. It creates the SandboxConfig `koinon-gvisor` (class `gvisor`, annotation
   `sandboxconfig.ate.dev/is-class-default: "true"`, so it is newer than `gvisor-default` and wins). Its
   version `gvisor-<release>` has `s3://<bucket>/gvisor-assets/<arch>/<sha256>.tar.zst` and the same SHA-256 for each
   architecture. A new release adds a version and keeps the old ones, because a snapshot restores its memory only on
   the version it was taken with. A version that exists with other assets is an error, never a rewrite.

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

## Verified and unverified

Verified against the Substrate v1.0.0 source (commit `b34fbee7`), its `ate-setup --help` and
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
- The RBAC of the preflight ServiceAccount (issuer discovery, `kube-root-ca.crt`): chart work.

### Known gaps

- The `generic` platform is verified against the pinned source and a rendered kustomization of the kind
  overlay (no `rustfsadmin`, the Koinon bucket, sizes and classes, `info` log level). A full install on a
  Talos cluster with a default StorageClass is not yet verified.
- microVM asset staging from inside the cluster (the vm tier on `generic`) is not done.
