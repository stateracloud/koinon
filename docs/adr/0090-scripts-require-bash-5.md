# 0090. Every repository script requires bash 5

- Status: Accepted; the lab as merge evidence (note of 2026-11-07 below) amended by [0105](0105-merge-queue-route.md)
- Date: 2026-11-02
- Area: engineering-process
- Tags: tooling, ci
- Relates to: [0079](0079-dependency-cooldown.md), [0082](0082-work-claims-and-ready-queue.md)
- Issue: old issue 553

## Context

macOS ships bash 3.2 as `/bin/bash`. For about 80 scripts we kept 3.2 compatibility by rule: no
`declare -A`, `mapfile` or `${v,,}`, empty arrays guarded under `set -u`, a variable in place of a
quote inside `"$(...)"`. The rule cost more than it paid. Two Mac-only bugs on 2026-11-02 came from it,
and the workarounds (`${arr[@]+"${arr[@]}"}`, a `tr` for lower-casing) make the code harder to read
and easy to get wrong again. CI runs on Ubuntu 24.04 (bash 5.2) and the e2e toolchain image on Debian bookworm (bash 5.2).

The dev-toolchain research (`docs/research/2026-11-dev-toolchain.md`) picked "stay, with a versions
file, a drift check and a bash 3.2 CI job" and named a Nix flake as the only option that removes the
bash gap at the source. Nix was ruled out (procoduck removed it), which left a 3.2 job that would catch the
gap but not remove it.

On a Mac, `brew install bash` puts bash 5 ahead of `/bin` on the PATH, and `#!/usr/bin/env bash` finds
it. That closes the gap at the source for the price of one prerequisite.

## Decision

1. **Every repository script that starts with `#!/usr/bin/env bash` requires bash 5 or newer.** On
   macOS that is Homebrew bash (`brew install bash`). The shebang stays `#!/usr/bin/env bash`; no
   script hard-codes a path to a bash.
2. **A guard is the first command of every such script**, one fixed line, placed after the header
   comments and before `set`:

   ```bash
   [ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
   ```

   It is POSIX syntax and uses `BASH_VERSINFO`, so bash 3.2 can parse and run it, and it comes before
   any bash 4+ syntax in the file (bash reads a script line by line, so later lines are never parsed
   after the exit). Exit code 2 is the usage-error code of the repository's scripts.
3. **The guard is inlined, not sourced.** A sourced helper would need its own path lookup
   (`$(dirname "$0")/lib/...`) written in portable syntax, and that is longer and more fragile than the
   guard it would hide. There is no common library the scripts already source. One line, copied
   verbatim, checked by a lint, costs less.
4. **Which files carry it.** Every tracked `*.sh` whose first line is `#!/usr/bin/env bash` (the lint lists files with `git ls-files` and rejects any other bash shebang, such as `#!/bin/bash`): entry
   points, tests and `hack/hooks/*.sh` alike. A test script is run directly by hand and by CI, so it
   gets the same early, plain failure. Not carried, on purpose:
   - files without a bash shebang: libraries that are only sourced by a guarded script
     (`hack/lab/config.sh`, `hack/chart-check.d/*.sh`) and the
     generated `test/e2e/quickstart.golden.sh`;
   - scripts that run inside container images or the sandbox: the flavors' `koinon-*` entry scripts
     and the spike images' (`#!/bin/sh`), the library they source
     (`flavors/common/koinon-entry-lib.sh`) and `internal/agentd/*.sh` (no shebang). Those images are not
     bash 5 by contract and these scripts use POSIX sh. A script that needs bash in an image must install bash 5 there first.
5. **`hack/bash5-lint.sh` fails on a script missing the guard** (or carrying a different text), with a
   test (`hack/bash5-lint-test.sh`, which also proves the guard under `/bin/bash` 3.2 where it exists).
   It runs in preflight (the `bash5` gate, chosen by a `*.sh` change) and in the CI `scripts` job.
6. **Hooks fail open.** `hack/hooks/*-check.sh` are PreToolUse hooks run by the harness with the user's
   PATH, and exit 2 there blocks the tool call, so on an old bash the exit-2 guard would block every
   Bash, Read and Agent call. These three files use a variant instead: it prints
   `{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}`
   and exits 0, so the call goes on and the user sees the hint. `hack/bash5-lint.sh` accepts the variant
   only in `hack/hooks/*-check.sh`; the hooks' `*_test.sh` files keep the exit-2 guard.
7. **Scripts may use bash 5 syntax freely.** Existing 3.2 workarounds are removed where they hurt
   readability; no mass rewrite.

## What this replaces

- The bash 3.2 compatibility rule in `docs/agents/subagent-brief.md` and in script comments.
- The planned bash 3.2 CI job of old issue 544. It is not built.
- The Nix point of `docs/research/2026-11-dev-toolchain.md`: the gap it would have closed is closed by
  Homebrew bash (addendum there). BSD-versus-GNU tool differences (sed, grep, date, mktemp) remain and
  are not covered here.

## Consequences

- A contributor on a Mac runs `brew install bash` once; CONTRIBUTING lists it as a prerequisite.
- Agents run scripts and tests with `bash` (or directly), not `/bin/bash`. `/bin/bash` is for proving
  the guard fires.
- Every new script copies the guard line; the lint tells a contributor who forgot.
- The version in the message is whatever the old shell reports in `$BASH_VERSION`.
- Raising the minimum later (6) is one edit of the guard line in every script plus the lint, done with
  the same mechanical change.

## Note (2026-11-07, issue #300)

The maintainer lab tooling left the repository: the remote-run client, its pod-side scripts, its
configuration and the document that described the setup are no longer tracked. The repository's e2e path
is kind (`make dev-kind-up`, `make test-e2e-kind`, the PR's own `quickstart` run). What the text above
says about the lab's scripts, `--argo`, `--gate` or `hack/lab/` describes the history. The repository
still reads two commit statuses, `e2e-lab` and `go-test-lab`, as evidence that an external run passed
on a commit. _(Changed by [0105](0105-merge-queue-route.md): `e2e-lab` is information only and no longer counts toward a merge.)_
