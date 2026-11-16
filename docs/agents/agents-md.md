<!-- SPDX-License-Identifier: Apache-2.0 -->

# AGENTS.md files: where they go and what they hold

How the agent instruction files of this repository are made. The root `AGENTS.md` holds what every
change needs. A folder with its own rules gets its own `AGENTS.md`. `hack/agentsmd-lint.sh` checks the
rules below (issue 1045).

## Why

- Every session loads the root file. A long root file costs tokens on every change, including the changes
  that need none of its chart, migration or CI rules. Anthropic's guide for memory files asks for under 200
  lines per file, and a study of context files ([arXiv 2602.11988](https://arxiv.org/abs/2602.11988))
  found that they add over 20% cost without a general gain in success. So the root stays short.
- There is no `CLAUDE.md` anywhere in the tree. Claude Code (v2.1.277 or later) reads `AGENTS.md` itself:
  the root file at the start, and the file of a folder when it reads a file in that folder. A `CLAUDE.md`
  that only says `@AGENTS.md` would stop that native reading, so nested files would be skipped.
- Other tools read the same files: OpenAI Codex walks from the root to the working directory, and
  grafana/grafana and apache/airflow keep a root index and nested `AGENTS.md` files.

## When a folder gets its own file

All three must hold:

1. It has 3 or more rules that apply only when you edit that folder and that no linter, generator or test
   enforces. One rule with a high failure cost is also enough.
2. Agents edit there often.
3. It is a real boundary: its own toolchain, generated code, a compatibility contract or a gate.

Put the file at the highest folder where all its rules apply (`charts/`, not `charts/koinon/templates/`).
A rule that a tool enforces does not need a line: the tool tells the agent.

## What goes where

- **Root:** the hard rules first, then the commands every change needs, picking up work, branches and pull
  requests, cross-folder conventions, working rules, and one index line per nested file.
- **Nested file:** only the rules of its folder: its commands, invariants, pitfalls and gate. The first line
  is exactly "The root AGENTS.md applies too; this file adds rules for `<folder>/`." Never restate a root
  rule.
- **Linked docs:** procedures, background and reasons (ADRs, `docs/dev/ci.md`, `docs/maintainers/`). A rule
  that an agent must not break stays in an `AGENTS.md`; the page it links says how and why.

The root index line has the form: "Editing under `charts/`: read `charts/AGENTS.md` (what it holds)."

## Budgets

| File | Target | Ceiling |
|---|---|---|
| Root `AGENTS.md` | 110 lines | 150 lines |
| Nested `AGENTS.md` | 60 lines | 120 lines |
| Any file | | 10 KB |

A file above its target prints a note. A file above its ceiling fails the lint.

## The lint

`hack/agentsmd-lint.sh` runs `hack/cmd/agentsmdlint` (a Go program, ADR 0118: logic in `hack/` is Go).
It runs in `hack/preflight.sh` (gate `scripts`) and in the `scripts` job of CI. It fails when:

- a `CLAUDE.md` is in the tree;
- the root index and the set of nested files differ;
- a file is above its ceiling or above 10 KB;
- the first line of a nested file is not the required line;
- a `make` target, a path (`hack/`, `charts/`, ...) or a relative link in any file does not exist;
- the same sentence of 30 or more characters is a rule in two files.

## Adding a nested file

1. Check the three conditions above. If one fails, write the rule in the root file or a linked doc.
2. Create `<folder>/AGENTS.md` with the required first line.
3. Add its index line to the root `AGENTS.md`.
4. Run `hack/agentsmd-lint.sh`.
5. A nested file is owned like the root file: `.github/CODEOWNERS` lists `AGENTS.md` at every depth.

## Check that Claude Code reads the files

After a change to how a session loads instructions, check in a session with no `CLAUDE.md`: the root
`AGENTS.md` loads at the start; reading a file under `charts/` loads `charts/AGENTS.md`; a subagent gets the
root rules. If native reading fails in a session type we use, stop and report it on issue 1045.
