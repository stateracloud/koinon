<!-- SPDX-License-Identifier: Apache-2.0 -->

# Releasing (plan item REL1, ADR 0027)

A release is a Git tag. Pushing `vX.Y.Z` or `vX.Y.Z-rc.N` runs
[`.github/workflows/release.yml`](../../.github/workflows/release.yml), which publishes to the
private `ghcr.io/stateracloud` and drafts the GitHub release. Nothing is committed for a version:
the images, the chart's `version` and `appVersion` all come from the tag, without the leading `v`.

| Artifact | Where |
| --- | --- |
| `koinon-api` (with the web UI), `koinon-reconciler`, `koinon-agentd` | `ghcr.io/stateracloud/<name>:<version>`, linux/amd64 and linux/arm64, ko SPDX SBOMs. `latest` moves only for final releases |
| `flavor-opencode`, the OpenCode flavor (ADR 0059) | `ghcr.io/stateracloud/flavor-opencode:<version>`, linux/amd64 and linux/arm64, BuildKit SBOM and provenance attestations. `latest` moves only for final releases |
| Helm chart | `oci://ghcr.io/stateracloud/charts/koinon`, tag `<version>` |
| Release notes | A draft GitHub release, from the Conventional Commits since the previous tag: features, fixes, others. `docs`, `ci`, `test`, `chore` and `style` commits are left out |

## Cut a release candidate

1. Merge everything to `main` and wait for the `ci` and `secrets` checks on the merge commit. `ci`
   cancels a run when a newer commit lands on `main`, so a superseded merge commit can be left with
   cancelled checks: tag the newest commit whose checks are green.
2. Tag the merge commit and push the tag (signed tags are fine; the workflow only needs the commit):

   ```sh
   git switch main && git pull
   git tag -s v0.1.0-rc.1 -m "v0.1.0-rc.1"
   git push origin v0.1.0-rc.1
   ```

3. Watch the `release` workflow. The `gate` job runs first and fails the release if the tag is not
   `vX.Y.Z` or `vX.Y.Z-rc.N`, the commit is not an ancestor of `origin/main`, or any required check
   of the `main` ruleset (dco, go, web, proto, migrations, generated code, gitleaks, chart, escape
   (admission)) has not concluded `success` on it. It reruns no tests. The `dco` workflow runs on
   pull requests only, so the gate reads that check from the head commit of the pull request that
   merged the tag's commit.
4. Check that every job succeeded: `release` and `flavor` run side by side after `gate`, so a failed
   `flavor` still leaves a draft release and a chart whose default flavor image does not exist, and
   it skips `sign`. Then open the draft release and read the notes. `-rc.N` releases are already
   marked as a prerelease. Publishing the draft is the only manual step.

A failed run can be re-run from the Actions page ("Re-run failed jobs" reruns `flavor`, then
`sign`): the draft release is replaced, the images are pushed again (the flavor under the same tag
with a new digest) and the chart tag is overwritten. Do not move a tag that was published; cut `rc.N+1`.

## Cut a final release

Same steps with `vX.Y.Z` after the last rc was exercised (REL2). The final release also moves the
`latest` image tag and is not marked as a prerelease.

## Signing and SBOM attestations

The workflow can sign with cosign keyless and attest the SBOMs, but its `sign` job runs only when
the repository variable `RELEASE_SIGNING` is `true` (default: unset, off). Keyless signing writes
image names and digests to the public Rekor log (ADR 0049), so leave it off while the repository is
private.

The `sign` job runs after `release` and `flavor` and is the only job with `id-token: write`. It gets
`dist/digests.txt` and the SPDX SBOMs from the release job as an uploaded artifact (`hack/release-refs.sh`
turns the digests into image references), signs every image by digest and the pushed chart artifact,
and attaches each Go image's SBOM as a cosign attestation (`cosign attest --type spdxjson`). The
flavor image is signed by the digest the `flavor` job reports; its SBOM is BuildKit's attestation,
pushed with the image. Verify with
`cosign verify` against the workflow's identity once it is on.

Set it with `gh variable set RELEASE_SIGNING --body true`, and remove it again with
`gh variable delete RELEASE_SIGNING`.

## What protects release tags

The workflow's gate checks that the tagged commit is on `main`, but anyone who can push a tag can
start a release from a commit on a branch that was merged later, or move a tag afterwards. Two tag
rulesets on `refs/tags/v*` close that (repository settings, added 2026-10-26; not created by the
repository's code):

| Ruleset | Blocks | Bypass |
| --- | --- | --- |
| `release tags: admins create` | creating a matching tag | repository admins |
| `release tags: immutable` | updating or deleting a matching tag | none |

They are separate because a ruleset's bypass covers all of its rules: one ruleset with an admin
bypass would let admins move or delete a published tag. To fix a bad tag, cut the next `rc.N`; only
disabling the `immutable` ruleset in the repository settings lets anyone move or delete one.

## Mirror a release to the home lab

REL2 run A installs the rc from the home-lab Zot ([registry.md](registry.md)), not from ghcr.io.
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

### The flavor image when installing a release

The packaged chart defaults `flavors.opencode.image` to `<image.registry>/flavor-opencode:<appVersion>`,
which the release publishes (the `flavor` job, `hack/release-flavor.sh`) and `hack/mirror-release.sh`
copies, so a default install needs no flavor values. To run a flavor you built yourself (`make
flavors`, then tag and push), set `flavors.opencode.image.repository` and `.tag`.
`flavors.<x>.image.registry` does not exist; a flavor in another registry needs
`flavors.<x>.image.repository` to be a path below `image.registry`.

## Dry runs

```sh
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign   # builds the web UI and loads the images into Docker
```

`KOINON_IMAGE_REGISTRY=localhost:5000` redirects a real (non-snapshot) run's image push to a
throwaway `registry:2`, which is how `hack/mirror-release.sh` is tested; combine it with a copy of the
config whose `release` is disabled so that nothing reaches GitHub.

The flavor image is built outside GoReleaser. Dry-run it against a throwaway registry with a
builder that can reach it:

```sh
docker run -d --rm --name dryrun-registry -p 127.0.0.1:5000:5000 registry:2
docker buildx create --name dryrun --driver docker-container --driver-opt network=host
BUILDX_BUILDER=dryrun hack/release-flavor.sh 0.0.0-rc.0 localhost:5000 "$(git rev-parse HEAD)"
```

It prints the pushed index's digest reference. The gate does not check the flavor contract suite
(`flavors` in `ci.yml` is not a required check and runs only when flavor files change), and CI
builds only the runner's own platform, so the release's QEMU build of the other platform is the
first one: dry-run it on an x86 host (where arm64 is emulated, as in the release) before an rc that
changes `flavors/`.

## Not released yet

- Signatures and attestations (the `sign` job), until `RELEASE_SIGNING` is switched on (when the repository goes
  public).
- Binary archives and checksums; the release ships images and the chart.
- A "local checks recorded" gate in place of the CI status gate (`hack/release-gate.sh`).
