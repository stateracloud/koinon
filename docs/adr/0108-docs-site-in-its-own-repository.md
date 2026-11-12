# 0108. The docs site lives in its own repository; this repository owns the tested commands

- Status: Accepted (maintainer decisions of 2026-11-10, issue #491); decision 9 amended 2026-11-11 (below, issue #127)
- Date: 2026-11-10
- Area: engineering-process
- Tags: docs, e2e
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
   for the docs repository. Between the move (part 1) and the removal (step 7), `hack/docs-check.sh` ran the
   same comparison here, in the other direction; it is gone with the pages.
7. **Removal (part 2 of #491, done).** One pull request removed `docs/site/`, `docs/ops/`, `make docs`,
   `hack/docs-check.sh`, and the `docs` and `warm-caches-docs` CI jobs and their path filters. The `main`
   ruleset still lists the `docs` job as a required status check; a pull request cannot change a ruleset, so the
   maintainer removes that required context before the merge. Links now point to the pages on the site. What stays
   here is `hack/e2e-commands-check.sh` (`make e2e-commands-check`, run by the `scripts` job): the command
   files parse and the e2e scripts run exactly their blocks. The page-side checks left with the pages and
   are ported to `koinon-docs`: `helm lint` of the `chart-values` blocks, the Go parse of the
   `model-provider` and `mcp-server` Secret blocks, the UI labels against the Playwright spec, and the
   prerequisite versions against the pins. The manifests of the e2e test and the dev profiles are still
   parsed here (`internal/declarative`). The developer part of the prices document moved to
   `docs/dev/prices.md`; the install writer's log hint and the other messages name the page address
   (`internal/project`).
8. **What this repository provides to `koinon-docs`.** Two things, both at a release tag: the command files
   `test/e2e/commands/quickstart.e1` and `test/e2e/commands/backup-restore.e1`, and the page-manifest tests
   `TestDocumentedSecretsParse` and `TestDocumentedMCPServerSecretsParse` in `internal/declarative/pages_test.go`.
   The tests read the pages in the directory named by `KOINON_PAGES_DIR`, skip when it is not set, and
   fail when the directory is missing, when it holds fewer than two `model-provider` blocks or no `mcp-server`
   block, or when a block does not parse. Names and variable are a contract: change them only with a change in
   `koinon-docs`.
9. **Reference pages** (generated) come from the release job here via a bot pull request in `koinon-docs`;
   later work, tracked on #505.
   _(Changed 2026-11-11, see the amendment below (issue #127): the generated reference is committed here under `docs/reference/proto/` and `koinon-docs` reads it at a release tag; no bot pull request.)_

## Alternatives considered

- **Keep the command blocks only in the pages, fetched by the test** (pages stay the source): the e2e test
  would depend on another repository being reachable and in step; main would wait for the docs. Rejected.
- **Generate the pages' blocks from the command files at the docs build:** the site would need this
  repository at build time. A check in docs CI gives the same safety with a smaller coupling, and a page
  can still carry prose around a command. Revisit if drift shows up in practice.
- **One command file for both pages:** simpler to fetch, but the backup guide and the quickstart change for
  different reasons and are compared with different pages. One file per page keeps each comparison local.
- **Name the files `*.sh`:** shellcheck and the bash 5 guard would then treat them as scripts, and a block
  is not a whole script. The `.e1` name says what they are and needs no exclusion;
  `hack/e2e-commands-check.sh` still runs `bash -n` on every block.

## Consequences

- Positive: the e2e test has no dependency on a docs page; the docs site can change and deploy without a
  code pull request; the commands have one owner.
- Negative: two places show a command (the file and the page); the check in `koinon-docs` is the only guard
  once `docs/site/` is gone, and it compares with the newest release, so a command changed on `main` reaches
  the pages with the next release.
- Follow-ups: none for this repository. The page-side checks and the reference pages are tracked on #505
  for `koinon-docs`. Older ADRs still name `docs/ops/` and `docs/site/` paths; those files moved to
  `koinon-docs` (decision 1) and the ADR text is left as written.
- The `docs/site` Dependabot follow-up of ADR 0079 is void. Links in older ADRs (0056, 0085, 0098) to
  `docs/ops/` pages now point at pages on the docs site: `docs/ops/sandbox-break-glass.md` is
  `https://koinon.stateracloud.com/docs/operate/sandbox-break-glass/`, `docs/ops/secret-keys.md` is
  `https://koinon.stateracloud.com/docs/operate/secret-keys/` and the prices page is
  `https://koinon.stateracloud.com/docs/operate/default-prices/`.

## Amendment 2026-11-11: the generated reference is committed here (issue #127)

Decision 9 names a bot pull request from the release job. That is not how the proto reference reaches
`koinon-docs`. This repository commits the generated reference under `docs/reference/proto/`
(`proto-api.json` and `proto-api.md`, written by `make generate`; CI's no-diff check keeps it current), and
`koinon-docs` reads it at a release tag (`stateracloud/koinon-docs#6`). No bot pull request is needed. The
generator is `cmd/protoc-gen-koinon-doc`, owned here; the JSON layout is in `docs/reference/README.md`.
