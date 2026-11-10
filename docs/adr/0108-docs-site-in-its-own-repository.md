# 0108. The docs site lives in its own repository; this repository owns the tested commands

- Status: Accepted (maintainer decisions of 2026-11-10, issue #491)
- Date: 2026-11-10
- Supersedes in part: [0037](0037-tooling.md) (the docs-site decision: Starlight in `docs/site/`; Starlight itself stays)
- Relates to: [0085](0085-upgrade-compatibility-and-recovery-policy.md) (decision 18, backup and restore), [0105](0105-merge-queue-route.md)
  (the merge queue's `quickstart` is the e2e proof)
- Issue: #491

## Context

The documentation site (Astro Starlight, `docs/site/`) lived in this repository. Its quickstart and backup
pages hold shell commands that the end-to-end test ran by reading the pages (`hack/docs-e1.sh`), so the docs
could not drift from the test. A site in the code repository also meant a `docs` CI job, a Node toolchain and
page edits in code pull requests. The maintainer wants the site on its own cadence, public and on Cloudflare,
without making this repository wait for it.

## Decision

1. **The site moves to `stateracloud/koinon-docs`** (public). Generator: Astro Starlight stays. All pages of
   `docs/site/src/content/docs/` and the three operator documents of `docs/ops/` move there.
2. **Deploy:** a Cloudflare Worker with static assets, built by Cloudflare's GitHub integration (no API
   token), at `koinon.stateracloud.com`. A small home page at `/`; all docs under `/docs/`. Public, with a
   "pre-release" label, hidden from search engines until the maintainer says go. Theme: the Koinon design
   system.
3. **One version** (the newest release) until v0.3; a version picker comes at v0.3.
4. **ADRs are not published on the site.** The Contribute page links to them.
5. **This repository owns the tested commands.** They are plain command files, `test/e2e/commands/quickstart.e1`
   and `test/e2e/commands/backup-restore.e1`: SPDX header, then blocks that start with a `# e1=<name>`
   line. `test/e2e/run.sh` and `test/e2e/backup-restore.sh` run them by name through `hack/docs-e1.sh`; they no
   longer read a docs page. A file can be fetched at a release tag with one raw-file request.
6. **The docs repository checks its pages against the files.** Its CI fetches the two files at the release
   named in its `koinon.ref` and fails when a page's `sh e1=<name>` blocks differ. This repository never waits
   for the docs repository. Until step 7, `hack/docs-check.sh` runs the same comparison here, in the other
   direction: the page must equal the file.
7. **Removal.** A follow-up pull request removes `docs/site/`, `make docs`, the `docs` CI job and its path
   filters, and updates links; the checks that need this source tree (chart values, Secret manifests, UI
   labels, prerequisite pins) get a new home in that pull request.
8. **Reference pages** (generated) come from the release job here via a bot pull request in `koinon-docs`;
   later work, tracked on #491.

## Alternatives considered

- **Keep the command blocks only in the pages, fetched by the test** (pages stay the source): the e2e test
  would depend on another repository being reachable and in step; main would wait for the docs. Rejected.
- **Generate the pages' blocks from the command files at the docs build:** the site would need this
  repository at build time. A check in docs CI gives the same safety with a smaller coupling, and a page
  can still carry prose around a command. Revisit if drift shows up in practice.
- **One command file for both pages:** simpler to fetch, but the backup guide and the quickstart change for
  different reasons and are compared with different pages. One file per page keeps each comparison local.
- **Name the files `*.sh`:** shellcheck and the bash 5 guard would then treat them as scripts, and the
  generated-file exclusion lives in `.github/`. The `.e1` name says what they are and needs no exclusion;
  `hack/docs-check.sh` still runs `bash -n` on every block.

## Consequences

- Positive: the e2e test has no dependency on a docs page; the docs site can change and deploy without a
  code pull request; the commands have one owner.
- Negative: two places show a command (the file and the page); the check in `koinon-docs` is the only guard
  once `docs/site/` is gone, and it compares with the newest release, so a command changed on `main` reaches
  the pages with the next release.
- Follow-ups: part 2 of #491 (removal and links); the install writer's log hint
  (`docs/ops/secret-keys.md`) must name the page address; the developer part of the prices document moves to
  `docs/dev/`.
