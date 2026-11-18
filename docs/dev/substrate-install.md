<!-- SPDX-License-Identifier: Apache-2.0 -->

# Substrate install step and preflight

Agent Substrate is a pinned cluster prerequisite, not part of the chart
([ADR 0119](../adr/0119-agent-substrate-execution-layer.md) D4). Run the installer step before
`helm install` with `execution.driver: substrate`. The release namespace may not exist at that time:
the installer step creates nothing there.

Substrate releases ship no `ate-setup` binary, and `ate-setup` applies the manifests of a source
checkout. So you need a clean checkout of the release tag and an `ate-setup` built from it:

```sh
git clone --branch v1.0.0 https://github.com/agent-substrate/substrate && cd substrate
go build -o /usr/local/bin/ate-setup ./cmd/ate-setup
koinon-api substrate install --ate-setup /usr/local/bin/ate-setup --substrate-source . \
  --probe-image <koinon-api image> --storage-bucket <bucket> \
  --substrate-worker-pool vm=<WorkerPool name>
```

The pin is in one place: `internal/preflight/substratepin.go` (version and commit of the release tag).
The step refuses to run when `ate-setup --version` does not report that version and commit, or when the
checkout is not at that commit or has local changes. After the install, the preflight accepts only an api
server image with exactly the pinned tag.

Order of the step:

1. Check the pin (above).
2. Create the Substrate namespace (Pod Security `privileged`) when it is missing: the probe pods run there.
3. Run the prerequisite preflight and stop when one check fails.
4. Run `ate-setup deploy ate-system` with pre-built release images of the pinned tag.
5. Wait for the CRDs to be Established (applying a `SandboxConfig` right after the CRDs raced in the lab).
6. Make sure the router allows `spiffe://cluster.local/ns/<substrate namespace>/sa/atenet-client`.
7. Run the full preflight, without the NetworkPolicy probe. That probe starts two pods in the release
   namespace, which may not exist yet. The chart's own preflight runs it after `helm install`.

`koinon-api preflight --driver substrate` runs only the checks.

## What the preflight checks

- Kubernetes 1.37 or later (D8).
- With the vm tier: one node mounts `/dev/kvm`, `/dev/net/tun`, `/dev/vhost-vsock` and `/dev/vhost-net`
  (a probe pod with `CharDevice` host paths; only the node it lands on is checked, so use
  `--substrate-node-selector` to aim it at workers).
- The service-account issuer answers from a pod in the Substrate namespace, with a certificate that
  pod trusts. An issuer that answers only without TLS verification fails: Substrate rejects the tokens.
- The Substrate CRDs (`workerpools.ate.dev`, `sandboxconfigs.ate.dev`, `csidriverconfigs.ate.dev`),
  a ready `ate-api-server` of exactly the pinned release, the `atenet-router` allow-list, and per tier a
  WorkerPool with all its workers ready (`status.readyReplicas`) and every sandbox class resolved.

## Service-account issuer not reachable

Substrate v1.0.0 fetches the OIDC discovery document and the JWKS from the issuer URL in the
service-account tokens. `ate-setup` has `--ateapi-expected-jwt-issuer`, but it sets the issuer the api
server trusts, not where it fetches from. On Talos the issuer is an IPv6 unique-local address that pods
cannot route to, and every call then fails with "invalid bearer token". Supported fixes:

1. Set the API server's `--service-account-issuer` (and `--service-account-jwks-uri`) to a URL that
   pods reach. On Talos, set `cluster.apiServer.extraArgs`.
2. Wait for an upstream discovery-URL override in Substrate (not yet requested; a human posts it).

A sidecar that serves a copy of the discovery document on the issuer address is not a supported install.

## Verified and unverified

Verified against the Substrate v1.0.0 source (commit `b34fbee7`), its `ate-setup --help` and
the koinon-lab install notes: the CRD names and the `v1` version, the Deployment names `ate-api-server`
and `atenet-router`, the router flag `--ingress-allowed-spiffe-ids`, the SPIFFE ID
`ns/<namespace>/sa/atenet-client`, the WorkerPool status (`replicas`, `readyReplicas`, a `Resolved`
condition per sandbox class), the `ate-setup` command `deploy ate-system` and the flags used here, and
that the release has no `ate-setup` asset (only `kubectl-ate` with `.sha256` files).

### Unverified

Not checked against a running cluster in this change (no koinon-lab kubeconfig file exists; labrun mints
credentials from OpenBao, and that was not done here). The names above come from the source:

- `--namespace` other than `ate-system`: ate-setup has the flag, but the lab found that `deploy ate-system`
  names three namespaces literally. The SPIFFE ID and the checks follow the configured namespace; an
  install into another one is untested.
- The release image path `ghcr.io/agent-substrate/substrate` and that it holds every image under tag
  `v1.0.0` (the Substrate docs say releases publish there; nothing was pulled).
- An S3-compatible object store and an external Postgres: `--storage-bucket-name` is the only storage flag
  of `ate-setup`. The bundled Postgres is the default; an external one is set by environment
  (`ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING`), which this step does not pass yet.
- That `--credential-provider='{"name":"k8s.io"}'` is the provider Koinon wants.
- The `ate-setup --version` output format beyond the local darwin build of the lab.
- The RBAC of the preflight ServiceAccount (issuer discovery, `kube-root-ca.crt`): chart work.
