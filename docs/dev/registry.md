# Dev registry

Tilt pushes images to a Zot registry on the home lab (ADR 0047). The registry is installed with the
home-lab platform, not by Koinon.

- URL: `registry.apps.lab.example.invalid`, images under `koinon/`
- Push user `koinon-push` (your Mac), pull user `koinon-pull` (cluster)
- Chart: `oci://ghcr.io/project-zot/helm-charts/zot` 0.1.128 (Zot v2.1.22), namespace `koinon-dev`

## One-time setup

```sh
# On your Mac
docker login registry.apps.lab.example.invalid -u koinon-push   # ko and Tilt use this

# In the cluster: the pull secret the dev manifests reference
kubectl create namespace koinon --dry-run=client -o yaml | kubectl apply -f -
kubectl -n koinon create secret docker-registry dev-registry \
  --docker-server=registry.apps.lab.example.invalid \
  --docker-username=koinon-pull --docker-password='<password>'
```

Then `make dev`. The Tiltfile only allows the `homelab` kube context.

## Troubleshooting

- Large layer pushes time out: set `timeouts: {request: 0s}` on the registry HTTPRoute rule.
- `ImagePullBackOff` with 401: the `dev-registry` secret is missing or has the wrong password.
