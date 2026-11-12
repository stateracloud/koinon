# 0053. Hermes context-file build patch: redo on every upgrade, with a guard test

- Status: Superseded by [0059](0059-v01-opencode-only.md)
- Date: 2026-10-23
- Area: runtime-sandbox
- Tags: harness, flavors, dependencies

## Context

Hermes loads repository context files (`.hermes.md`, `HERMES.md`, `AGENTS.md`, `CLAUDE.md`,
`.cursorrules`) from the session directory into its system prompt, and splices those of
subdirectories into tool results. A repository must not change what the agent is told: Koinon's
instructions come from the agent version, and the repository is content to read.

No Hermes setting turns context files off for an ACP session without also dropping `SOUL.md`,
where Koinon's instructions are linked. So the Hermes flavor patches Hermes at image build
time, in [`flavors/hermes/no-context-files.py`](../../flavors/hermes/no-context-files.py) (see
[`flavors/hermes/README.md`](../../flavors/hermes/README.md), "Context-file patch"). The patch
edits one block of `agent/agent_init.py` found by an exact text match. It is not a stable
interface, so any Hermes upgrade can break it.

## Decision

1. **The patch stays, and is redone on every Hermes upgrade.** A new Hermes version is not
   accepted until the patch applies to it and the guard test passes.
2. **Guard test.** Two layers, both must fail the build or the CI job:
   - `no-context-files.py patch` exits non-zero unless the code to patch is present exactly once;
   - `no-context-files.py check` builds an agent the way the gateway builds an ACP session and
     fails if `SOUL.md` is missing from the system prompt, if any repository context file is in it,
     or if the subdirectory hints are on. The flavor contract suite
     ([`test/flavors`](../../test/flavors)) checks the same over ACP for each context file.
3. **Upgrade checklist** _(inferred)_. For each Hermes upgrade:
   1. Bump the pin in `flavors/hermes/Dockerfile`.
   2. Re-read `agent/agent_init.py`, `agent/prompt_builder.py`, `agent/system_prompt.py`,
      `agent/subdirectory_hints.py` and `gateway/session_execution.py` for changes to how context
      files, `SOUL.md` and `skip_context_files` interact.
   3. Update the anchor and the replacement in `no-context-files.py` if the code moved; never
      loosen the exactly-once match.
   4. Build the image and run the contract suite for Hermes.
   5. Update the table of switches in `flavors/hermes/README.md`.
   6. If the patch cannot be redone, do not ship the upgrade.

## Alternatives considered

- **Fork Hermes** _(reconstructed)_. Same maintenance, plus a fork to keep.
- **Delete context files from the repository before each run** _(reconstructed)_. Races with the
  agent and misses files created during the run.
- **Accept repository context files** _(reconstructed)_. A repository could then rewrite the
  agent's instructions, which breaks the instruction model of ADR 0052.

## Consequences

- Positive: a Hermes upgrade cannot quietly let a repository change the agent's instructions.
- Negative: every Hermes upgrade costs a manual patch review.
- Follow-ups: the checklist goes into the Hermes flavor README.

## Open questions

- Whether upstream Hermes will offer a setting that makes the patch unnecessary. If it does, the
  patch can go.
- Whether the guard should also run on a schedule against the newest Hermes release, to warn early.

## Upstream verification

None needed. The patch and its build-time check exist in `flavors/hermes/no-context-files.py`.

## Links

- [`flavors/hermes/no-context-files.py`](../../flavors/hermes/no-context-files.py),
  [`flavors/hermes/README.md`](../../flavors/hermes/README.md)
- ADRs [0035](0035-v01-flavors.md), [0052](0052-opencode-v2-flavor.md)
