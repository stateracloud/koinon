# 0118. Logic in hack/ moves to Go programs that run with go tool

- Status: Accepted
- Date: 2026-11-16
- Area: engineering-process
- Tags: tooling, ci
- Relates to: [0079](0079-dependency-cooldown.md), [0090](0090-scripts-require-bash-5.md)
- Issue: #1040

## Context

Much of `hack/` is logic written in bash and awk. Some scripts parse JSON from `gh`, YAML or
Dockerfiles. Parsing bugs in these scripts come back: see #954 and the Dockerfile lint in PR #1034.
A shell script has no real parser, no typed data and no unit test framework. We have 78 shell tests,
and 33 of them need PATH shims or fakes.

The maintainer asked whether a task runner such as mage fixes this. The research of 2026-11-16
(`~/.config/koinon/research/2026-11-16-mage-and-go-tooling.md`, prior art) compared mage, plain Go
programs, `go tool`, Task, just and the status quo. Its result: the gain comes from the language, not
from a task runner. Make is already the task runner and the front door.

Facts that decide this (checked 2026-11-16):

- Go 1.24 added `tool` directives to `go.mod` (<https://go.dev/doc/go1.24>). A tool can be a package
  of the main module (`go get -tool ./hack/cmd/<name>`). `go.mod` of Koinon already lists eight tools.
- `go tool <name>` passes the exit code of the tool through (`cmd/go/internal/tool/tool.go`).
  `go run` does not: its documentation says the exit status of `go run` is not the exit status of
  the program. `hack/go-test.sh` builds `hack/testverdict` for this reason (exit code 3 is part of
  the gate contract).
- Koinon already has Go programs in `hack/`: `testverdict`, `chartlint` and `pgserver`.

## Decision

1. **Logic goes in Go.** A script that parses data or holds a lot of logic becomes a Go program in
   `hack/cmd/<name>`. The program has Ginkgo unit tests beside it. It follows the exit codes of the
   script it replaces. A new program uses 0 for success, 1 for findings and 2 for a usage or read
   error, and 3 for a missing requirement where the gate contract needs it.
2. **Programs are main-module tools.** Declare each one with `go get -tool ./hack/cmd/<name>`. Run it
   with `go tool <name>`, not with `go run`, so the exit code reaches the caller.
3. **Bash 5 stays for glue.** Calling programs, wiring files and starting processes stay in bash
   ([ADR 0090](0090-scripts-require-bash-5.md)). Make stays the front door. We add no task runner.
4. **A ported script keeps its path as a thin wrapper.** The wrapper has the shebang, the bash 5
   guard, and a line that runs `go tool <name>` with the same arguments (it goes to the repository
   root first, because `go tool` finds the tool through the module of the working directory).
   So Make, `hack/paths.rules`, preflight and `ci.yml` do not change. The only change in
   `hack/paths.rules` is that the Go sources join the inputs of the gate.
5. **The old test stays.** The `*-test.sh` of a ported script keeps running against the wrapper and
   proves that the Go version behaves like the old one. A test may read a constant from the Go
   source instead of from the old script. The Go unit tests add the cases that are hard to write in
   shell.
6. **One script per pull request.** There is no big rewrite. Each port is its own issue.
7. **Candidates** are scripts of these kinds:
   - parse-heavy scripts: they read JSON, YAML, TOML, Dockerfiles or other structured text with
     awk, sed, grep or jq chains;
   - scripts with many fix commits: `git log --oneline -- <script>` shows repeated fixes for
     parsing or quoting;
   - scripts whose test needs many PATH shims, because a Go program takes the shim as an interface.

   Glue scripts are not candidates: scripts that only call other programs in order, wait, or set
   up the environment stay in bash.
8. **Dependencies follow [ADR 0079](0079-dependency-cooldown.md).** A port may import a library
   only when the release is at least 3 days old. `hack/dep-age-check.sh` already reads `go.mod`.
   Prefer the standard library, and prefer parsers that the upstream tool itself uses (for example
   the Dockerfile parser of BuildKit).
9. **Existing programs stay where they are.** `hack/testverdict`, `hack/chartlint` and
   `hack/pgserver` do not move now. They may move to `hack/cmd/` when a change touches them anyway.
   Their callers use `go run` or `go build` today; a change moves them to `go tool` then.
10. **First port: `hack/bash5-lint.sh`** becomes `hack/cmd/bash5lint`. It sets the pattern.
    Other ports are separate issues (see #1042).

## Alternatives considered

- **mage.** Rejected. The last release before 2026 is v1.15.0 (2023-05-11); v1.16.0 to v1.17.2
  followed in March and April 2026. One maintainer wrote 42 of the last 100 commits. Mage writes
  `mage_output_file.go` into the source directory on every run. Two runs in one directory collide
  (magefile/mage#247, open since 2019). Make already is our front door, so mage would be a second one.
- **Task and just.** Rejected. The logic stays in shell strings inside a YAML or text file, so the
  parsing bugs stay. Task runs commands in `mvdan.cc/sh`, not in bash 5. just is a Rust binary that
  needs a pinned download and a checksum.
- **`go run ./hack/<name>`.** Rejected as the default. It hides exit codes other than 0 and 1.
- **Keep bash and awk.** Rejected for parse-heavy scripts. It stays right for glue.

## Consequences

- Positive: real parsers and typed data; tests with `go test`; the exit-code contract stays; no new
  tool in the repository.
- Negative: a Go build runs on the first call of a tool (later calls use the Go build cache);
  CI jobs that run a wrapper need Go installed (the `scripts` job has it); each port costs time.
- Follow-ups: ports are separate issues. `CONTRIBUTING.md` says how to propose one.

## Upstream verification

- Go 1.24 release notes, tool directives: <https://go.dev/doc/go1.24>
- `go tool` exit status and `go run` exit status: `cmd/go/internal/tool/tool.go` and
  `cmd/go/internal/run/run.go` in Go 1.27.4.
- mage releases and issue 247: <https://github.com/magefile/mage/releases>,
  <https://github.com/magefile/mage/issues/247>
- Prior art in other projects: Trivy declares `tool` entries and runs mage on top; Hugo pins mage
  in `go.mod` but installs another version in CI. Gitea, Cilium and Dapr keep Make as the front door.
