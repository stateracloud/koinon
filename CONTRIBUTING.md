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
make dev        # Tilt against the home-lab cluster (docs/dev/)
```

## Pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and PR titles.
- These checks must pass: Go lint and tests, web lint, typecheck and tests, `buf lint` and
  `buf breaking`, generated code up to date, migration lint, DCO, and the gitleaks secret scan.
- `main` is protected by a ruleset: changes land only through pull requests, with the checks
  above passing and every review thread resolved; force-pushes and deletion are blocked, with no
  bypass. Merge with a merge commit (keeps stacked pull requests intact) or squash; rebase merges
  are off. CODEOWNERS requests the maintainer's review on every pull request.
- Dependabot opens one grouped update per ecosystem (Go, web, GitHub Actions) each Monday;
  security updates arrive as soon as they are published.
- Migrations are forward-only: add a new `NNNNN_name.sql` file; never edit a merged one.
- Decisions that change architecture get an ADR (copy `docs/adr/0000-template.md`).
