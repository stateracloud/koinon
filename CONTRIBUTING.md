# Contributing to Koinon

Thanks for your interest. Koinon is pre-alpha, so expect things to move. Open an issue before large
changes, and read the relevant ADRs in [`docs/adr/`](docs/adr/) first.

## Developer Certificate of Origin

Every commit must be signed off under the [Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s -m "feat(api): add org lookup"
```

The sign-off must match the commit author. CI rejects pull requests with unsigned commits; fix them
with `git rebase --signoff main`. There is no CLA.

Author and committer of every commit (merge commits too) must appear in `hack/git-identities.allowed`
(`Name <email>` globs; GitHub noreply addresses are in it). CI checks the pull request's commits against the
list on the pull request's base (`main`), so a pull request cannot widen its own list: add your own line in
your first pull request (a local hook run uses `KOINON_ALLOWED_IDENTITIES` meanwhile) and it counts from
the next one on. Agent worktrees made by `hack/wt.sh new` also refuse a
commit under another identity (`hack/git-identity-check.sh install` does it for any worktree).
The hooks set `core.hooksPath`, which replaces `.git/hooks` in that worktree (a global `core.hooksPath`
is overridden too, so a hook kept there should go in `.git/hooks` to keep running); they chain to a hook of the
same name in `.git/hooks` (`pre-commit`, `pre-merge-commit`), so a personal one such as a local gitleaks
hook still runs. The hooks do not run on `--no-verify`, rebase or cherry-pick; CI checks every commit.
`hack/ship.sh` ignores `KOINON_ALLOWED_IDENTITIES` and `KOINON_SKIP_IDENTITY_CHECK`.

## AI-assisted contributions

You may use AI tools. The rules are the same as for any contribution:

- The human author signs off the commits (DCO above) and is responsible for the change: you must
  understand it, have run it, and be able to answer review questions about it.
- Disclose the tool with a `Co-Authored-By:` trailer on the commit. Never add an AI as
  `Signed-off-by`: only a person can certify the DCO.
- Maintainers may close a pull request that shows no understanding of the change, however
  well-formed it looks.

## Requirements

You need nothing from the maintainers' infrastructure: no registry login, no model
key. Everything below runs on your machine with anonymous image pulls. Versions are the ones CI
uses; newer patch releases usually work, but if something differs from CI, match this table first.

| Tool | Version | Used for |
| --- | --- | --- |
| Go | `go.mod` (1.27.3) | build, tests, code generators (pinned as `go tool` entries) |
| Docker | any recent, with buildx | Postgres tests (testcontainers), flavor images, kind |
| Node | 24 | web UI |
| pnpm | `web/package.json` `packageManager` (12.18.0) | web UI |
| buf | current v1 (CI: `bufbuild/buf-action`) | `make lint-proto`, `make generate` |
| golangci-lint | v2.15.0 | `make lint-go` |
| Helm | v4.3.1 | `make chart-check`, admission tests, Tilt |
| kind | v0.33.0 (Kubernetes 1.37.0) | `make dev-kind-up`, `make test-e2e-kind` |
| kubectl | within one minor of 1.37 | kind loop, e2e |
| ko | 0.19 or newer (CI: 0.19.1) | Koinon images for kind and Tilt |
| Tilt | current | `make dev` |
| gitleaks | 8.30.1 | secret scan before you push |
| jq | any | e2e and kind scripts |
| bash | 5 or newer (macOS ships 3.2: `brew install bash`, ADR 0090) | every script in `hack/` and `test/`; they exit 2 on an older bash |

Code generators (protoc-gen-go, protoc-gen-connect-go, sqlc, goose, oapi-codegen) are pinned as Go
tools in `go.mod` and run through `go tool`. `setup-envtest` for the admission tests is fetched by
`make test-admission`.

Tool versions live in `hack/versions.env`: kind, ko, Helm, kubectl, gitleaks, shellcheck, actionlint,
goreleaser, cosign, kubeconform and golangci-lint, with the sha256 of each release asset the e2e image
or a workflow downloads (Go stays in `go.mod`). Scripts source the file and workflows load it into the
job environment; the copies that cannot (the `hack/e2e-toolchain/Dockerfile` ARGs, the `version:` input
of `setup-helm` and `golangci-lint-action`, the table above) are compared by `hack/versions-check.sh`,
which runs in `hack/preflight.sh` and in CI. To bump a tool: edit the file (version and checksums, a
release at least 3 days old, ADR 0079), run `hack/versions-check.sh`, and update the copies it names.
`hack/dep-age-check.sh` dates the new release.

## Common tasks

```sh
make web-deps   # install web dependencies (once; run it before make generate and make lint)
make build      # build Go binaries and the web UI
make test       # Go and web unit tests, the same set CI runs (without test/flavors)
make test-flavors # build the flavor images, then run the flavor contract suite (Docker)
make lint       # golangci-lint, oxlint, tsc, buf lint, migration lint
make generate   # regenerate protobuf and sqlc code; commit the result
make breaking   # buf breaking against main
make dev-kind-up # local kind cluster with Koinon's prerequisites (once)
make dev        # Tilt on that cluster (docs/dev/loop.md)
make test-escape-kind # the sandbox escape suite's OpenShell cases on that cluster
```

The local development loop is kind: `make dev-kind-up`, then `make dev` (Tilt), and
`make test-e2e-kind` for the quickstart end to end. [docs/dev/loop.md](docs/dev/loop.md) covers it,
including how to point Tilt at a remote cluster of your own.

## Pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and PR titles.
- These checks must pass: Go lint and tests, web lint, typecheck and tests, `buf lint` and
  `buf breaking`, generated code up to date, migration lint, DCO, and the gitleaks secret scan.
  The full list of required checks is `hack/ci-required.txt` (it also covers the docs build, the
  chart smoke test, the dependency age check and the script tests); a check that does not apply to
  your change reports success. `flavors` is the only advisory job on pull requests (`flavor-publish` is advisory and push-only). `hack/ci-required-test.sh` keeps
  the list and `ci.yml` in step.
- `main` is protected by a ruleset: changes land only through pull requests, with the checks
  above passing and every review thread resolved; force-pushes and deletion are blocked, with no
  bypass. Merge with a merge commit (keeps stacked pull requests intact); squash and rebase merges
  are off. CODEOWNERS requests the other maintainer's review; approval is required on sensitive
  paths (workflows, ADRs, migrations, auth, gateway, secret store, sandbox policy).
- A pull request names the issue it closes with `Closes #N`. A maintainer's agent claims the issue
  first with `hack/claim.sh take N`, so no other lane starts on it; the claim ends when the pull
  request merges, or with `hack/claim.sh release N` (or `release-pr <PR>` after a close without a merge)
  ([ADR 0082](docs/adr/0082-work-claims-and-ready-queue.md),
  [ADR 0104](docs/adr/0104-work-claims-reduced-to-the-lock.md)). Outside contributors do not claim:
  say on the issue that you are working on it. The `claim` check fails only when another person
  holds a claim on an issue the pull request closes, or reserved the number of an ADR it adds; it
  names the holder and what to do. There is no updater bot: merge `main` into
  your branch only when GitHub reports a conflict (the merge queue tests the pull request on main's tip). For an `--admin` merge, merge
  `origin/main` and wait for green first.
- ADR numbers are unique and contiguous (`hack/adr-lint.sh`): if yours collides, renumber it.
- Dependabot opens one grouped update per ecosystem (Go, web, GitHub Actions, Docker) each Monday
  (see [Dependency updates](#dependency-updates)); security updates arrive as soon as they are
  published.
- Migrations are forward-only: add a new `NNNNN_name.sql` file; never edit a merged one.
- Decisions that change architecture get an ADR (copy `docs/adr/0000-template.md`).

## Dependency updates

No dependency version enters Koinon until it has been published for at least 3 days. That covers Go
modules, npm packages (including transitive ones), GitHub Actions, container images, release
binaries, Helm dependencies and the pinned OpenShell and Agent Sandbox versions, major versions
included. [ADR 0079](docs/adr/0079-dependency-cooldown.md) has the reasoning and the exact rules.

- Dependabot (`cooldown`) and pnpm (`web/pnpm-workspace.yaml`, strict;
  `bootConfig` extends it to the pnpm binary that `packageManager` selects) enforce it for the bot and for
  `pnpm add`. The dependency age steps of the `quick checks` CI job (`hack/dep-age-check.sh`) check the rest on every pull
  request. You can run it locally: `hack/dep-age-check.sh origin/main`. Publication dates are a guard
  against accidents, not proof of safety.
- Security fixes may skip the wait, with checks: a reviewed advisory names the fix (take the lowest
  fixing version), it is reachable (`govulncheck`) or high, critical or in CISA's KEV list, the
  publication is verified (npm provenance, a signature or upstream checksum, a genuinely new tag for
  an Action), and a human reads the diff and records that in the pull request. A pnpm exemption lists
  one exact version (`pkg@1.2.3`) with a comment naming the advisory and the date.
- Anything else younger than 3 days needs a waiver from the maintainer, recorded in the pull
  request (who decided, why, what was verified), and the `dependency-age-waiver` label. Re-run the
  `quick checks` job after labelling.
