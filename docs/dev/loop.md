<!-- SPDX-License-Identifier: Apache-2.0 -->

# Dev loop with Tilt (plan item X1, ADR 0039)

`tilt up` builds the Koinon images and deploys `charts/koinon` into the namespace `koinon`, the same
chart a user installs, with the values in `hack/dev/`. It replaces the earlier kustomize manifests.
There are two profiles; the kube context picks one.

| | `homelab` | `kind` |
| --- | --- | --- |
| Context | `homelab` or `homelab-koinon` | `kind-koinon-*` (`make dev-kind-up` makes `kind-koinon-dev`) |
| Registry | Zot, `zot.apps.lab.example.invalid/koinon` ([registry.md](registry.md)) | none; ko and `kind load` put images into the node |
| Sandbox runtime | `kata` | `runc` (kind has no VM runtime) |
| OpenShell | the one already on the cluster | OpenShell 0.1.5, installed by `make dev-kind-up` |
| Model | home-lab Ollama, `qwen3.5:4b` | the fake model server (model `repo-helper`) |
| GitHub | not configured | the fake GitHub App API |
| Values | `hack/dev/values.yaml` + `values-homelab.yaml` | `hack/dev/values.yaml` + `values-kind.yaml` |

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
- `openshell-client-cert`: copies the OpenShell client certificate Secret into `koinon` (kind only
  by default, see below).
- `web-deps`, `web`: the Vite dev server for `web/` on http://localhost:5173, proxying the API.
  `tilt up -- --web=false` leaves it out.
- The API is port-forwarded to http://localhost:8080.

The chart's hooks are rendered by `helm template` but applied without hook semantics: the preflight,
ensure-secrets and migration Jobs are ordinary resources ordered with resource dependencies, and
their `hook-delete-policy` is ignored (the Jobs stay after they succeed). The chart renders no
password and no gateway config, so Tilt needs no workaround for `helm template`; the gateway's
config ConfigMap is created by the reconciler.

## kind

Needs Docker, kind, kubectl, helm, jq, Go, ko, Tilt and pnpm. Pulling images (ko base image, charts)
needs a Docker config without a locked credential helper; on a Mac with a locked keychain use
`DOCKER_CONFIG` pointing at a directory whose `config.json` has no `credsStore`.

```sh
make dev-kind-up          # kind cluster kind-koinon-dev: CNPG, Agent Sandbox, runc RuntimeClass, OpenShell (about 1 minute)
tilt up                   # the current context is kind-koinon-dev
# or, without the UI: tilt ci
make dev-kind-down        # delete the cluster
```

Another name: `DEV_KIND_CLUSTER=koinon-foo make dev-kind-up`
and a context named `kind-koinon-foo`. A fresh `tilt ci` takes about a minute with a warm Go cache.

## Home lab

One-time setup: the Zot login and the `dev-registry` pull secret ([registry.md](registry.md)). Then:

```sh
kubectl config use-context homelab
tilt up --context homelab
```

Tilt builds for `linux/amd64`. The home-lab profile assumes OpenShell at
`openshell.openshell.svc:8080` and the `kata` RuntimeClass; change them without editing files:

```sh
tilt up --context homelab -- --helm-set openshell.address=openshell.other.svc:8080 --helm-set openshell.runtimeClassName=kata-qemu
tilt up --context homelab -- --openshell-tls-secret=   # no mTLS (the default copies openshell-client-tls)
tilt up --context homelab -- --openshell-namespace=openshell
```

The home-lab gateway requires mTLS (docs/spikes/openshell.md), so both profiles copy the Secret
`openshell-client-tls` from the OpenShell namespace into `koinon`; `--openshell-tls-secret=` turns
that off, another name picks another Secret.

### Cluster-wide objects on the home lab

The sandbox ValidatingAdmissionPolicies and their bindings (ADRs 0045, 0048) are cluster-wide and
deny writes in every `openshell.ai/managed-by=openshell` namespace, which would reach the platform's
existing sandboxes. The home-lab profile therefore renders them off. `tilt up -- --sandbox-policy`
turns them on; that needs a Rivet request that procoduck approves first. The kind profile always has them.
With them off, nothing at admission enforces the Kata runtime class on home-lab dev sandboxes: that
rests on OpenShell's `default_runtime_class_name: kata` and the reconciler's `runtimeClassName`, so
the escape suite's admission cases don't hold there.

The cluster-scoped objects the home-lab profile still applies, both needed:

- ClusterRole and ClusterRoleBinding `koinon-koinon-reconciler`: the reconciler gets, lists, watches
  and deletes `sandboxes.agents.x-k8s.io`. The binding is cluster-wide, so this holds in every
  namespace, not only the OpenShell workspace namespaces.
- ClusterRole and ClusterRoleBinding `koinon-koinon-preflight`: read-only. `get` on the CRDs, the
  Agent Sandbox controller, the RuntimeClass and the like that the install preflight checks, and
  in cnpg mode `list` on `validatingwebhookconfigurations` and `endpointslices` cluster-wide.

## Logging in

The `koinon-ensure-secrets` Job generates the bootstrap admin password into the Secret
`koinon-bootstrap-admin` once and never overwrites it. Nothing is stored in the repository.

```sh
kubectl -n koinon get secret koinon-bootstrap-admin -o jsonpath='{.data.password}' | base64 -d
```

Log in as `admin`. Dev images do not embed the UI, so use the Vite
server on http://localhost:5173 (see [web.md](web.md)); the API itself is on http://localhost:8080.

## Overrides

`tilt up -- --help` lists the options: `--mode`, `--flavors`, `--platform`, `--openshell-tls-secret`,
`--openshell-namespace`, `--helm-set key=value` (repeatable), `--web`.

## Troubleshooting

- `kube context ... is not a Koinon dev cluster`: switch to `homelab`, `homelab-koinon` or a
  `kind-koinon-*` context.
- `ImagePullBackOff` with 401 on the home lab: `dev-registry` is missing or wrong, see
  [registry.md](registry.md). Sandbox namespaces are created by OpenShell and need their own pull
  access to `koinon/flavor-*` in Zot.
- `koinon-preflight` fails: it names the missing prerequisite (Agent Sandbox, OpenShell, the
  RuntimeClass, CloudNativePG). Its Job is kept for `kubectl -n koinon logs job/koinon-preflight`.
- `tilt down` deletes the release objects including the database and its volume.
