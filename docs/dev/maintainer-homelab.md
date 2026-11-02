<!-- SPDX-License-Identifier: Apache-2.0 -->

# Maintainer's home lab

This page describes the maintainer's home lab: its Zot registry, the dev loop against it and release
mirroring. It is not needed to contribute. Contributors use kind ([loop.md](loop.md)) or a remote
cluster of their own (loop.md, "Remote cluster"). The maintainer's values map onto that generic
profile as described under "Home-lab values in the remote profile" below.

## Dev registry

Tilt pushes images to the home lab's Zot registry (ADR 0047). Zot is part of the home-lab platform
(Argo CD), not installed by Koinon.

- URL: `<registry host from lab.env>`, Koinon images under `koinon/`
- Zot v2.1.22 in namespace `zot`. It is also the cluster's pull-through cache: anonymous reads
  work only for mirrored upstreams (docker.io, ghcr.io, quay.io, registry.k8s.io), so `koinon/*`
  needs credentials for both push and pull.

## One-time setup

The credentials come from the maintainer's secret store; these commands never print them.

```sh
# On your Mac: ko and Tilt push with this login
<read the push credential from the secret store> |
  docker login <registry host from lab.env> -u <push user> --password-stdin

# In the cluster: the pull secret the dev manifests reference.
# Prefer a read-only Zot account here over your push credentials.
kubectl --context <your lab kube context> create namespace koinon --dry-run=client -o yaml |
  kubectl --context <your lab kube context> apply -f -
kubectl --context <your lab kube context> -n koinon create secret docker-registry dev-registry \
  --docker-server=<registry host from lab.env> \
  --docker-username=<pull user> --docker-password="<read the pull credential from the secret store>"
```

Then `make dev` (see [loop.md](loop.md)). The Tiltfile allows `kind-koinon-*` and the contexts listed in your git-ignored `hack/dev/local.yaml`
(see "Maintainer config outside the repository" below).

## Maintainer config outside the repository

Nothing of the lab is tracked (issue #508): the repository works without it. The maintainers keep
these in `${XDG_CONFIG_HOME:-$HOME/.config}/koinon/`:

| File | What |
| --- | --- |
| `lab.env` | `KOINON_E2E_ARGO_SERVER`, `_BUNDLE_URL`, `_RESULTS_URL`, `_S3_REGION`, `_ARGO_CREDS`, `_LIGHT_MANIFEST` for `hack/e2e-remote.sh --argo` (template `hack/lab/lab.env.example`; the environment overrides it) |
| `lab/creds.sh` | the credentials functions of `hack/e2e-argo.sh` (contract `hack/lab/creds.example.sh`) |
| `lab/light.workflow.json` | the Argo Workflow of `--lane light` (superseded by the lab's `koinon-gate` WorkflowTemplate, `--gate <set>`; no file needed) |
| `lab/local.homelab.example.yaml`, `values-homelab.yaml`, `model-provider-homelab.yaml` | the Tilt remote profile of the lab |

Without `lab.env`, `hack/e2e-remote.sh --argo` exits 78 (no lab configured) and names what is missing.

## Where a pull request's kind e2e runs (#547)

A needs-e2e pull request (`hack/needs-e2e.sh`) is tested lab first. A green lab run is enough to merge,
recorded as a commit status. ADR [0025](../adr/0025-testing-strategy.md) (amendment) has the decision.

1. Push the branch, then `hack/e2e-remote.sh --argo` (about 15-25 min; `--no-wait` and `--follow <run>`
   to come back later). A followed run posts the commit status `e2e-lab` on the commit it tested: `success`
   on exit 0, `failure` when the e2e failed, nothing on a fallback. The description names the run and the
   target URL is the run's page on the Argo Server from `lab.env`. A branch that is not pushed cannot take
   a status: the script says so; push, then `hack/e2e-status.sh <sha> success|failure <description> [url]`.
   `--no-status` (or `KOINON_E2E_NO_STATUS=1`) is for local experiments.
2. `hack/pr-wait.sh <PR> --checks --require e2e` is the merge gate: it passes on a green `e2e-lab` on the
   head, or on any green `quickstart` check run on the head (also a dispatched one next to a skipped
   pull_request run). Then auto-merge as usual.
3. GitHub's `e2e-kind` is the fallback, `gh workflow run e2e-kind.yml --ref <branch>`, when:

| Exit of `--argo` | Meaning | What to do |
| --- | --- | --- |
| 0 | the e2e passed, `e2e-lab` is success | merge gate 2 |
| 1 | the e2e failed, `e2e-lab` is failure | fix it |
| 75 | the lab cannot run it now: unreachable, a failed upload or submit, no verdict | GitHub e2e-kind |
| 78 | no lab configured | GitHub e2e-kind |
| 79 | the change touches `flavors/`: the lab cannot build a flavor image (`nvcr.io` is fenced) | GitHub e2e-kind, dispatched by hand (nothing triggers it for a flavors/ PR); `--allow-flavors` if the image is already in Zot |
| 2 | usage, or the lab rejected the request | fix the call |

   `hack/e2e-remote.sh --local` (your own Docker) is a dev and debug tool, not merge evidence: it yields
   neither `e2e-lab` nor `quickstart`. A run that fails with no result.txt (a hang, an OOM) exits 75, so it
   goes to GitHub and leaves no lab result. `--no-status` is not remembered by the run: a later `--follow`
   posts unless it gets `--no-status` too.

   The `koinon-gate` lane (`--gate <set>`, not the e2e) has the same 75 / 76 contract: 75 is "run the gate
   locally", including the 120 s Pending fallback (reasons `quota`, `unschedulable`, `namespace-limit`,
   `slot`; the run is deleted, never stopped).
4. A pull request that changes the e2e's own files (`needs-e2e.sh --forced-only`) gets `e2e-kind` on GitHub
   by itself, so the lab run is optional there; the `e2e` label dispatches it too.
5. Main stays batched on GitHub (each push that changed something e2e-relevant, one running and the newest
   pending) and nightly, always.

`e2e-lab` is not a required check of the ruleset, because contributors without a lab cannot produce it;
the required check stays `e2e result`.

### Lanes and notifications

Each lane of a parallel session runs `hack/e2e-remote.sh --argo --repo <its worktree>` with
`KOINON_E2E_AGENT=<session>-<lane>` and keeps at most one kind run waiting. The lab's notifications
(ntfy) carry the agent label as a tag (`?tags=<agent>`).

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
tilt up --context <your lab kube context>
```

### Home-lab values in the remote profile

The home lab is the generic remote profile ([loop.md](loop.md#remote-cluster)) with these values,
kept as `local.homelab.example.yaml` in the maintainer config directory (below), not in the repository.
Copy it to `hack/dev/local.yaml` (git-ignored); the Tiltfile reads only that file.

| Home-lab value | Where it lives now |
| --- | --- |
| Zot, `<registry host from lab.env>/koinon` | `registry` |
| the `dev-registry` pull secret | `imagePullSecret` |
| OpenShell at `openshell.openshell.svc:8080`, runtime class `kata`, mTLS Secret `openshell-client-tls` | `openshell.*` |
| the Ollama provider | `modelProvider:` (`model-provider-homelab.yaml`, same directory) |
| Ollama egress host and network policy | `values:` (`values-homelab.yaml`, same directory) |
| the `<lab secret-store label from lab.env>` namespace label | `namespaceLabels` (it is no longer on the kind namespace) |
| sandbox policies off | `sandboxPolicy: false` |

Tilt builds for `linux/amd64`. Change single values without editing files:

```sh
tilt up --context <your lab kube context> -- --helm-set openshell.address=openshell.other.svc:8080 --helm-set openshell.runtimeClassName=kata-qemu
tilt up --context <your lab kube context> -- --openshell-tls-secret=   # no mTLS (the default copies openshell-client-tls)
tilt up --context <your lab kube context> -- --openshell-namespace=openshell
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
tmp="$(mktemp -d "${TMPDIR:-/tmp}/docker-config.XXXXXX")"
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
hack/mirror-release.sh 0.1.0-rc.1 <registry host from lab.env>/koinon
```

It copies the four images (the three Go images and `flavor-opencode`, every platform, plus any
cosign signature, attestation and SBOM tags) and the chart to `<registry>/<name>:<version>` and `<registry>/charts/koinon:<version>`. The script
uses the credentials in the docker config and never prints them. Cosign 3 stores its bundles as
OCI referrers; the script copies them only where they also exist under the `sha256-<hex>` tag, so
referrers-API-only output is not copied. Install the mirrored chart with
`helm install koinon oci://<registry>/charts/koinon --version <version> --set image.registry=<registry>`.


## RT fixtures on the home lab

`kubectl --context <your lab kube context> apply -k test/fixtures/rt1/overlays/homelab` applies the story-test fixtures
with the images from Zot pinned by digest; the Secrets come from OpenBao through ExternalSecrets.
Everyone else uses `overlays/kind` or `overlays/remote` ([real-model-tests.md](real-model-tests.md)).
