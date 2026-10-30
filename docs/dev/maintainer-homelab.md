# Dev registry

Tilt pushes images to the home lab's Zot registry (ADR 0047). Zot is part of the home-lab platform
(Argo CD), not installed by Koinon.

- URL: `zot.apps.lab.example.invalid`, Koinon images under `koinon/`
- Zot v2.1.22 in namespace `zot`. It is also the cluster's pull-through cache: anonymous reads
  work only for mirrored upstreams (docker.io, ghcr.io, quay.io, registry.k8s.io), so `koinon/*`
  needs credentials for both push and pull.

## One-time setup

The credentials live in the macOS login keychain. These commands never print them.

```sh
# On your Mac: ko and Tilt push with this login
security find-generic-password -s zot-kionon -w |
  docker login zot.apps.lab.example.invalid -u procoduck --password-stdin

# In the cluster: the pull secret the dev manifests reference.
# Prefer a read-only Zot account here over your push credentials.
kubectl --context homelab create namespace koinon --dry-run=client -o yaml |
  kubectl --context homelab apply -f -
kubectl --context homelab -n koinon create secret docker-registry dev-registry \
  --docker-server=zot.apps.lab.example.invalid \
  --docker-username=<pull user> --docker-password="$(security find-generic-password -s <pull entry> -w)"
```

Then `make dev` (see [loop.md](loop.md)). The Tiltfile allows the contexts `homelab`, `homelab-koinon`
and `kind-koinon-*`.

## Dev database

The chart renders the single-instance CloudNativePG `Cluster` `koinon-pg` (`postgres.mode: cnpg`),
using the platform's operator, and generates one password Secret per component role
([loop.md](loop.md)).

## Troubleshooting

- `ImagePullBackOff` with 401: the `dev-registry` secret is missing or has the wrong password.
- Large layer pushes time out: the Zot HTTPRoute needs `timeouts: {request: 0s}` (already set).
- `koinon-pg` stays pending: check the operator in `cnpg-system`; its admission webhook must be
  reachable before the `Cluster` can be created.
