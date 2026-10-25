<!-- SPDX-License-Identifier: Apache-2.0 -->

# Releasing (plan item REL1, ADR 0027)

A release is a Git tag. Pushing `vX.Y.Z` or `vX.Y.Z-rc.N` runs
[`.github/workflows/release.yml`](../../.github/workflows/release.yml), which publishes to the
private `ghcr.io/stateracloud` and drafts the GitHub release. Nothing is committed for a version:
the images, the chart's `version` and `appVersion` all come from the tag, without the leading `v`.

| Artifact | Where |
| --- | --- |
| `koinon-api` (with the web UI), `koinon-reconciler`, `koinon-agentd` | `ghcr.io/stateracloud/<name>:<version>`, linux/amd64 and linux/arm64, ko SPDX SBOMs. `latest` moves only for final releases |
| Helm chart | `oci://ghcr.io/stateracloud/charts/koinon`, tag `<version>` |
| Release notes | A draft GitHub release, from the Conventional Commits since the previous tag: features, fixes, others. `docs`, `ci`, `test`, `chore` and `style` commits are left out |

## Cut a release candidate

1. Merge everything to `main` and wait for the `ci`, `dco` and `secrets` checks on the merge commit.
2. Tag the merge commit and push the tag (signed tags are fine; the workflow only needs the commit):

   ```sh
   git switch main && git pull
   git tag -s v0.1.0-rc.1 -m "v0.1.0-rc.1"
   git push origin v0.1.0-rc.1
   ```

3. Watch the `release` workflow. The `gate` job runs first and fails the release if the tag is not
   `vX.Y.Z` or `vX.Y.Z-rc.N`, the commit is not an ancestor of `origin/main`, or any required check
   of the `main` ruleset (dco, go, web, proto, migrations, generated code, gitleaks, chart, escape
   (admission)) has not concluded `success` on it. It reruns no tests. `dco` runs on pull requests
   only, so it is read from the head commit of the pull request that merged the tag's commit.
4. Open the draft release and read the notes. `-rc.N` releases are already marked as a prerelease.
   Publishing the draft is the only manual step.

A failed run can be re-run from the Actions page: the draft release is replaced, the images are
pushed again and the chart tag is overwritten. Do not move a tag that was published; cut `rc.N+1`.

## Cut a final release

Same steps with `vX.Y.Z` after the last rc was exercised (REL2). The final release also moves the
`latest` image tag and is not marked as a prerelease.

## Signing and SBOM attestations

The workflow can sign with cosign keyless and attest the SBOMs, but it does so only when the
repository variable `RELEASE_SIGNING` is `true` (default: unset, off). Keyless signing writes image
names and digests to the public Rekor log (ADR 0049), so leave it off while the repository is private.

When it is on, the `release` job (the only one with `id-token: write`) signs every image manifest
through GoReleaser's `docker_signs`, signs the pushed chart artifact, and attaches each image's SPDX
SBOM as a cosign attestation (`cosign attest --type spdxjson`). Verify with
`cosign verify` against the workflow's identity once it is on.

Set it with `gh variable set RELEASE_SIGNING --body true`, and remove it again with
`gh variable delete RELEASE_SIGNING`.

## Mirror a release to the home lab

REL2 run A installs the rc from the home-lab Zot ([registry.md](registry.md)), not from ghcr.io.
With `crane` and `jq` installed and `docker login` done for both registries:

```sh
hack/mirror-release.sh 0.1.0-rc.1 zot.apps.lab.example.invalid/koinon
```

It copies the three images (every platform, plus any cosign signature, attestation and SBOM tags)
and the chart to `<registry>/<name>:<version>` and `<registry>/charts/koinon:<version>`. The script
uses the credentials in the docker config and never prints them. Install the mirrored chart with
`helm install koinon oci://<registry>/charts/koinon --version <version> --set image.registry=<registry>`.

## Dry runs

```sh
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign   # builds the web UI and loads the images into Docker
```

`KOINON_IMAGE_REGISTRY=localhost:5000` redirects a real (non-snapshot) run's image push to a
throwaway `registry:2`, which is how `hack/mirror-release.sh` is tested; combine it with a copy of the
config whose `release` is disabled so that nothing reaches GitHub.

## Not released yet

- **Flavor images** (`opencode`, `hermes`, `pi`). The flavor design is being reopened as "bring your
  own harness", so v0.1 releases none. `make flavors` still builds them for development and CI.
- Signatures and attestations, until `RELEASE_SIGNING` is switched on (when the repository goes
  public).
- Binary archives and checksums; the release ships images and the chart.
- A "local checks recorded" gate in place of the CI status gate (`hack/release-gate.sh`).
