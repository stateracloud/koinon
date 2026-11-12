# 0093. koinon.core: one Koinon-owned OpenCode plugin instead of a fork

- Status: Accepted in full (maintainer decision 2026-11-11, #603; every decision of this ADR, decision 2 included)
- Date: 2026-11-03
- Area: runtime-sandbox
- Tags: harness, flavors, spend-ledger
- Relates to: [0052](0052-opencode-v2-flavor.md) (the OpenCode flavor, its config and readiness), [0059](0059-v01-opencode-only.md) (one harness in v0.1), [0056](0056-model-providers-pricing-and-budgets-per-org.md) (the `koinon.retry` plugin, amendment 2026-10-31), [0041](0041-koinon-spend-ledger-and-budget-stops.md) (the ledger counts the title request), [0074](0074-run-traces-trace-router.md) (run traces, tool names), [0023](0023-observability-and-audit.md) (fixed metric labels), [0045](0045-openshell-hardening-defaults.md) (OpenShell defaults), [0079](0079-dependency-cooldown.md) (pin age), [0084](0084-leak-guard.md) and [0091](0091-leak-guard-by-destination.md) (the guards outside the sandbox), [0051](0051-session-home-generations.md) (the home outlives a run)
- Issue: #122; the Go harness this contract feeds is #48

## Context

OpenCode is Koinon's only v0.1 harness (ADR 0059), pinned at 2.2.6 (ADR 0052, amendment
2026-10-28). Koinon changes its behaviour today in three ways: the read-only global config
(`/etc/koinon/opencode/opencode.jsonc`), the entry script (`flavors/opencode/koinon-opencode`), and
one plugin, `koinon.retry` (`flavors/opencode/plugins/koinon-retry/index.js`, ADR 0056 amendment
2026-10-31). Several open tickets want more: a mid-turn wrap-up notice (old issue 183), budget notices, loop
nudges (#119), a split of model cost by purpose, and the end of a silent failure mode.

The research of 2026-11-03 (maintainers' store, checked against the 2.2.0 source) found that
OpenCode 2.2.0's plugin API covers most of this without touching OpenCode's code:

- session hooks: prompt, context, compaction, title, retry, model and HTTP request;
- tool hooks before and after a call, and a permission hook (`permission.evaluate`);
- add, update and remove for tools and commands;
- keyed session instructions.

What it also found:

- **A plugin runs inside the sandbox, as the agent's own process.** Whatever it does, the agent's
  code could undo or imitate. It can advise; it cannot enforce. Every rule that must hold stays in
  OpenShell (ADR 0045, 0075) and agentgateway (ADR 0084, 0091).
- **A plugin that fails to load fails silently.** OpenCode logs it and runs on
  (`packages/core/src/plugin/supervisor.ts`). If `koinon.retry` broke, OpenCode would retry 10 times
  instead of Koinon's 3, and neither the entry script nor agentd could tell (flavors/opencode
  README, "Model retries", known limit). Only the flavor contract suite catches it.
- **The permission hook can turn an allow into an ask**, including one that comes from a saved
  "always" answer, but it cannot overrule a configured deny. agentd never picks an "always" option
  (old issue 260), but OpenCode's approval store lives in the agent's home, which outlives a run (ADR 0051),
  and a plugin loaded with `harness.pluginInstalls` could answer one.
- **OpenCode makes an extra model call per session for its title** (one-shot, its own retry path;
  ADR 0041 counts it). Nobody reads the title.
- **Model requests carry no purpose.** The gateway cannot tell a turn's request from a compaction
  or a title, so cost cannot be split by purpose.
- **Config plugin lists add up.** With `pluginInstalls` on, the agent's plugins load after Koinon's
  and can override Koinon's hook results, answer permission prompts, remove tools, and reuse a
  Koinon plugin ID, which stops plugin loading altogether. Their folders reload when edited mid-run.
- **Project config is off** (`OPENCODE_DISABLE_PROJECT_CONFIG=1`, checked by `test/flavors`), so a
  cloned repository's `.opencode/` plugins never load.
- **Compaction and retries need no plugin to be seen.** OpenCode's ACP layer already sends them as
  `session_info_update`, which agentd records raw; #121 is agentd work.

OpenCode 2.2.1 to 2.2.5 (release notes read 2026-11-03) change none of the hooks, ACP events,
title generation, plugin-list merging or permission semantics used here, with one exception that
matters: **2.2.5 (#60687) runs a tool call named `bash` as `shell`**, with the same permissions.
Code that matches the shell tool by name must match both. 2.2.1 adds an experimental
`experimental.idle` session hook (#59437). 2.2.1 (#59649, throttling retried) and the 2.2.5
compaction fixes (#60647, #60660, #60692) may change when `session_info_update` fires. 2.2.3
(#60315) moves managed policy to `plugin.use`, `mcp.use` and `skill.use`; Koinon sets no managed
policy.

## Decision

### 1. One owned plugin, `koinon.core`

Koinon customises OpenCode through **one plugin it owns, `koinon.core`**, plus the existing
`koinon.retry`. It does not fork OpenCode, patch its internals, or replace the global `fetch`.
A new customisation goes into `koinon.core` as a module with its own options and contract-suite
spec, not as a new ad-hoc plugin per ticket. `koinon.retry` stays a separate plugin: it works, has
its own spec, and moving it buys nothing (its options would only move).

### 2. Advisory only

`koinon.core` must never be the only guard for anything. Every behaviour it adds is one of:

- **making OpenCode ask agentd more**, never less (allow to ask; never ask to allow, never a deny
  that agentd would then not see);
- **saving work OpenCode would otherwise do** (the title call);
- **labelling** what the gateway and agentd already see (a request's purpose), where a false label
  costs accuracy in a report, never a limit;
- **hygiene** in the agent's own process (the shell environment).

If an agent's code turned `koinon.core` off, the run would be less tidy and less well reported,
and every boundary would still hold: egress, credentials and isolation in OpenShell, tool
approvals, the leak guard, budgets and request caps in agentgateway and agentd.

Out of bounds, as hard rules:

- real keys or any credential in the plugin, its options or its files (no secrets in sandboxes);
- a plugin as the only deny, block, redaction, tool hiding or answer to a permission prompt;
- patching OpenCode internals, monkey-patching `fetch` or other globals (a fork in all but name);
- community npm plugins, or any run-time npm dependency in Koinon's plugins (ADR 0079 age, and some
  are GPL or AGPL); the one exception is the CI-only type package of decision 4 and its dependency
  closure, which never enter the image;
- any new network egress: the plugin sends nothing anywhere; what it reports goes over ACP or the
  model request it is already part of;
- org, agent, user, task or run IDs in anything that becomes a metric label (ADR 0023);
- weakening OpenShell defaults (ADR 0045) or the isolation tiers (ADR 0075).

### 3. First slice

1. **Load report.** In `setup`, after its options validate, `koinon.core` registers a command,
   `koinon-core-ready-v1`: the contract version is in the command name, because agentd keeps only
   command names from ACP, not descriptions. agentd already waits for
   `koinon:github_koinon-ready` after `session/new` and `session/load` (ADR 0052 decision 4); it now
   also waits for this command and checks the name against the flavor descriptor
   (`internal/flavor`, a new field listing the Koinon plugins and contract versions the image must
   load). A missing or wrong report fails the start with its own reason, within the existing
   readiness timeout. `koinon.retry` reports the same way (`koinon-retry-ready-v1`). This ends the
   silent load failure: a broken plugin, a bad option, or a hook that the pinned OpenCode lacks
   becomes a failed start. A user plugin that reuses a Koinon ID fails the start only if loading stops
   before `koinon.core`'s setup runs (to check in the first slice; it is in the v0.3 deferred
   section anyway).
   If the first slice finds that plugin commands do not reach ACP's `available_commands_update`,
   the fallback is a status file the plugin writes in a directory agentd creates before each start,
   holding a per-start nonce agentd passes in env; agentd reads it over exec. The nonce proves
   freshness only; it is not a credential.
2. **No saved approvals (procoduck's choice).** For `external_directory` (access outside the
   workspace), on `permission.evaluate`, an allow whose resource matches none of the four configured
   exemptions becomes ask again: such an allow can only come from a saved "always" answer (or a user
   plugin), so agentd decides it (`internal/agentd/permission.go`). Config allow and deny rules
   otherwise stand, and the hook cannot loosen either. The earlier wording, "re-ask only where the
   config has no allow rule", was a no-op, because OpenCode's built-in default allows everything, so
   every action has an allow rule. This needs no way to tell a stored answer from a config rule.
   Matching by tool name covers `bash` and `shell` (decision 5).
3. **No title call.** On the session title hook, `koinon.core` sets a fixed constant title,
   `Koinon run`, and OpenCode makes no title request. Nothing from the task goes in the title. One model request fewer per session on the run's ledger and request cap.
4. **Request purpose.** On the HTTP request hook, `koinon.core` sets
   `x-koinon-request-kind: primary | compaction | title`, from the session hooks it sees (a
   compaction hook marks the next request). agentgateway reads it, maps an absent header to `none`
   and any other unknown value to `other`, removes it before the provider, and records it in the
   ledger line's `kind` field and in a fixed-label metric (the fixed set: `primary`, `compaction`,
   `title`, `other`, `none`). The run trace does not carry it, and there is no UI for it in v0.1.
   It is never used to price, cap or allow a request: the request cap and the USD ledger count
   every request whatever its label. With item 3 working,
   `title` should be zero; a non-zero count shows the skip regressed.
5. **Shell environment.** The agent's shell commands see a fixed set of variables: the hook for a
   shell command's environment (or the tool before hook on the shell tool, if 2.2.x has none)
   removes the `KOINON_*` and the remaining `OPENCODE_*` variables and sets non-interactive defaults
   (`PAGER=cat`, `GIT_PAGER=cat`, `GIT_TERMINAL_PROMPT=0`). This is hygiene: `KOINON_MODEL_KEY` is
   already an OpenShell placeholder, not a key.

Next slices, each in its own issue and spec: wrap-up and budget notices that survive compaction
(keyed instructions; `experimental.idle` only after decision 5's rule for experimental hooks), then
loop nudges on top of agentd's own detector (#119).

Not in a plugin, ever: token or cost telemetry (agentgateway is the source); Koinon tools that hold
state (they stay on MCP, behind agentgateway); compaction and retry reporting (#121, ACP already
carries it).

### 4. Packaging, pinning and loading

The `koinon.retry` pattern, unchanged:

- Source in `flavors/opencode/plugins/koinon-core/`: **TypeScript** modules with the SPDX header,
  loaded by OpenCode's bundled Bun, with no npm dependency at run time and nothing installed in the
  sandbox. The plugin uses only `import type` from `@opencode/plugin`, which is erased at run
  time. The package is `@opencode/plugin@2.2.5` (the v2 line, same scope as the `@opencode/cli` the
  image installs); `@opencode-ai/plugin` is the v1 line and must not be used. It is a CI-only dev
  dependency, pinned to the OpenCode release (decision 5), used for `tsc --noEmit`. It is not one
  package but a closure: it depends on `effect` and the `@opencode/*` packages, and declares peers
  (Electron, Vite, solid-js and others) that pnpm must not auto-install (`autoInstallPeers: false`).
  All of it is CI-only, falls under ADR 0079's age rule, and never enters the image.
  Why TypeScript and not JS: the plugin will grow (more modules per decision 1), and a type error
  surfaces in CI when OpenCode is bumped instead of at the first start.
  OpenCode's plugin docs (https://opencode.ai/docs/plugins/) say "Place JavaScript or TypeScript
  files in the plugin directory" and load them at startup, but do not explicitly promise that an
  untranspiled `.ts` loads from an absolute path in the config. The first slice therefore adds a
  `test/flavors` spec proving the `.ts` plugin loads in the pinned image. If it does not, the
  fallback is a pinned, reproducible transpile step in the image build; never a run-time
  `npm install`.
- `koinon.retry` stays plain JS for now; porting it is a separate follow-up, not part of this
  ADR's first slice.
- Copied into the image at `/usr/local/lib/koinon/opencode/plugins/koinon-core/`, root-owned, mode
  0644, outside every config root, so nothing loads it unless the config lists it.
- Listed by absolute path in the read-only `opencode.jsonc` `plugins` list, **first**, before
  `koinon.retry` and before anything `OPENCODE_CONFIG_CONTENT` adds. Options come from the entry
  script's switches through `{env:KOINON_*}` values, validated strictly in `setup`; a bad option
  throws, which with decision 3.1 fails the start.
- Pinned with the image: the plugin, OpenCode and the config ship and change together. Nothing is
  fetched at run time.
- v0.1 loads only Koinon's own plugins (`koinon.core`, `koinon.retry`). Custom, user-installed
  plugins are a v0.3-or-later feature (procoduck, 2026-11-03). The code already has the template option
  `harness.pluginInstalls` (off by default; `internal/agentconfig/agentconfig.go:53`,
  `internal/reconciler/plugins.go:14`, `internal/flavor/flavor.go:71`, entry script switch
  `--plugin-installs` in `flavors/opencode/koinon-opencode:77`). It only opens the npm registry and
  passes the switch. The minimum fence in v0.1: it stays off by default, this ADR adds nothing to it,
  and an ID collision with a Koinon plugin, which stops plugin loading, becomes a failed start through
  decision 3.1 if loading stops before `koinon.core`'s setup (to check). No reserved-name check and no exit-64 refusal ships in v0.1 (see "Deferred to custom
  plugins").

**What `pluginInstalls` still allows.** Where a template turns the existing option on, the agent's plugins run after Koinon's and can override
anything `koinon.core` does inside the sandbox: re-allow a stored answer, call the title model,
change the request label, put variables back. That is acceptable only because decision 2 holds:
none of those is a boundary. A template that turns `pluginInstalls` on accepts a less tidy run, not
a weaker one. A user plugin that answers a permission prompt itself acts on in-sandbox actions only;
external effects are MCP tools behind agentgateway.

### 5. Compatibility with OpenCode releases

- **Pin: OpenCode 2.2.5**, as part of the first slice. The image is on 2.2.0 today; the first slice
  re-pins it and ADR 0052 gets an amendment with the re-pin (at least 3 days old per ADR 0079).
  In 2.2.5 the shell tool is named `shell`; matching still accepts `bash` and `shell` (below).
  2026-11-10: the image is now pinned to 2.2.6 (#422, release notes 2.2.1 to 2.2.6 read), which
  supersedes the 2.2.0 "today" above; the 2.2.5 decision text stays as written.
- `koinon.core` targets **the pinned OpenCode release only**. A hook it needs that is missing makes
  `setup` throw, so the start fails rather than the behaviour quietly vanishing.
- **Tool names.** Every match on the shell tool takes `bash` and `shell` (2.2.5 #60687), in
  `koinon.core` and in agentd's trace name list (`builtinTools` in `internal/agentd/runtrace.go`,
  which today knows only `bash`). Both stay until no supported pin uses `bash`.
- **Experimental hooks** (`experimental.*`) are not used in a shipped slice without an amendment to
  this ADR: OpenCode may change them in a patch release.
- **Every re-pin** (ADR 0052, at least 3 days old per ADR 0079) diffs the `packages/plugin` hook
  types and the permission, title, compaction and ACP command paths between the old and new tags,
  rereads the release notes, reruns `test/flavors`, and records the result in
  `flavors/opencode/README.md`. Decision 3.1 makes a missed break visible at the first start.
- If Koinon ever sets OpenCode managed policy, it must allow Koinon's plugins there
  (`plugin.use` since 2.2.3).

### 6. The contract for koinon-harness

Decisions 2 and 3 are also the written contract for Koinon's own Go harness (#48): it must report
its readiness, keep no stored approvals, make no title call, label each model request's purpose,
and run commands with a fixed environment. When it lands these become native behaviour, and
`koinon.core` is retired with the OpenCode flavor, not ported.

### 7. Tests

`test/flavors` gains one spec per first-slice item: a broken or missing `koinon.core` fails the
start; a stored "always" answer still reaches agentd as a request; the fake model sees no title
request; the fake model sees `x-koinon-request-kind` on a turn and on a compaction; a shell command
sees none of the removed variables. The existing checks stay: project config off, plugin files
root-owned and read-only, `koinon.retry`'s 3 tries. CI never calls paid models.

## Alternatives considered

- **Fork OpenCode.** Full control: every hook we lack, any internal change. Cost later: every
  upstream release (2.2.0 to 2.2.5 came in about a week) is a merge, security fixes arrive late, the
  project's harness direction rules it out, and the fork becomes a second harness to maintain next
  to #48.
- **Upstream patches only.** No Koinon code in the harness at all. Cost: each change waits on
  OpenCode's review and release, then the 3-day cooldown, and some of what Koinon wants (no title
  call, Koinon's request label) has no reason to exist upstream. Not rejected as a supplement:
  where a hook is missing, Koinon proposes it upstream and uses `koinon.core` meanwhile.
- **Ad-hoc hooks per ticket** (a small plugin, or an entry-script trick, each time). Cheapest per
  ticket. Cost later: silent load failures multiply, load order and override lessons are relearned
  every time, and there is no single list of what Koinon changes for #48 to implement.
- **Build the Go harness (#48) now.** Owns the loop, which some leak-guard ideas need (ADR 0091).
  Cost now: weeks of work before v0.1 for behaviour a plugin gives in days; it remains the
  long-term plan and decision 6 feeds it.
- **Enforce through the plugin** (deny, redact, hide tools). Would look stronger. Rejected: it runs
  as the agent; a guard the agent can undo is a false promise, and once relied on it would need a
  redesign.
- **Several small Koinon plugins, one per concern.** Clear separation. Cost: one load report, one
  option set and one ordering question each; one plugin with modules gets the separation without
  the load surface. `koinon.retry` stays separate only because it already exists and is covered.

## Consequences

- Positive: a plugin that fails to load fails the start instead of changing retries and approvals
  without anyone seeing.
- Positive: one model request fewer per session, cost split by purpose, and every in-sandbox
  permission decision visible to agentd.
- Positive: Koinon's changes to OpenCode are in one place with a written contract for #48.
- Negative: more Koinon code depends on OpenCode's plugin API, which moves quickly; each re-pin
  costs a hook diff and a contract-suite run.
- Negative: readiness now also waits for plugin commands; if the ACP command path changes upstream,
  every start fails until the fallback or a fix ships (loud, by design).
- Negative: where a template turns the existing `pluginInstalls` option on, everything in decision 3 can be undone by the agent's own
  plugins; reports from such runs (request labels) are less trustworthy.
- Follow-ups: the first slice (#122); the descriptor field and agentd's load check; the gateway's
  handling of `x-koinon-request-kind`; `shell` in agentd's trace tool names; the README's "known
  limit" for `koinon.retry` is removed once decision 3.1 ships.

## Deferred to custom plugins (v0.3+)

Design notes for the later ADR on user-installed plugins, not v0.1 decisions. v0.1 loads only
Koinon's own plugins.

- **Reserved names.** `koinon.*` plugin IDs and `koinon-*` plugin directory names reserved; with
  `harness.pluginInstalls` on, the entry script would refuse to start (exit 64) when a directory
  under `/sandbox/.config/opencode/plugins/` uses one.
- **Plugins that undo `koinon.core`.** Refuse or detect a user plugin that registers
  `permission.evaluate` (stricter, but OpenCode does not tell us today), or other hooks that
  re-allow a stored answer, call the title model, change the request label or restore variables.
  Decision 2 means none of this is a boundary; the question is only how tidy and trustworthy the
  run's reports stay.
- **Load report.** Whether user plugins report readiness the way Koinon's do, and how the check treats
  an unknown plugin; the load report of decision 3.1 covers Koinon's plugins only until then.

To check in the first slice (unverified in the 2.2.0 source read): whether plugin commands reach
agentd over ACP; whether skipping the title and compaction calls works at runtime; whether keyed
instruction entries survive compaction (for the next slice); whether `permission.evaluate` can tell
a stored answer from a config rule (no longer needed by 3.2, but useful); whether a user plugin
that reuses a Koinon ID stops loading before `koinon.core`'s setup; the name of the shell
environment hook; whether the HTTP request hook can set a header on provider requests; how
OpenCode picks the entry file in a plugin directory; whether agentgateway's access log sees the
header before the standalone header allowlist drops it; and the `packages/plugin` hook-type diff
from 2.2.0 to the chosen pin.

## Delivery

The first slice ships as a series of PRs, in this order: a spike answering the checks above; the
re-pin to 2.2.5 plus `shell` in `builtinTools`; the TypeScript skeleton with a `tsc` and
`node --test` CI job and the `.ts` load spec; agentd's load check; the title and the shell
environment; the gateway and ledger `kind` with one additive migration, then the plugin's header
module; the re-ask (3.2) last. The `flavors` CI job becomes required (procoduck, 2026-11-03).

## Upstream verification

- 2026-11-03: OpenCode 2.2.0 source (tag `v2.2.0`, commit
  `1097e2668b1e08ad63bdf0b1c4860b9bc01e420c`) read for the plugin hooks, plugin loading and list
  merging, permission evaluation, and ACP events (maintainers' research, workflow wf_bb7ae045-3e0).
- 2026-11-03: release notes of 2.2.1 to 2.2.5 read; source not diffed (the first slice diffs the hook types, decision 5).
- Nothing here changes OpenShell or agentgateway behaviour except the gateway reading and removing
  one request header.
- 2026-11-11: OpenCode 2.2.6 source (commit `dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06`) read for the
  first slice's checks (Delivery steps 1-3; research report, workflow session): the plugin shape,
  loading rule, duplicate-ID handling, and the shell, permission and request-label hook types, each
  re-read at source. See the amendment below for the five corrections it found.

## Amendment (2026-11-11, #122): five corrections from reading OpenCode 2.2.6 source

The "To check in the first slice" research (2026-11-03, against the 2.2.0 source) got five
decisions wrong once OpenCode 2.2.6 itself was read for this PR. None of them changes decision 2
(advisory only) or the first slice's scope; they correct what the plugin module writes.

1. **Decision 3.5, the shell environment hook.** The hook is `ctx.shell.hook("create.before", e =>
   ...)`, with `e.env: Record<string, string | undefined>`, not `shell.env`. Remove a variable by
   setting it `undefined` in that map, not with `delete` (the spawner's `extendEnv` behaviour for a
   `delete`d key is unverified; `undefined` removes it either way).
   https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/plugin/src/promise/shell.ts#L3-L17
2. **Decision 4 and the Context section ("reuse a Koinon plugin ID, which stops plugin loading
   altogether").** Wrong, and wrong since OpenCode 2.2.0 too: a duplicate plugin ID does not stop
   loading. The supervisor orders built-ins, then configured packages in document order, keeps the
   first of a duplicate ID, and fails only the later one, before any `setup` runs. `koinon.core`
   being listed first (decision 4) is therefore both necessary and sufficient against an ID clash;
   no further defence is needed in v0.1.
   https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/core/src/plugin/supervisor.ts#L94-L113
3. **Decision 3.4, request purpose.** Use the `model.request` hook's own `kind` (readonly) and
   `headers` (mutable), not a label kept across hooks from the compaction one ("a compaction hook
   marks the next request"). `kind` is one of four ready-made values per call site: `primary`,
   `compaction`, `title`, `generate`. `generate` is outside the ADR's fixed label set (`primary`,
   `compaction`, `title`, `other`, `none`); send it through unchanged and let the gateway's mapping
   turn it into `other`.
   https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/core/src/session/model-request.ts#L337-L362
4. **Decision 3.2, the permission hook.** "the hook cannot loosen either [a configured allow or
   deny]" is true only for allow and deny; the hook *can* loosen a configured `ask` to `allow`. The
   module must therefore write only `ask`, never assume the hook is one-directional.
   https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/core/src/permission.ts#L176-L215
5. **Decision 4, the types package pin.** `@opencode/plugin@2.2.6`, matching the image's pinned
   OpenCode release (the ADR's `2.2.5` text was the re-pin plan's pin at the time of writing, since
   superseded by the 2026-11-10 re-pin to 2.2.6 noted in decision 5).

Also confirmed, not a correction: plugin commands do reach agentd over ACP's
`available_commands_update` (so decision 3.1's load report needs no status-file fallback); the
title and compaction `result` hooks exist and skip the request as decision 3.3 assumes; keyed
session instructions (the next slice) moved under `ExperimentalSessionApi` between 2.2.0 and
2.2.6, so decision 5's rule for experimental hooks now applies to them.

## Links

- Issue #122; related #48, #119, #121; old issues 53, 84, 183, 243, 260, 261 and 341
- OpenCode PRs #59437 (`experimental.idle`), #59649, #60315, #60647, #60660, #60687 (`bash` runs as
  `shell`), #60692
- [flavors/opencode/README.md](../../flavors/opencode/README.md) ("Model retries", "Plugins")
- [flavors/opencode/plugins/koinon-retry/index.js](../../flavors/opencode/plugins/koinon-retry/index.js)
- ADRs [0052](0052-opencode-v2-flavor.md), [0059](0059-v01-opencode-only.md),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0074](0074-run-traces-trace-router.md), [0023](0023-observability-and-audit.md),
  [0045](0045-openshell-hardening-defaults.md), [0075](0075-isolation-tiers.md),
  [0079](0079-dependency-cooldown.md), [0084](0084-leak-guard.md), [0091](0091-leak-guard-by-destination.md),
  [0051](0051-session-home-generations.md)
- https://github.com/anomalyco/opencode/tree/v2.2.0
