<!-- SPDX-License-Identifier: Apache-2.0 -->

# Maintainer's home lab

This page describes the maintainer's home lab: its Zot registry, the dev loop against it and release
mirroring. It is not needed to contribute. Contributors use kind ([loop.md](loop.md)) or a remote
cluster of their own (loop.md, "Remote cluster"). The maintainer's values map onto that generic
profile as described under "Home-lab values in the remote profile" below.

## Dev registry

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

## Dev loop on the home lab

One-time setup: the Zot login and the `dev-registry` pull secret ([Dev registry](#dev-registry)). Then:

```sh
kubectl config use-context homelab
tilt up --context homelab
```

### Home-lab values in the remote profile

The home lab is the generic remote profile ([loop.md](loop.md#remote-cluster)) with these values,
checked in as `hack/dev/local.homelab.example.yaml`. When the context is `homelab` or `homelab-koinon`
and there is no `hack/dev/local.yaml`, the Tiltfile uses that file, so the command above is unchanged.
To change something, copy it to `hack/dev/local.yaml` (git-ignored) and edit the copy.

| Home-lab value | Where it lives now |
| --- | --- |
| Zot, `zot.apps.lab.example.invalid/koinon` | `registry` |
| the `dev-registry` pull secret | `imagePullSecret` |
| OpenShell at `openshell.openshell.svc:8080`, runtime class `kata`, mTLS Secret `openshell-client-tls` | `openshell.*` |
| the Ollama provider | `modelProvider: hack/dev/model-provider-homelab.yaml` |
| Ollama egress host and network policy | `values: [hack/dev/values-homelab.yaml]` |
| the `lab.example.invalid/openbao` namespace label | `namespaceLabels` (it is no longer on the kind namespace) |
| sandbox policies off | `sandboxPolicy: false` |

Tilt builds for `linux/amd64`. Change single values without editing files:

```sh
tilt up --context homelab -- --helm-set openshell.address=openshell.other.svc:8080 --helm-set openshell.runtimeClassName=kata-qemu
tilt up --context homelab -- --openshell-tls-secret=   # no mTLS (the default copies openshell-client-tls)
tilt up --context homelab -- --openshell-namespace=openshell
```

The home-lab gateway requires mTLS (docs/spikes/openshell.md), so the profile copies the Secret
`openshell-client-tls` from the OpenShell namespace into `koinon`.

### Cluster-wide objects on the home lab

The sandbox ValidatingAdmissionPolicies and their bindings (ADRs 0045, 0048) are cluster-wide and
deny writes in every `openshell.ai/managed-by=openshell` namespace, which would reach the platform's
existing sandboxes. The home-lab profile therefore renders them off. `tilt up -- --sandbox-policy`
turns them on; that needs a Rivet request that Andre approves first. The sandbox observer (ADR 0078)
needs the policies, so it is rendered off too.
With them off, nothing at admission enforces the Kata runtime class on home-lab dev sandboxes: that
rests on OpenShell's `default_runtime_class_name: kata` and the reconciler's `runtimeClassName`, so
the escape suite's admission cases don't hold there.

The cluster-scoped objects it still applies are listed in [loop.md](loop.md#remote-cluster).

## Docker config caveat

The registry setup above logs in with `docker login`, which writes to the Docker config. Do not
work around a locked keychain by pointing `DOCKER_CONFIG` at an empty directory: that also hides
the config's `cli-plugins/` and `contexts/`, so `docker buildx` ("unknown command") and
`docker context` (OrbStack, Colima) break. Build the temporary directory with a `config.json`
that has no `credsStore` and symlinks to the real `cli-plugins` and `contexts`:

```sh
real="${DOCKER_CONFIG:-$HOME/.docker}"
tmp="$(mktemp -d)"
echo '{"auths":{}}' > "$tmp/config.json"
ln -s "$real/cli-plugins" "$tmp/cli-plugins"
ln -s "$real/contexts" "$tmp/contexts"   # only if it exists
export DOCKER_CONFIG="$tmp"
```

For a push to Zot, add that registry's `auths` entry to the temporary `config.json` (or run
`docker login` with the variable set). The same applies to `make dev-kind-up` and Tilt on kind
when the default config's credential helper hangs; there the empty `auths` is all you need.

## Mirror a release to the home lab

REL2 run A installs the rc from the home-lab Zot ([Dev registry](#dev-registry)), not from ghcr.io.
With `crane` and `jq` installed and `docker login` done for both registries:

```sh
hack/mirror-release.sh 0.1.0-rc.1 zot.apps.lab.example.invalid/koinon
```

It copies the four images (the three Go images and `flavor-opencode`, every platform, plus any
cosign signature, attestation and SBOM tags) and the chart to `<registry>/<name>:<version>` and `<registry>/charts/koinon:<version>`. The script
uses the credentials in the docker config and never prints them. Cosign 3 stores its bundles as
OCI referrers; the script copies them only where they also exist under the `sha256-<hex>` tag, so
referrers-API-only output is not copied. Install the mirrored chart with
`helm install koinon oci://<registry>/charts/koinon --version <version> --set image.registry=<registry>`.


## RT fixtures on the home lab

`kubectl --context homelab apply -k test/fixtures/rt1/overlays/homelab` applies the story-test fixtures
with the images from Zot pinned by digest; the Secrets come from OpenBao through ExternalSecrets.
Everyone else uses `overlays/kind` or `overlays/remote` ([real-model-tests.md](real-model-tests.md)).
