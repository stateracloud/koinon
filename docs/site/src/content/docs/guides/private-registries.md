---
title: Private registries and mirrors
description: Pull Koinon's images with credentials or through a pull-through registry such as Artifactory, Nexus or Harbor.
---

Koinon runs a lot of images. Three are Koinon's own, and the rest come from other projects. An
air-gapped or locked-down cluster needs two things: credentials to pull from a private registry, and
a way to pull everything through one internal registry. The chart has a value for each. The sandbox
pods are the exception: OpenShell creates them, so you set their pull secret on the OpenShell release.

## Values

```yaml chart-values
global:
  # Pull Secrets (type kubernetes.io/dockerconfigjson, in the release namespace) for every pod the
  # chart renders, and for the CNPG Postgres Cluster. [regcred] works too.
  imagePullSecrets:
    - name: regcred
  # One host for everything...
  imageRegistry: artifactory.example.com/proxy
  # ...or one entry per upstream. A host listed here wins over imageRegistry.
  registryMirrors:
    ghcr.io: artifactory.example.com/ghcr-remote
    docker.io: artifactory.example.com/docker-remote
```

Use `imageRegistry` or `registryMirrors`, or both. `imagePullSecrets` at the top level of the values
still works. It is deprecated, and the chart merges it with `global.imagePullSecrets`.

## The rewrite rule

Every image reference the chart renders goes through one rule:

1. The registry host is the first path element if it contains a `.` or a `:`, or is `localhost`.
   Anything else is on `docker.io`, and a bare `name` is `docker.io/library/name`.
2. If `registryMirrors` has an entry for the host, the host is replaced with that value.
3. Otherwise, if `imageRegistry` is set, the host is replaced with that.
4. Otherwise the reference is untouched.

The path, tag and digest are kept, so a digest pin still verifies through a pull-through cache.
Values may carry a path prefix. With `--set`, escape the dots in a host key:
`--set 'global.registryMirrors.ghcr\.io=artifactory.example.com/ghcr-remote'`.

| Reference | `registryMirrors: {ghcr.io: art.example.com/ghcr-remote}` | `imageRegistry: art.example.com/proxy` |
| --- | --- | --- |
| `ghcr.io/stateracloud/koinon/api:1.0.0` | `art.example.com/ghcr-remote/stateracloud/koinon/api:1.0.0` | `art.example.com/proxy/stateracloud/koinon/api:1.0.0` |
| `ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie@sha256:…` | `art.example.com/ghcr-remote/cloudnative-pg/postgresql:18.6-system-trixie@sha256:…` | `art.example.com/proxy/cloudnative-pg/postgresql:18.6-system-trixie@sha256:…` |
| `quay.io/org/img:1` | unchanged | `art.example.com/proxy/org/img:1` |
| `postgres:18` | unchanged | `art.example.com/proxy/library/postgres:18` |

### What the rewrite covers

| Image | Where |
| --- | --- |
| `api`, `reconciler`, `agentd` | the three Deployments |
| `api` | the migrate, ensure-secrets and preflight Jobs, and the preflight probe pods (`--probe-image`) |
| `flavors/opencode` and any other flavor | `flavors.*.image`: the reconciler gives it to OpenShell as the sandbox image |
| agentgateway | `gateway.standalone.image`, in standalone mode |
| PostgreSQL | `postgres.cnpg.imageName` (the CNPG `Cluster`), in cnpg mode |
| OpenShell's supervisor image | the default `sandboxPolicy.supervisorImages`, see below |

`image.registry` is the upstream the Koinon images are published at, `ghcr.io/stateracloud/koinon`. The rewrite
starts from it, so leave it at its default when you use `imageRegistry` or `registryMirrors`. If you
copied the images to your own registry with `hack/mirror-release.sh` and set `image.registry` to it
without a mirror setting, nothing is rewritten and that keeps working.

### Which registry shape fits

- **Artifactory**: one remote repository per upstream, `art.example.com/<repo-key>/<path>` (or one host
  per repository). Use `registryMirrors` with `ghcr.io` and `docker.io` entries. A virtual repository
  that aggregates the upstreams works with `imageRegistry` as long as the paths are the upstream ones.
- **Harbor**: a proxy-cache project per upstream, `harbor.example.com/<project>/<path>`. Use
  `registryMirrors`. Harbor wants `library/` on Docker Hub official images, which the rule adds.
- **Nexus**: a Docker proxy repository per upstream, on its own port or host
  (`nexus.example.com:8082/<path>`). Use `registryMirrors` with a host and port per upstream.

Check a reference by hand before you rely on it: `docker pull <rewritten reference>`.

## Sandboxes: set the pull secret on OpenShell

The workload pod and the supervisor pod of every sandbox are created by OpenShell and the Agent
Sandbox controller. Koinon's sandbox API has no pull-secret field, and the chart does not copy
Secrets into sandbox namespaces or change ServiceAccounts there. OpenShell does this itself. On the
**OpenShell** release, set:

```yaml
server:
  # Secrets in the OpenShell namespace. For every sandbox the gateway copies them into the sandbox's
  # workspace namespace and puts them in imagePullSecrets of both pods.
  sandboxImagePullSecrets:
    - name: regcred
```

Create `regcred` in OpenShell's namespace (`kubectl -n openshell create secret docker-registry …`).
Use a read-only token that can pull only the repositories sandboxes need. The kubelet reads the copy
to pull. The sandbox itself never sees it: sandbox pods mount no ServiceAccount token and have no
route to the API server.

The preflight checks this. When `global.imagePullSecrets`, `global.imageRegistry` or
`global.registryMirrors` is set, it reads OpenShell's gateway ConfigMap (`openshell-config` in the
`openshell` namespace; change it with `preflight.openshell.namespace` and `.configMap`) and warns if
the gateway has no `image_pull_secrets`. It only warns: the nodes may hold the credentials, or the
mirror may be anonymous.

## When the pull fails

A flavor image that a node cannot pull leaves the sandbox waiting. After the org's wait cap the run fails with
the cause **the image could not be pulled** and the registry's error as its detail (for example a failed name
lookup, `pull access denied` or `manifest unknown`), and the agent is marked unhealthy. Koinon learns this by
reading the sandbox's pod ([How Koinon finds the cause](../agent-health/#how-koinon-finds-the-cause)); with
`sandboxObserver.enabled: false` the cause shows as unknown.

## The supervisor image

OpenShell starts a supervisor pod for every sandbox, and Koinon's admission policy admits exactly one
supervisor image (`sandboxPolicy.supervisorImages`). With the list empty, which is the default, the
policy admits OpenShell 0.1.7's own image, `ghcr.io/nvidia/openshell/supervisor:0.1.7`, rewritten by the
same rule as every other image. So when you point OpenShell at the same mirror, the two agree. To pin
something else, list it: an explicit list is used exactly as written. The preflight compares the list with
the `supervisor_image` in OpenShell's gateway ConfigMap and warns when they differ, because every
sandbox would then be denied.

The ConfigMap read itself is a failure, not a warning: when a supervisor image is configured (the chart always passes one) and the preflight cannot read the ConfigMap, or it has no `gateway.toml`, the release fails, because Koinon could not tell whether sandboxes would be admitted.

## Components the chart does not install

Koinon's chart cannot change another release's values. Point these at the mirror in their own charts:

| Component | Setting |
| --- | --- |
| OpenShell gateway, sandbox-runtime and supervisor images | `global.image.registry` on the OpenShell chart. Its default is `ghcr.io/nvidia`, and the repository paths (`openshell/gateway`, `openshell/sandbox`, `openshell/supervisor`) are added to it, so the mirror value is `art.example.com/ghcr-remote/nvidia`. |
| OpenShell's pods | `imagePullSecrets` (gateway and helper pods) and `server.sandboxImagePullSecrets` (sandboxes) |
| CloudNativePG operator | its chart's `image.repository` and `imagePullSecrets` |
| Agent Sandbox controller | the controller's image in its manifest, or a mirror on the nodes |
| agentgateway controller (`gateway.mode: kubernetes`) | its chart's image and `imagePullSecrets` values. The proxy pods it creates take their image from its chart too. |

The CNPG operator and Agent Sandbox are installed before Koinon, so their images are yours to place.

## Node-level mirrors and credentials

You can leave all image references alone and configure the nodes instead. containerd reads a
`hosts.toml` per registry from its `config_path` directory (`/etc/containerd/certs.d/ghcr.io/hosts.toml`,
or `_default/hosts.toml` for every registry) that names a mirror:

```toml
server = "https://ghcr.io"

[host."https://art.example.com/v2/ghcr-remote"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

`hosts.toml` carries TLS settings, not credentials. The kubelet supplies those: pull Secrets, or a
[kubelet credential provider](https://kubernetes.io/docs/tasks/administer-cluster/kubelet-credential-provider/).
Image references stay unchanged, so digests and the supervisor pin keep working. The cost is node access,
and every pod on the node can pull with the node's credentials.
