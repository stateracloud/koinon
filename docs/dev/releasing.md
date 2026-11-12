<!-- SPDX-License-Identifier: Apache-2.0 -->

# Releasing (plan item REL1, ADR 0027)

A release is a Git tag. Pushing `vX.Y.Z` or `vX.Y.Z-rc.N` runs
[`.github/workflows/release.yml`](../../.github/workflows/release.yml), which publishes to the
private `ghcr.io/stateracloud/koinon` (every package lives under the repository that builds it, so
the workflow's own token owns it; #167) and drafts the GitHub release. Nothing is committed for a version:
the images, the chart's `version` and `appVersion` all come from the tag, without the leading `v`.

| Artifact | Where |
| --- | --- |
| `api` (with the web UI), `reconciler`, `agentd` | `ghcr.io/stateracloud/koinon/<name>:<version>`, linux/amd64 and linux/arm64, ko SPDX SBOMs. `latest` moves only for final releases |
| `flavors/opencode`, the OpenCode flavor (ADR 0059) | `ghcr.io/stateracloud/koinon/flavors/opencode:<version>`, linux/amd64 and linux/arm64, BuildKit SBOM and provenance attestations. `latest` moves only for final releases |
| Helm chart | `oci://ghcr.io/stateracloud/koinon/charts/koinon`, tag `<version>` |
| Release notes | A draft GitHub release, from the Conventional Commits since the previous tag: features, fixes, others. `docs`, `ci`, `test`, `chore` and `style` commits are left out |

## Cut a release candidate

1. Merge everything to `main` and wait for the `ci` and `secrets` checks on the merge commit. `ci`
   cancels a run when a newer commit lands on `main`, so a superseded merge commit can be left with
   cancelled checks: tag the newest commit whose checks are green. Write down its SHA: step 3 tags this SHA, not
   whatever `main` is by then.
2. Re-check the gateway fallback, for every release, rc and final (ADR 0040, amendment 2026-11-12).
   Do it before you push the tag. Koinon stays on agentgateway behind `ModelGateway`; Agent Router
   (formerly Envoy AI Gateway, `github.com/theagentrouter/agent-router`) is the documented fallback
   for the model path while agentgateway is the driver (ADR 0097 amends ADR 0040: agentgateway is
   only the first driver behind Koinon's own gateway). The change that ships ADR 0097 step 3
   (agentgateway removed) deletes this step and closes the amendment in ADR 0040.

   - Look at the upstream pull request theagentrouter/agent-router#2834
     (https://github.com/theagentrouter/agent-router/pull/2834) and at the Agent Router release notes
     since the last release. Note in the release pull request or the tracking issue whether it is
     open, merged or closed, and the version of Agent Router you looked at.
   - Check the three gaps that ADR 0040 gives as reasons not to choose it: budgets are token windows
     of at most one day with no USD or never-reset budget; quota fails open by default; it needs
     Envoy Gateway. That pull request would emit partial metrics when the stream is torn down mid-response.
   - Reconsider the fallback only if one of these holds: agentgateway fails a requirement of ADR
     0040 or ADR 0041 that we cannot fix (USD budgets per key, unpriced models, spend after a
     streamed response, MCP authorization), or Agent Router closes all three gaps. Weigh also the
     reasons in ADR 0097 for rejecting Agent Router as the data plane: it charges after the
     response, it needs Redis, and it brings vendor coupling. Then open an
     issue and write an ADR that amends or supersedes ADR 0040. Otherwise write "no change" and go
     on.

3. Tag the commit you wrote down in step 1 and push the tag (signed tags are fine; the workflow only
   needs the commit). A commit may have landed on `main` during step 2; do not tag it, because the
   `release` gate checks the tagged commit and tags are immutable:

   ```sh
   git fetch origin
   git tag -s v0.1.0-rc.1 -m "v0.1.0-rc.1" <sha-from-step-1>
   git push origin v0.1.0-rc.1
   ```

4. Watch the `release` workflow. The `gate` job runs first and fails the release if the tag is not
   `vX.Y.Z` or `vX.Y.Z-rc.N`, the commit is not an ancestor of `origin/main`, or any required status
   check that applies to `main` has not concluded `success`. The gate reads that list at run time from
   `rules/branches/<default>` (every active repository and organization rule for the branch, branch
   patterns resolved by GitHub), so adding or removing a required check needs no edit to
   `hack/release-gate.sh`; if it cannot read the rules, or finds no required checks, it fails closed.
   Each context is read in this order: the tagged commit itself; where it reports `skipped` (the push to
   main skips its heavy jobs when the merge queue already tested the tree, `hack/ci-skip-heavy.sh`), the
   successful `merge_group` `ci` run on a commit with the same tree (the queue build, found through the
   run's own `head_commit.tree_id` among the newest `QUEUE_RUN_LIMIT`; the result is read from that run's
   check runs (Actions app, by run id; `dco`, `claim` and `e2e result` from the other `merge_group` workflow runs on that head), not as the newest check run on the commit, because a queue merge
   fast-forwards main to the queue commit and the push run adds a later `skipped` to the same sha); and, for the three a push to main
   does not report as a result, the queue build first and then the merged pull request's head:
   `dco` (pull requests and the queue only), `claim` (`pull_request`, `merge_group`; skipped on main) and
   `e2e result` (a composite that also passes when the e2e was skipped on main; its real subject, the kind
   e2e, the gate asks for through the `quickstart` rule below). `go`, `web` and every other `ci` job are
   never read from a PR head: a draft's green is vacuous. `quick checks` (proto, migrations, adr-lint,
   claim-tests and the dependency age steps, which run on pull requests only) must succeed on the tagged
   commit and also on the merged pull request's head (`both` in the script). The three above are listed in
   the script (`pr_only`); the pull request head is the newest run that started before the merge (the
   `closed` event reruns some of them as skipped). They are listed rather than inferred from "no run on the
   commit", so a new push-triggered required check that is missing or red on the commit cannot be excused.
   When a workflow gains or loses a `push` trigger, move its context in or out of `pr_only`.
   An admin-merged commit has no queue build: its `ci` jobs ran in its own push run, `dco`, `claim` and
   `e2e result` come from the PR head, and the `quickstart` rule fails with a dispatch hint unless the
   batched push run ran it. A skipped `go-test` needs the queue build's `go-test` to be green, or a change
   with no Go (the lab status `go-test-lab` is a pull-request-only shortcut and no longer excuses it).
   A queue run older than the newest `QUEUE_RUN_LIMIT` (90 days of API retention at most) is not found and
   the gate fails closed.
   The gate also requires the `quickstart` job of the kind e2e to have succeeded on the commit. It
   reruns no tests. `hack/release-gate-test.sh` tests it against a stubbed `gh`.

   Patch releases (vX.Y.Z, Z > 0) are tagged on `main` like every other release. There is no schema
   or upgrade promise before v0.3 (ADR 0099), so the marker `hack/migrate-patch-line` stays `none`
   for v0.1 and v0.2 and their release branches may contract. Once the v0.3 line has shipped its
   `.0`, set the marker to that line (`v0.3`); from then on the `migrations` check applies the
   patch-line rule (ADR 0085) to every migration a pull request adds: no rename, drop or retype, so
   the previous patch release keeps working on the new schema. The marker is the only switch, on
   `main` and on `release/*` bases alike, so land the marker change before you cut `release/v0.3`.
   The change that opens the next minor line resets the file to `none`, which lets it contract. The
   file is read at the merge base, so the same change cannot lift the rule.
5. Check that every job succeeded: `release` and `flavor` run side by side after `gate`, so a failed
   `flavor` still leaves a draft release and a chart whose default flavor image does not exist, and
   it skips `sign`. Then open the draft release and read the notes. `-rc.N` releases are already
   marked as a prerelease. Publishing the draft is the only manual step.

A failed run can be re-run from the Actions page ("Re-run failed jobs" reruns `flavor`, then
`sign`): the draft release is replaced, the images are pushed again (the flavor under the same tag
with a new digest) and the chart tag is overwritten. Do not move a tag that was published; cut `rc.N+1`.

## Cut a final release

Same steps with `vX.Y.Z` after the last rc was exercised (REL2). The final release also moves the
`latest` image tag and is not marked as a prerelease.

Checklist for the v0.1.0 gate (ADR 0085 decision 1 and implementation item 22, ADR 0099). It is
tracked in #313 ("release: v0.1.0 tag-time gate checklist"), separate from the docs change that
added it. Tick it on the tag commit, not before:

- [ ] The restore drill passed on a real cluster (ADR 0085 decision 15), with RTO and RPO recorded
      (#275).
- [ ] The ADR 0085 implementation issues still in the v0.1.0 gate (decisions 2-4 and 13-16, ADR 0099
      decision 2) are closed.
- [ ] `hack/release-gate.sh` is green for the commit.
- [ ] The `quickstart` job of the kind e2e succeeded on the tagged commit.
- [ ] The release notes carry the pre-v0.3 notice (upgrade may need a reinstall or manual steps;
      backups and the restore runbook are supported), from the template below.

Checklist step for every `vX.Y.0` from v0.3.0 on (not for v0.1.0 or v0.2.0, ADR 0099): after the
final tag is published, open a pull request that sets `hack/migrate-patch-line` to `vX.Y` (for
v0.3.0: `v0.3`). Until it merges, main can still contract the schema that the shipped release runs
on (ADR 0085).

Checklist step for every `vX.Y.0`, with the one above: record the persisted-state compatibility
folder (old issue 652) from the tagged commit, so later changes are checked against what that release wrote:

```sh
git switch --detach vX.Y.0
hack/record-compat-fixtures.sh vX.Y.0 --out "${TMPDIR:-/tmp}/compat-vX.Y.0"
git switch -c test/compat-vX.Y.0 origin/main
mkdir -p testdata/compat && cp -R "${TMPDIR:-/tmp}/compat-vX.Y.0" testdata/compat/vX.Y.0
```

Commit the folder on a branch from `main` and merge it by pull request. `internal/compat` loads every
folder under `testdata/compat/` and, until the first one exists, reports "NO RECORDED RELEASE". Do
not record a release candidate. Refreshing and what to do when a folder stops loading:
[docs/dev/ci.md](ci.md#persisted-state-compatibility).

## Release notes: "Upgrading from vX.Y"

The generated notes list commits only. Before you publish the draft (step 5), add an "Upgrading from
vX.Y" section by hand, where X.Y is the previous minor line (for `v0.2.0`, "Upgrading from v0.1").
Operators follow [Upgrading Koinon](https://koinon.stateracloud.com/docs/operate/upgrading/) (ADR 0085); the notes
say what is specific to this release. Template:

```markdown
## Upgrading from vX.Y

- **Procedure:** stop, migrate, start (the minor upgrade of the Upgrading Koinon guide).
  For a patch release write "rolling upgrade, no schema change that breaks the previous patch" instead.
- **Rollback:** none short of restoring the pre-upgrade database and key backup. For a patch
  release: roll the images back.
- **Schema:** the migrations that contract (rename, drop, retype) and what they remove.
- **Values:** values added, renamed or removed, and what to change in `values.yaml`.
- **OpenShell:** the pinned version, and whether it changed (a change recreates every sandbox over time).
- **Other manual steps:** anything an operator must do before or after.
```

For a release before v0.3.0, open the section with this notice, then the list above:

```markdown
Until v0.3.0 there is no schema or upgrade promise (ADR 0099). An upgrade, a patch upgrade
included, may need a reinstall or the manual steps below. Backups of the database and the encryption
keys, and restoring from them, are supported: take both before you upgrade.
```

For `v0.1.0` and `v0.2.0` use the notice above. v0.1.0 is a first release, so its "Upgrading from"
section says there is nothing to upgrade from and keeps the notice. Name the restore runbook only if
it exists on `main` when you publish the notes; otherwise link
[Upgrading Koinon](https://koinon.stateracloud.com/docs/operate/upgrading/#roll-back). For `v0.3.0`, write that
it is the compatibility baseline, and how an earlier install reaches it (stop, migrate, start, or a
reinstall) and that it cannot roll back to an earlier release.

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

## Mirror a release to a registry

`hack/mirror-release.sh <version> <registry>` copies a release's images and chart to another
registry, for example to test an rc from a registry your cluster can reach. Install the mirrored chart with
`helm install koinon oci://<registry>/charts/koinon --version <version> --set image.registry=<registry>`.

### The flavor image when installing a release

The packaged chart defaults `flavors.opencode.image` to `<image.registry>/flavors/opencode:<appVersion>`,
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
(`flavors` in `ci.yml` is required but runs only in the merge queue and on main, and only when flavor files change; see `hack/ci-required.txt`), and CI
builds only the runner's own platform, so the release's QEMU build of the other platform is the
first one: dry-run it on an x86 host (where arm64 is emulated, as in the release) before an rc that
changes `flavors/`.

## Not released yet

- Signatures and attestations (the `sign` job), until `RELEASE_SIGNING` is switched on (when the repository goes
  public).
- Binary archives and checksums; the release ships images and the chart.
- A "local checks recorded" gate in place of the CI status gate (`hack/release-gate.sh`).
