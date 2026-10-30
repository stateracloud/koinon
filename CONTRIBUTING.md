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

## Requirements

- Go (version in `go.mod`), Docker (for Postgres tests)
- Node 24 and pnpm (version in `web/package.json`)
- [buf](https://buf.build/docs/installation), [golangci-lint](https://golangci-lint.run) v2
- For the inner loop: [Tilt](https://tilt.dev), [ko](https://ko.build) and a Kubernetes cluster

Code generators (protoc-gen-go, protoc-gen-connect-go, sqlc, goose) are pinned as Go tools in
`go.mod` and run through `go tool`.

## Common tasks

```sh
make web-deps   # install web dependencies (once)
make build      # build Go binaries and the web UI
make test       # Go and web unit tests
make lint       # golangci-lint, oxlint, tsc, buf lint, migration lint
make generate   # regenerate protobuf and sqlc code; commit the result
make breaking   # buf breaking against main
make dev        # Tilt against the home lab, or kind after `make dev-kind-up` (docs/dev/loop.md)
```

## Pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and PR titles.
- These checks must pass: Go lint and tests, web lint, typecheck and tests, `buf lint` and
  `buf breaking`, generated code up to date, migration lint, DCO, and the gitleaks secret scan.
- `main` is protected by a ruleset: changes land only through pull requests, with the checks
  above passing and every review thread resolved; force-pushes and deletion are blocked, with no
  bypass. Merge with a merge commit (keeps stacked pull requests intact) or squash; rebase merges
  are off. CODEOWNERS requests the other maintainer's review; approval is required on sensitive
  paths (workflows, ADRs, migrations, auth, gateway, secret store, sandbox policy).
- Every pull request names its issue with `Closes #N`, and its author holds the claim on that issue
  (`hack/claim.sh take N`; [ADR 0082](docs/adr/0082-work-claims-and-ready-queue.md)). The `claim`
  check enforces it; `chore`, `ci` and `deps` titles, Dependabot and the label `unclaimed-ok` are
  exempt. With strict checks, keep the branch up to date with `main`; the updater bot also does it
  for auto-merge pull requests.
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

- Dependabot (`cooldown`) and pnpm (`web/pnpm-workspace.yaml`, strict) enforce it for the bot and for
  `pnpm add`. The `dependency age` CI check (`hack/dep-age-check.sh`) checks the rest on every pull
  request. You can run it locally: `hack/dep-age-check.sh origin/main`. Publication dates are a guard
  against accidents, not proof of safety.
- Security fixes may skip the wait, with checks: a reviewed advisory names the fix (take the lowest
  fixing version), it is reachable (`govulncheck`) or high, critical or in CISA's KEV list, the
  publication is verified (npm provenance, a signature or upstream checksum, a genuinely new tag for
  an Action), and a human reads the diff and records that in the pull request. A pnpm exemption lists
  one exact version (`pkg@1.2.3`) with a comment naming the advisory and the date.
- Anything else younger than 3 days needs a waiver from the maintainer, recorded in the pull
  request (who decided, why, what was verified), and the `dependency-age-waiver` label. Re-run the
  `dependency age` job after labelling.
