# Substrate install step and preflight

Agent Substrate is a pinned cluster prerequisite, not part of the chart
([ADR 0119](../adr/0119-agent-substrate-execution-layer.md) D4). Run the installer step before
`helm install` with `execution.driver: substrate`:

```sh
koinon-api substrate install --ate-setup ./ate-setup --probe-image <koinon-api image> --namespace <release namespace> \
  --object-store-endpoint ... --object-store-bucket ... --object-store-secret ... --database-secret ... \
  --substrate-worker-pool vm=<WorkerPool name>
```

The step checks the prerequisites first and stops before it installs anything when one fails.
Then it runs the pinned `ate-setup`, waits for the CRDs to be Established (applying a `SandboxConfig`
right after the CRDs raced in the lab), lets the router accept
`spiffe://cluster.local/ns/ate-system/sa/atenet-client`, and runs the full preflight.
`koinon-api preflight --driver substrate` runs only the checks.

## What the preflight checks

- Kubernetes 1.37 or later (D8).
- With the vm tier: one node mounts `/dev/kvm`, `/dev/net/tun`, `/dev/vhost-vsock` and `/dev/vhost-net`
  (a probe pod with `CharDevice` host paths; only the node it lands on is checked, so use
  `--substrate-node-selector` to aim it at workers).
- The service-account issuer answers from a pod in the Substrate namespace.
- The Substrate CRDs, a ready api server of release v1.x, the router allow-list, and a ready WorkerPool per tier.

## Service-account issuer not reachable

Substrate v1.0.0 fetches the OIDC discovery document and the JWKS from the issuer URL in the
service-account tokens and has no override. On Talos the issuer is an IPv6 unique-local address that
pods cannot route to, and every call then fails with "invalid bearer token". Supported fixes:

1. Set the API server's `--service-account-issuer` (and `--service-account-jwks-uri`) to a URL that
   pods reach. On Talos, set `cluster.apiServer.extraArgs`.
2. Wait for an upstream discovery-URL override in Substrate (not yet requested; a human posts it).

A sidecar that serves a copy of the discovery document on the issuer address is not a supported install.
