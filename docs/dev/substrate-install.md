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
  --substrate-worker-pool vm=<WorkerPool name>
```

Options for a real cluster, passed to `ate-setup` under the names the pinned `ate-setup` defines:

| `koinon-api substrate install` | `ate-setup` | When you need it |
| --- | --- | --- |
| `--postgres-storage-class <name>` | `--ateapi-postgres-storage-class` | The cluster has no default StorageClass: the bundled Postgres claim stays Pending without it. |
| `--setup-csi nfs\|hostpath\|both\|none` | `--setup-csi` | A CSI driver should be installed with Substrate (default `none`; `hostpath` is for kind only). |
| `--expected-jwt-issuer <url>` | `--ateapi-expected-jwt-issuer` | See [Service-account issuer](#service-account-issuer). |
| `--kind` | `--kind` | The cluster is a local kind cluster. |

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
8. Run the full preflight, without the NetworkPolicy probe. That probe starts two pods in the release
   namespace, which may not exist yet. The chart's own preflight runs it after `helm install`.

The probe pods in the Substrate namespace use no image pull secrets: `--probe-image-pull-secrets` names
Secrets of the release namespace, which do not exist there. How the Substrate namespace pulls the probe
image is part of S2 (chart work).

`koinon-api preflight --driver substrate` runs only the checks.

### Permissions of the installer

The kubeconfig of the installer needs these rules:

- `namespaces`: get, create, delete
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

1. The tokens already carry `https://kubernetes.default.svc` (the API server lists it first in
   `--service-account-issuer`): pass `--expected-jwt-issuer https://kubernetes.default.svc`. Substrate then
   reads discovery from the in-cluster API server with the pod's token. Pods need no route to anything else.
2. Otherwise set the API server's first `--service-account-issuer` to `https://kubernetes.default.svc`, or to a
   URL that pods reach with a certificate from the system roots. On Talos, set `cluster.apiServer.extraArgs`.
   Later `--service-account-issuer` values are only accepted for validation, not used to sign.

The API server serves the discovery document and the JWKS to every service account through the
`system:service-account-issuer-discovery` ClusterRoleBinding. A sidecar that serves a copy of the discovery
document on the issuer address is not a supported install.

## Verified and unverified

Verified against the Substrate v1.0.0 source (commit `b34fbee7`), its `ate-setup --help` and
the koinon-lab install notes: the CRD names and the `v1` version, the Deployment names `ate-api-server`
and `atenet-router`, the router flag `--ingress-allowed-spiffe-ids`, the SPIFFE ID
`ns/<namespace>/sa/atenet-client`, the WorkerPool status (`replicas`, `readyReplicas`, a `Resolved`
condition per sandbox class), the `ate-setup` command `deploy ate-system` and the flags used here, and
that `internal/version` takes its version and commit from `-ldflags -X`, and that the release has no `ate-setup` asset (only `kubectl-ate` with `.sha256` files).

### Unverified

Not checked against a running cluster in this change (no koinon-lab kubeconfig file exists; labrun mints
credentials from OpenBao, and that was not done here). The names above come from the source:

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
