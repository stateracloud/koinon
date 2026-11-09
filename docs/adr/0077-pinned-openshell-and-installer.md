# 0077. Koinon pins OpenShell and Agent Sandbox per release, and ships an installer

- Status: Accepted (delivery after v0.1.0, in v0.1.x)
- Date: 2026-10-28
- Amends: [0049](0049-v01-scope-decisions.md) (prerequisites "with pinned install steps in the docs")
- Relates to: [0027](0027-repo-and-versioning.md), [0048](0048-openshell-spike-results.md), [0061](0061-image-pull-secrets-and-registry-mirrors.md), [0075](0075-isolation-tiers.md), [0076](0076-database-modes.md)

## Context

Today the chart "does not install its prerequisites" (`docs/site/src/content/docs/prerequisites.md:6-7`).
Before `helm install`, a team installs and pins five things by hand:

- a NetworkPolicy engine where needed;
- CNPG;
- Agent Sandbox v1.0.7;
- a RuntimeClass;
- OpenShell 0.1.5.

`test/e2e/kind/up.sh:15-91` does exactly this for kind, with checksums and digests. The preflight
accepts exactly these versions: `SupportedOpenShellVersions = []string{"0.1.5"}`, Agent Sandbox
v1.0.x and agentgateway v1.7.x (`internal/preflight/preflight.go:52-75`).

procoduck, 2026-10-28: Koinon pins and upgrades OpenShell and Agent Sandbox in each release.
Bring-your-own stays possible with a version check.

What the upstream projects provide:

- **OpenShell** publishes a Helm chart at `oci://ghcr.io/nvidia/openshell/helm-chart` with no
  subchart dependencies. Stable releases ship "generally … every week". Patch releases keep Stable
  interfaces backward-compatible. Fixes go to "the latest minor release and the previous minor
  release, also called N-1", on the newest patch of each line
  (`docs/about/support-matrix.mdx@v0.1.5:29-43`). The chart needs Agent Sandbox first and does not
  install or upgrade its CRDs (`docs/kubernetes/setup.mdx@v0.1.5:44-70`). After an Agent Sandbox
  upgrade, the gateway must restart (`:85-87`).
- **Agent Sandbox** v1.0.7 publishes manifests only: `sandbox.yaml`, `sandbox-with-extensions.yaml`
  and `extensions.yaml`. Its Helm chart lives in the source tree (`helm/`, CRDs under `crds/`) and is
  not a release asset. Helm installs `crds/` once and never upgrades them.
- **How others ship:**
  - kagent installs a CRD chart, then the main chart, and has a CLI with
    `kagent install --profile demo`.
  - E2B Embed ships one machine as Compose, Terraform or a single-node Kubernetes package
    (`e2b-dev/infra` `embed/README.md`).
  - GitLab, Coder and Rancher keep their stateful and cluster-scoped dependencies out of the main
    chart for production.

## Decision

### Pins

1. **Each Koinon release pins exact versions** of OpenShell (chart version and digest, gateway,
   supervisor and sandbox-runtime images) and Agent Sandbox (manifest SHA-256 and controller image
   digest), and tests them:
   - the kind e2e;
   - the escape suite at each qualified tier ([0075](0075-isolation-tiers.md));
   - an upgrade test from the previous Koinon release's pins.

   The pins live in one file, `release/versions.yaml`, and are read by:
   - the preflight (the supported versions);
   - both install paths (decisions 3 and 4);
   - `hack/docs-check.sh` (the prerequisites page);
   - `test/e2e/kind/up.sh`.

   Nothing else hard-codes a version. agentgateway, which the chart already bundles, and the
   standalone Postgres image ([0076](0076-database-modes.md)) are pinned in the same file.

2. **OpenShell's support window.** A Koinon release pins an OpenShell version on a supported line:
   the latest minor or N-1, at that line's newest patch. A pin that falls out of the window is moved
   to a supported line in Koinon's next release. A security patch on the pinned line is picked up in
   a Koinon patch release after the e2e and the escape suite pass. Koinon does not skip a minor line
   when it upgrades, so each upgrade is one step the upgrade test covers.

### Two install paths, one set of releases

Argo CD and Istio both offer two paths. Argo CD ships manifests and a Helm chart next to its CLI.
Istio ships `istioctl install` next to plain Helm charts (base, istiod, gateway) installed in a
documented order. Koinon offers both paths too.

3. **Plain Helm.** Every component installs with `helm install` from a published chart, in a
   documented order. The docs publish the pinned versions and digests from `release/versions.yaml`:
   1. **Agent Sandbox**, from the vendored chart (decision 6).
   2. **OpenShell**, from its upstream chart. The docs give the values Koinon requires:
      - `workspaceMode: managed`;
      - telemetry off;
      - the tier's RuntimeClass and user-namespace setting (ADR 0075);
      - `sandboxImagePullSecrets` and mirrors matching Koinon's `global` values (ADR 0061).
   3. **Koinon**, from `charts/koinon`, unchanged in role. The preflight still runs as its
      pre-install hook.

   Each step that is not a `helm` command is a documented `kubectl` command: copying the OpenShell
   client certificate, and restarting the gateway after an Agent Sandbox upgrade, as OpenShell's
   docs require. GitOps users (Argo CD, Flux) take these charts and values directly, one
   Application or HelmRelease each, ordered with sync waves or `dependsOn`.

4. **The `koinon` CLI.** The `koinon install` and `koinon upgrade` commands live in a new
   user-facing `koinon` CLI, a static Go binary attached to each release. They run exactly the
   plain-Helm steps of decision 3, in order, and wait for each one:
   - The CLI shells out to `helm` and `kubectl` (already prerequisites) instead of linking the Helm
     SDK, so `go.mod` gains no Helm dependency.
   - Each step it runs is in the docs. **The CLI never does anything plain Helm and the documented
     `kubectl` steps can't.**
   - Flags:
     - `--profile eval|production`;
     - `--tier vm|hardened|dev`;
     - `--postgres external|cnpg|standalone`; the `eval` profile sets `standalone` (ADR 0076);
     - `--render`, which writes the same releases as `helm upgrade --install` commands, values
       files and the `kubectl` steps, for GitOps or review, instead of applying them;
     - `--bring-your-own openshell,agent-sandbox`, which skips those releases and only checks them
       (decision 7).
   - Both paths install the same pinned releases. The e2e tests both: the CLI, and the plain-Helm
     steps run from the docs as the quickstart's `e1` blocks are today.
5. **What neither path installs:** the CNPG operator (ADR 0076), a CNI or NetworkPolicy
   engine, Kata or any node runtime, and cert-manager. The preflight checks these and names the fix.

### Agent Sandbox and bring-your-own

6. **The Agent Sandbox chart is vendored.** `third_party/agent-sandbox/chart` holds the upstream
   `helm/` directory at the pinned tag, unmodified, with a README naming the tag, commit and licence
   (Apache-2.0), like `third_party/openshell`. It is published as an OCI chart under
   `ghcr.io/stateracloud/charts/agent-sandbox`. Its version is the upstream version with a
   `-koinon.N` suffix, and its image is the upstream controller image pinned by digest.

   Helm never upgrades `crds/`, so the published chart carries the CRDs as templates with
   `helm.sh/resource-policy: keep`. That way `helm upgrade` upgrades them on both paths, and
   `helm uninstall` never deletes them. This is the one change to the upstream layout, and the
   README records it. When upstream publishes its own chart, Koinon switches to it and drops the
   vendored copy.

7. **Bring-your-own stays.** A platform that runs its own OpenShell or Agent Sandbox points Koinon at
   it, as today (`openshell.address`, `preflight.agentSandbox.*`). The preflight's version check
   reads the supported versions from `release/versions.yaml`. For bring-your-own components it
   accepts the pinned version and any newer patch on the same minor line, with a warning that names
   the tested pin. Any other version fails, as today.

### Staged path

8. **v0.1.x:**
   - `release/versions.yaml`;
   - the plain-Helm install order in the docs, with the pinned versions;
   - the `koinon install` and `koinon upgrade` commands with `--render`;
   - the vendored Agent Sandbox chart;
   - OpenShell values rendered by the installer;
   - the quickstart rewritten around `koinon install`, with the prerequisites page keeping the
     manual steps.

9. **v0.2:**
   - an umbrella chart, `koinon-stack`, built from the same pins, with two value profiles:
     - `eval`: `standalone` Postgres, port-forward, one org;
     - `production`: `external` or `cnpg`, Ingress or HTTPRoute, OIDC required.
   - The umbrella orders its parts with a post-install check instead of relying on the pre-install
     preflight.
   - The installer can install it as one release instead of three.

10. **Later:**
   - an operator (a `KoinonPlatform` resource that owns upgrades and version skew);
   - a single-node appliance (k3s, Kata, Koinon) on a KVM host or VM image with nested
     virtualisation; E2B Embed is the reference.

   Each needs its own ADR.

## Consequences

- **An install in an afternoon.** `koinon install` replaces five manual installs. The remaining
  prerequisites are platform-level: a VM-capable or hardened node pool, NetworkPolicy, and a
  database when the mode is not `standalone`.
- **Koinon owns OpenShell and Agent Sandbox upgrades.** That is a weekly upstream cadence to
  follow, an upgrade test per release, and security patches on the pinned line within a release
  cycle.
- **One source of version truth.** The preflight, docs, e2e and installer cannot drift.
- **Users with their own OpenShell keep it,** with a clear supported range.
- **Two install paths to keep equal.** Every release tests the CLI and the documented Helm steps,
  and keeps the vendored chart true _(inferred)_.
- **GitOps users use the charts directly,** with the documented values and order, and need no CLI.
- **Follow-ups:**
  - `release/versions.yaml` and its readers;
  - the plain-Helm install guide;
  - `cmd/koinon` with `install`, `upgrade` and `--render`;
  - `third_party/agent-sandbox`;
  - the upgrade test;
  - the quickstart rewrite;
  - the OpenShell client certificate in Koinon's namespace.

## Alternatives

- **Keep the prerequisites manual (today).** This gives clean ownership, but five installs before
  the first `helm install`, and every team re-learns OpenShell's values. Rejected.
- **A helmfile shipped with each release.** It no longer adds anything: the published charts in a
  documented order are what a helmfile would wrap, and the CLI and `--render` cover ordering and
  review. Not shipped. A team that wants a helmfile can write one from the published charts and
  versions.
- **The CLI only, or plain Helm only.** The CLI alone shuts out GitOps users who install charts
  directly. Plain Helm alone leaves a small team with three releases and kubectl steps to run by
  hand. procoduck chose both, as Argo CD and Istio do.
- **An umbrella chart now.** One release, but Helm never upgrades the CRDs it installs, the
  pre-install preflight would run before the bundled OpenShell exists, and cluster-scoped
  controllers collide with existing ones. Deferred to v0.2, with a post-install check.
- **An operator now.** The best for upgrades, but a new cluster-admin component before the pins and
  installer are proven. Later.
- **Apply Agent Sandbox's manifest instead of vendoring its chart.** Simpler, and this is what
  `up.sh` does, but it gives no Helm release to upgrade, roll back or render for GitOps. Rejected.

## Amendment (2026-11-09): OpenShell 0.1.7

The pin moves from 0.1.5 to 0.1.7 (issue 421; the release is older than the 3 days of
[0079](0079-dependency-cooldown.md)). `SupportedOpenShellVersions` is `[]string{"0.1.7"}`, the kind e2e
and the spike installer pin the chart by the digest of the 0.1.7 tag
(`sha256:3e5998c749390a6be3a4f553e52e488b6f692e19be7b8bf681a26285d1e18901`, read from the ghcr.io
manifest), and the vendored protos in `third_party/openshell` are the v0.1.7 files (additions only).
The decisions above are unchanged. The text and the verification below describe the 0.1.5 pin of
2026-10-28.

## Upstream verification

- 2026-10-28, OpenShell `v0.1.5`:
  - `docs/about/support-matrix.mdx:29-43` (cadence, N-1 support);
  - `docs/kubernetes/setup.mdx:35-87` (Agent Sandbox first, CRDs not installed by the chart, restart
    after an upgrade);
  - `deploy/helm/openshell/Chart.yaml` (no dependencies).
- 2026-10-28, Agent Sandbox `v1.0.7`: release assets (manifests only); `helm/README.md` (CRDs in
  `crds/`, installed before other resources).
- 2026-10-28:
  - kagent installation docs;
  - `e2b-dev/infra` `README.md` and `embed/README.md` (commit `b7ca8364ea`).
- Research notes: the session report `research/standalone.md`, sections 4 to 6.

## Links

- [0048](0048-openshell-spike-results.md), [0049](0049-v01-scope-decisions.md),
  [0075](0075-isolation-tiers.md), [0076](0076-database-modes.md)
- https://github.com/NVIDIA/OpenShell/blob/v0.1.5/docs/about/support-matrix.mdx
- https://github.com/kubernetes-sigs/agent-sandbox/tree/v1.0.7/helm
- [`test/e2e/kind/up.sh`](../../test/e2e/kind/up.sh)
