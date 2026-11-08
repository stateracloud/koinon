<!-- SPDX-License-Identifier: Apache-2.0 -->

# Mutation testing spike (issue 401), November 2026

A spike, not a design: it measures what avito-tech/go-mutesting does on Koinon's code and leaves the
concrete CI job to an ADR. Nothing here is part of the required CI.

**Short answer.** Running mutants through `go test -overlay` works with Ginkgo and with the
testcontainers Postgres specs. But go-mutesting cannot produce the pilot bug's mutant: its
`statement/remove` never touches the body of a `select` arm. On the pilot lines the tests at HEAD
leave 2 real gaps (both in `Importer.Run`) and 6 noise survivors. The baseline on the allow-list was
**not run here**: it ran on the maintainer's Mac, where heavy test loads must not run. The command is
below.

## What was committed

- `tools/mutation/go.mod`: its own module, so the tool never enters the root `go.mod`. It pins
  `github.com/avito-tech/go-mutesting v0.0.0-20261002081621-9ce5f731e321`. The fork has no tags;
  that commit is from 2026-10-02, 37 days before this spike, which satisfies ADR 0079's 3 days. It
  builds with Go 1.27.3.
- `hack/mutation.sh`: the driver. go-mutesting only generates the mutants (`--no-exec`). The driver
  runs each one with `go test -vet=off -overlay=<json> -count=1 -timeout <3x clean run>`, at most
  `--max-procs` at a time (default 2). Flags:
  - `--packages`, `--diff <base>` (only lines changed since `git merge-base <base> HEAD`, working
    tree included) and `--lines FILE:A-B`.
  - `--replace F=SRC`, which overlays another version of a file, for example an older test file.
  - `--disable <mutator>` and `--dry-run`.
  - Postgres specs share one server (`hack/pgserver`, `KOINON_PGTEST_DSN`, one testcontainers
    session), as in `hack/go-test.sh`.

  Output goes to `results.tsv` and `survivors/*.diff`, with one summary line per package. The exit
  code is 1 when any mutant survived. Generated files (`// Code generated ... DO NOT EDIT.`) are
  skipped.

## Pilot: the deleted `backoff = first` in `Importer.Run`

The bug fixed by e48a3cc83 was a missing reset of the backoff in the `case <-wake:` arm of the
`select` in `internal/declarative/declarative.go` (line 181 at HEAD, line 179 at 248ca4295).

| Question | Result |
|---|---|
| Does go-mutesting generate the mutant? | **No.** `statement/remove` only walks `*ast.BlockStmt` and `*ast.CaseClause` bodies (`mutator/statement/remove.go`). A `select` arm is an `*ast.CommClause`, so no statement in any `select` arm is ever removed. No mutator at all touched lines 179-183 except the `time.Sleep(200 * time.Millisecond)` constant. |
| The same mutant, built by hand (`_, _ = backoff, first`, exactly what `statement/remove` emits), run through the driver's overlay path against HEAD's tests | **KILLED** (1 run, 20 s). It fails "Run backoff requests a doubling wait, none once it reaches the resync interval, and starts over on a Secret event". |
| The same mutant against the tests before the fix (248ca4295 = bbdb7747 for `run_test.go`, with 248ca4295's `declarative.go`) | **Survives by construction, not shown by a run.** The old spec ends with a clean pass (which resets the backoff in its `else` branch) and only then sends a Secret event, so the wake reset is never needed. The run could not decide it, because the old spec is flaky against its own unmutated code. It raced a real 50 ms `Resync` ticker, and went red in 2 of 2 runs here: first at `run_test.go:96` (`Consistently ... ShouldNot(Receive())`), then, with that line removed, at `:106` (a stale 50 ms request). A red clean run gives no verdict, and the driver refuses one. |
| The old tests against HEAD's code | Red: they assert the pre-fix behaviour (a retry at the cap), as expected. |

What this shows:

1. The overlay path classifies this kind of bug correctly once the mutant exists.
2. The tool as pinned would have missed it, because nothing mutates the arms of a `select`. Before
   any gate, `statement/remove` (and `branch/case`) must handle `*ast.CommClause`. That is a
   two-line change in `MutatorRemoveStatement`: `case *ast.CommClause: l = n.Body`.
3. Flaky, wall-clock specs block mutation testing outright. e48a3cc83's injected `after` with exact
   durations is what made the mutant decidable.

### Pilot run on lines 176-203 at HEAD

`hack/mutation.sh --packages ./internal/declarative --lines internal/declarative/declarative.go:176-203 --max-procs 4`
(no shared Postgres yet in that run): 193 mutants generated in `declarative.go`, 17 in range.
Results: 8 killed, 8 survived, 1 error (it does not build: `declared and not used: waiting`).

| Line | Mutation | Verdict |
|---|---|---|
| 197 | `branch/case`: the body of `case !waiting && err == nil:` (`backoff = first`) removed | **(a) real gap.** It mirrors the pilot bug. No spec checks that, after a clean pass and with no Secret event in between, the next org wait starts at `first` again. The spec's Secret event resets the backoff through the wake arm, which hides this reset. |
| 194 | `statement/remove`: `retry = nil` removed | **(a) real gap, minor.** No spec checks that a retry still pending from before a wake- or tick-triggered pass is dropped when that pass is clean. If it is kept, the stale timer later fires an extra pass. |
| 182 | `numbers/incrementer`, `numbers/decrementer`: `200` becomes `201` or `199` ms | (b) equivalent: the coalescing window's exact length is not behaviour. |
| 182 | `arithmetic/base`: `200 * time.Millisecond` becomes `200 / time.Millisecond` (no sleep) | (c) noise. The coalescing of an event burst is a performance property; a test would assert N events give 1 pass, which is timing-bound. A waiver candidate. |
| 191 | `expression/remove` twice: `err != nil && ctx.Err() == nil` loses one operand | (c) noise: it only decides whether to log. |
| 192 | `branch/if`: the `Error` log call removed | (c) noise: logging. |

Real gaps by package: `internal/declarative` 2 (`declarative.go:197`, `declarative.go:194`).
Nothing else was mutated in this spike.

### Timing (Mac, warm build cache, 4 procs; Docker 29.8.2 via OrbStack was available)

- Clean run of `./internal/declarative`: 21 s with a container per test process, 15 s with the
  shared `pgserver`.
- Generation is the surprise: about **70-117 s per file**, serial. go-mutesting type-checks every
  file with a fresh package load (client-go and all its dependencies). The whole package (3 files,
  299 mutants) took 4 min 14 s; `declarative.go` alone took 117 s.
- Per mutant: 14-61 s, about equal to the package's test time (the slow ones are specs that hit
  their `Eventually` timeout). With 4 procs, 17 mutants took 139 s, i.e. about 8 s of wall clock
  per mutant.

## Baseline on the allow-list: not run here

Packages, resolved from the brief:

| Area | Package | Non-test files / lines |
|---|---|---|
| leak guard | `./internal/leakguard` | 9 / 3342 |
| declarative import | `./internal/declarative` | 3 / 911 |
| auth | `./internal/auth`, `./internal/otlpauth` | 8 / 1780, 1 / 224 |
| secretstore core | `./internal/secretstore`, `./internal/secretstore/wrapping` | 4 / 1148, 1 / 164 |
| ledger and pricing | `./internal/ledger`, `./internal/prices`, `./internal/providerusage` | 8 / 1961, 9 / 2531, 3 / 666 |
| gateway retry and guard | `./internal/gateway` (`retry.go`, `guard.go`, plus `gateway.go`, `headers.go`, `formats.go`) | 5 / 1251 |

Excluded:

- the cloud KMS adapters `secretstore/awskms`, `secretstore/openbao` and `secretstore/remotekms`;
- the test helpers `auth/oidctest`, `secretstore/secretstoretest` and `gateway/gatewaytest`;
- all of `internal/gen/...`.

There is no `internal/usage`: the ledger's pricing lives in `internal/ledger/price.go` and
`internal/prices`. `internal/mcpguard`, the MCP tool guard rather than the gateway's guard, is a
candidate for a later round.

Command for a cloud runner (Docker required; set `--max-procs` to about half the cores, since each
mutant's `go test` is itself parallel):

```sh
KUBECONFIG=/dev/null bash hack/mutation.sh --max-procs 8 --out "$PWD/mutation-baseline" --packages \
  "./internal/leakguard ./internal/declarative ./internal/auth ./internal/otlpauth ./internal/secretstore ./internal/secretstore/wrapping ./internal/ledger ./internal/prices ./internal/providerusage ./internal/gateway"
```

Results table, to fill from the summary lines (mutants generated / tested, killed, survived,
timeout, error, clean and wall time, cores):

| Package | Mutants | Killed | Survived | Timeout | Error | Wall | Cores |
|---|---|---|---|---|---|---|---|
| (not run here) | | | | | | | |

Triage each survivor as in the pilot table: (a) real gap with the missing assertion, (b)
equivalent, (c) noise (logging, metrics labels, error text).

### Extrapolation

The pilot density was 0.33 mutants per line (299 / 911). The allow-list has 51 files and 13,978
lines, so about **4,600 mutants**.

- **Generation:** 51 files at about 90 s is about 77 min serial. Files are independent, so a few
  go-mutesting processes in parallel cut that; the current driver generates one package at a time.
- **Tests:** about 4,600 mutants at about 20 s each over 8 procs is roughly **3 h** of wall clock
  on an 8-to-16-core runner, before the slower suites (`ledger` and `prices` hit Postgres hard).
- **Nightly job:** feasible only sharded per package across runners, or on a rotation (a few
  packages a night). A full run on one runner is a 3-5 h job.
- **Per-PR diff job:** a typical PR touches 1-3 allow-listed files and about 50-150 changed lines.
  That is 15-50 mutants: 2-5 min to generate (only the touched files) plus 3-5 min of tests at 4
  procs, so **about 10 min**.

## Problems hit

1. **No mutation of `select` arms** (above): the main gap in the tool for Koinon's concurrent
   code.
2. **The tool's executor edits the source tree in place** and runs `go test` serially: it renames
   the file, copies the mutant over it and restores it in a defer. A crash leaves a mutated tree,
   and two runs in one worktree corrupt each other. Its `--exec` hook (`MUTATE_CHANGED`,
   `MUTATE_ORIGINAL`, `MUTATE_PACKAGE`) runs one mutant at a time too. Hence `--no-exec`, with the
   driver doing the execution through `-overlay`.
3. **Ginkgo under `go test -overlay`: works.** The overlay replaced both a source file and a
   `_test.go` file; the suite ran all 45 specs. No Ginkgo CLI or `-p` is needed: the parallelism
   is across mutants.
4. **`go test` exits 1 both for a failing test and for a build failure**, so the driver tells them
   apart by `[build failed]`, `[setup failed]` and `# pkg` in the output. 1 in 17 mutants did not
   build (`declared and not used`).
5. **vet**: `go test` runs vet; a mutant that only vet rejects would count as killed. The driver
   uses `-vet=off`.
6. **Flaky specs**: see the pilot. A real-clock spec makes its package unusable for mutation
   testing. The driver stops on a red clean run instead of counting every mutant as killed.
7. **Postgres**: without `KOINON_PGTEST_DSN`, every mutant's test binary starts its own container.
   The shared `pgserver` saves the start-up time and keeps Ryuk alive for the whole run.
8. **Generation speed**: a full type-check per file (above).
9. **Waiver syntax**: `// mutator-disable-next-line statement/remove` (a comma list, or `*`). The
   text after the marker is parsed as mutator names, so a reason on the same line breaks the match:
   the reason has to go on its own comment line directly above it.

## Recommended design for the ADR

- **Generator:** go-mutesting, used only to generate mutants, pinned in `tools/mutation`. First,
  upstream support for `*ast.CommClause` in `statement/remove` and `branch/case`. Until that is
  merged and 3 days old, pin a fork commit with a `replace` in `tools/mutation/go.mod`, with an
  issue whose exit condition is the upstream release. A Koinon-owned generator (go/ast plus
  go/printer, a few hundred lines, also fixing the per-file type-check cost) is the fallback if
  upstream does not move.
- **Executor:** `hack/mutation.sh`, never the tool's: overlay, parallel, a shared Postgres, a red
  clean run aborts.
- **Per-PR gate:**
  - `hack/mutation.sh --diff origin/main`, restricted to the allow-list packages.
  - It fails on any surviving mutant on a changed line that has no waiver. There is no score.
  - It is advisory, listed in `hack/ci-required.txt` as such, until the nightly runs have triaged
    the baseline; then it becomes required.
- **Nightly:** the full allow-list, sharded per package (a matrix job). The survivors list is
  uploaded, and one issue per package is refreshed with them. Nothing gates on a score.
- **Waivers:** `// mutator-disable-next-line <mutator>` with a reason line directly above,
  checked by a lint (every waiver has a reason). Logging and coalescing constants, like the pilot's
  noise, are the expected waivers.
- **Before turning a package on:** its specs must be deterministic under an injected clock. A
  flaky package stays off the list until it is.
