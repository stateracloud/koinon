<!-- SPDX-License-Identifier: Apache-2.0 -->

# Dev loop with Tilt (plan item X1, ADR 0039)

`tilt up` builds the Koinon images and deploys `charts/koinon` into the namespace `koinon`, the same
chart a user installs, with the values in `hack/dev/`. It replaces the earlier kustomize manifests.
There are two profiles, picked by the kube context: **kind** (`kind-koinon-*`, the default for
everyone, no configuration) and **remote** (a cluster of your own, configured in a git-ignored file).

| | `kind` | `remote` |
| --- | --- | --- |
| Context | `kind-koinon-*` (`make dev-kind-up` makes `kind-koinon-dev`) | any context listed under `contexts` in `hack/dev/local.yaml` |
| Registry | none; ko and `kind load` put images into the node | yours (`registry` in local.yaml); Tilt pushes there |
| Sandbox runtime | `runc` (kind has no VM runtime) | `openshell.runtimeClassName` (for example `kata`) |
| OpenShell | OpenShell 0.1.5, installed by `make dev-kind-up` | the one already on the cluster (`openshell.*`) |
| Model | the fake model server (model `repo-helper`) | the provider manifest named by `modelProvider`, or add one in the UI |
| GitHub | the fake GitHub App API | not configured |
| Values | `hack/dev/values.yaml` + `values-kind.yaml` | `hack/dev/values.yaml` + local.yaml's keys and `values` files |

The Tiltfile refuses every other context, so a stray `tilt up` cannot touch another cluster.

## What Tilt runs

- `koinon-api`, `koinon-reconciler`, `koinon-agentd`: built with ko (`./cmd/...`) on every Go
  change, injected into the chart's pods. Each image also gets the tag `dev`, which the chart uses
  where Tilt cannot inject (the preflight Job's probe pods).
- `flavor-<name>`: `make flavor-<name>` (tag from `hack/flavor-tag.sh`), then `kind load` or
  `docker push`. The chart gets that tag, so a flavor change rolls the reconciler. Only `opencode`
  exists in v0.1 (ADR 0059): `tilt up -- --flavors=opencode`.
- `postgres` (CloudNativePG `Cluster` from the chart), `koinon-migrate-install` (`koinon-api
  migrate`), `koinon-preflight`, `koinon-ensure-secrets` (`koinon-api ensure-secrets`: the generated database and
  bootstrap-admin passwords, created once and never overwritten), `koinon-gateway` (standalone agentgateway), `koinon-config` (the
  rest of the chart's objects).
- `openshell-client-cert`: copies the OpenShell client certificate Secret into `koinon` (both profiles;
  `--openshell-tls-secret=` turns it off).
- `web-deps`, `web`: the Vite dev server for `web/` on http://localhost:5173, proxying the API.
  `tilt up -- --web=false` leaves it out.
- The API is port-forwarded to http://localhost:8080.

The chart's hooks are rendered by `helm template` but applied without hook semantics: the preflight,
ensure-secrets and migration Jobs are ordinary resources ordered with resource dependencies, and
their `hook-delete-policy` is ignored (the Jobs stay after they succeed). The chart renders no
password and no gateway config, so Tilt needs no workaround for `helm template`; the gateway's
config ConfigMap is created by the reconciler.

## kind

Needs Docker, kind, kubectl, helm, jq, Go, ko, Tilt and pnpm (versions in
[CONTRIBUTING.md](../../CONTRIBUTING.md#requirements)). Everything is pulled anonymously. If your
Docker config uses a credential helper that is locked or hangs (a Mac with a locked keychain), point
`DOCKER_CONFIG` at a directory with an empty `config.json` for the Koinon commands: the pulls are anonymous.

```sh
make dev-kind-up          # kind cluster kind-koinon-dev: CNPG, Agent Sandbox, runc RuntimeClass, OpenShell (about 1 minute)
tilt up                   # the current context is kind-koinon-dev
# or, without the UI: tilt ci
make dev-kind-down        # delete the cluster
```

Another name: `DEV_KIND_CLUSTER=koinon-foo make dev-kind-up`
and a context named `kind-koinon-foo`. A fresh `tilt ci` takes about a minute with a warm Go cache.

## Remote cluster

For a cluster that is not kind (a dev cluster of your own, a VM runtime for the `vm` isolation tier):
you need a container registry the cluster can pull from, an OpenShell gateway and the Agent Sandbox
and CloudNativePG operators on it (the install preflight names anything missing), and `docker login`
for the registry.

```sh
cp hack/dev/local.example.yaml hack/dev/local.yaml     # git-ignored; edit it
cp hack/dev/model-provider.example.yaml hack/dev/model-provider.local.yaml   # optional, git-ignored
tilt up --context my-dev-cluster
```

`local.yaml` names, and never holds, secrets:

| Key | Meaning |
| --- | --- |
| `contexts` | kube contexts this profile may deploy to (required) |
| `registry` | where Tilt pushes images and the cluster pulls them (`image.registry`; required) |
| `platform` | build platform of the nodes (default `linux/amd64`) |
| `imagePullSecret` | name of a pull Secret you created in `koinon` |
| `openshell.address`, `.runtimeClassName`, `.namespace`, `.clientTLSSecret` | the gateway, the sandbox runtime class, and the gateway's namespace and client certificate Secret that Tilt copies into `koinon` (`""`: no mTLS) |
| `sandboxPolicy` | render the cluster-wide sandbox admission policies (default off on a remote cluster, because they also deny writes in other OpenShell sandbox namespaces; always on for kind) |
| `modelProvider` | a manifest declaring the default org's model provider (ADR 0056) |
| `values` | extra chart values files, for the model endpoint's network policy and egress hosts |
| `namespaceLabels` | labels added to the `koinon` namespace |

The command line overrides the file (`--helm-set`, `--openshell-tls-secret=`, `--openshell-namespace`,
`--sandbox-policy`, `--platform`).

Tilt builds for the platform you set; images are pushed with `docker push` (flavors) and ko (Koinon).
With `sandboxPolicy` off, nothing at admission enforces the sandbox runtime class: that rests on
OpenShell's default runtime class and the reconciler's `runtimeClassName`, so the escape suite's
admission cases do not hold on that cluster. The sandbox observer (ADR 0078) needs the policies, so
it is rendered off with them: an image pull error shows as an unknown wait cause.

Cluster-scoped objects the remote profile applies even with the policies off: ClusterRole and
ClusterRoleBinding `koinon-koinon-reconciler` (get, list, watch and delete `sandboxes.agents.x-k8s.io`,
cluster-wide) and `koinon-koinon-preflight` (read-only: the CRDs, the Agent Sandbox controller, the
RuntimeClass and so on that the preflight checks; in cnpg mode `list` on
`validatingwebhookconfigurations` and `endpointslices`).

A profile for your own cluster goes in `hack/dev/local.yaml`; it is not tracked.

## Logging in

The `koinon-ensure-secrets` Job generates the bootstrap admin password into the Secret
`koinon-bootstrap-admin` once and never overwrites it. Nothing is stored in the repository.

```sh
kubectl --context <your context> -n koinon get secret koinon-bootstrap-admin -o jsonpath='{.data.password}' | base64 -d
```

Log in as `admin`. Dev images do not embed the UI, so use the Vite
server on http://localhost:5173 (see [web.md](web.md)); the API itself is on http://localhost:8080.

## Overrides

`tilt up -- --help` lists the options: `--mode`, `--flavors`, `--platform`, `--openshell-tls-secret`,
`--openshell-namespace`, `--sandbox-policy`, `--helm-set key=value` (repeatable), `--web`.

## Troubleshooting

- `kube context ... is not a Koinon dev cluster`: switch to a `kind-koinon-*` context, or list the
  context under `contexts` in `hack/dev/local.yaml`.
- `ImagePullBackOff` with 401 on a remote cluster: the pull Secret named by `imagePullSecret` is
  missing or wrong. Sandbox namespaces are created by OpenShell and need their own pull access to
  `flavor-*` in your registry.
- `koinon-preflight` fails: it names the missing prerequisite (Agent Sandbox, OpenShell, the
  RuntimeClass, CloudNativePG). Its Job is kept for `kubectl --context <your context> -n koinon logs job/koinon-preflight`.
- `tilt down` deletes the release objects including the database and its volume.
