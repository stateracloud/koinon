# 0027. Monorepo, one version, supply-chain hygiene

- Status: Accepted; amended 2026-10-25 (below)
- Date: 2026-10-21
- Decision: [design.md#27](../design.md)

## Context

Koinon's components (API, reconciler, agentd, web UI, flavor images, connectors, templates, Helm
chart) change together: a new connector scope touches the proto API, the reconciler's policy
rendering, the UI card and the chart. Separate repos would need cross-repo version matrices. Users
install Koinon into clusters that run untrusted agent code, so releases must be signed and carry
SBOMs.

## Decision

- Monorepo layout: `api/ cmd/ internal/ web/ flavors/ connectors/ templates/ charts/ test/ docs/`.
- Go images are built with ko.
- Connectors and templates are data (setup guides, scopes, templates), not code plugins.
- SemVer with one shared version for all components.
- CRDs stay `v1alpha1` until 1.0.
- `buf breaking` runs in CI on the protobuf API.
- Migrations are forward-only, with an upgrade test from the previous release.
- Releases: GoReleaser (OSS) with its `kos` pipe for images; images signed keylessly with cosign
  from GitHub Actions through `docker_signs` (`artifacts: manifests`); the Helm chart published as an
  OCI artifact to ghcr.io.
- SBOMs: ko generates image SBOMs (SPDX, pushed to the registry); GoReleaser's syft-based SBOMs
  cover archives and binaries only.

## Alternatives considered

- Polyrepo per component _(reconstructed)_. Rejected: components move together.
- Docker/Buildx images instead of ko _(reconstructed)_. Kept for flavor images, which are not plain
  Go binaries; ko for Go services _(inferred)_.
- Per-component versions _(reconstructed)_. Rejected: compatibility matrix for users.
- Reversible (down) migrations _(reconstructed)_. Rejected: down migrations are rarely tested and
  risky with data; forward fixes instead.
- GoReleaser Pro _(reconstructed)_. Not needed: kos pipe and signing are in OSS; only the `if`
  filter is Pro.

## Consequences

- Positive: one version to reason about; signed, attested artifacts.
- Negative: a release ships every component even if only one changed (accepted).
- Negative: flavor images need a separate build path (Dockerfiles), not ko _(inferred)_.
- Negative: Helm 4 is current and Helm 3 gets no new minor releases; the chart must be tested with
  Helm 4 and should stay compatible with Helm 3 for users who have not moved _(inferred)_.
- Follow-ups: `.goreleaser.yaml`, release workflow with `id-token: write`, chart push job (done in
  REL1, see [releasing.md](../dev/releasing.md)).

## Open questions

- Whether flavor images get their own release cadence because they track upstream harness releases.
  Not decided; v0.1 releases no flavor images at all (see Amendment).
- Minimum supported Helm version (3.x still in use vs 4.x only). The chart is packaged and tested
  with Helm 4 (v4.3.1); a Helm 3 minimum is not promised.

## Amendment (REL1, 2026-10-25)

- Flavor images are not released in v0.1. The flavor design is being reopened as "bring your own
  harness", so a release builds only the three Go images (`koinon-api`, `koinon-reconciler`,
  `koinon-agentd`) and the chart. The Dockerfile build path stays for development (`make flavors`).
  _(Changed: #167 adds `koinon/flavors/opencode` to the released images; see the list below.)_
- Signing and SBOM attestations are not done by GoReleaser's `docker_signs`. A separate `sign` job
  (the only one with `id-token: write`) runs cosign over the pushed digests and the chart, and only
  when the repository variable `RELEASE_SIGNING` is `true` (ADR [0049](0049-v01-scope-decisions.md)).

## Amendment (#167, 2026-11-05)

- Every published package moved under the public repository's namespace, `ghcr.io/stateracloud/koinon/*`,
  so the repository's own Actions token owns them: the packages created while the code lived in
  the old private repository stayed linked to that repository, and Actions access to a package can only be
  granted in the browser. The images are `koinon/api`, `koinon/reconciler`, `koinon/agentd` and
  `koinon/flavors/opencode`; the chart is `oci://ghcr.io/stateracloud/koinon/charts/koinon`; the e2e
  toolchain images are `koinon/e2e-toolchain` and `koinon/e2e-toolchain-web`. Where a workflow can,
  the prefix comes from `github.repository`; elsewhere it is the chart's `image.registry` default.
  The old packages (`koinon-api`, `flavor-opencode` and the rest) are neither deleted nor changed.
  The packages stay private until v0.3. The spike probe image is no longer built.

## Upstream verification

Checked 2026-10-21:
- ko v0.19.1; GoReleaser v2.18.3, kos pipe in OSS, ko images signed via `docker_signs`.
  https://goreleaser.com/customization/ko/, https://goreleaser.com/pro/
- GoReleaser SBOMs exclude container images: https://goreleaser.com/customization/sbom/
- cosign v3.1.4 keyless in Actions (`permissions: id-token: write`).
  https://docs.sigstore.dev/quickstart/quickstart-ci/
- Helm v4.3.1; OCI push to ghcr.io: https://helm.sh/docs/topics/registries/
- buf v1.75.0, buf.yaml v2, breaking default `FILE`, BSR not required.
  https://buf.build/docs/configuration/v2/buf-yaml/
- goose forward-only migrations (Down optional): https://github.com/pressly/goose

## Links

- [design.md](../design.md), decision 27
- ADR [0034](0034-project-identity.md), [0037](0037-tooling.md)
