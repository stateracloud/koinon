# 0071. The skill library: pinned, scanned skills delivered by agentd

- Status: Accepted (procoduck, 2026-10-28; built with the UI milestone after v0.1.0)
- Date: 2026-10-28
- Area: connectors-tools
- Tags: skills, security, harness, flavors
- Relates to: [0003](0003-postgres-source-of-truth.md), [0007](0007-connector-model.md),
  [0020](0020-agent-builder-and-templates.md), [0023](0023-observability-and-audit.md),
  [0031](0031-artifacts-in-object-storage.md), [0033](0033-file-based-memory.md),
  [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0045](0045-openshell-hardening-defaults.md), [0052](0052-opencode-v2-flavor.md),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0057](0057-mcp-servers.md),
  [0062](0062-code-host-connectors.md), [0063](0063-forge-credential-classes.md),
  [0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md); issues
  old issue 58 and
  old issue 84

## Context

A skill is a directory of instructions, and optionally scripts and reference files, that an agent
loads when a task calls for it. Koinon has no skill concept today. Templates (`templates/templates.go`,
`Spec`) have no skills field, and the API, the database and the UI have no skill objects.
`docs/design.md` drew "home volume: memory, skills, sessions", but skills were then shut out on
purpose:

- **The old issue 84 test.** `test/flavors/opencode_test.go` (`homeSeed`) plants `SKILL.md` files in
  `~/.claude/skills`, `~/.agents/skills`, `~/.opencode/skills` and `~/.config/opencode/skills`, and
  asserts that none of them reach the model ("None of it may reach the model or loosen the harness
  (old issue 84)").
- **How.** `OPENCODE_CONFIG_DIR=/etc/koinon/opencode` is read-only, `OPENCODE_DISABLE_PROJECT_CONFIG=1`
  and `OPENCODE_DISABLE_CLAUDE_CODE=1` are set (`flavors/opencode/koinon-opencode`), and the plugin
  `opencode.config.compatibility`, which adds the `.claude` and `.agents` roots, is negated in
  `flavors/opencode/etc/koinon/opencode/opencode.jsonc`.
- **The only extension switch is `harness.pluginInstalls`**, off in every template. With it on, the
  agent can write a plugin into its writable home and the plugin loads with OpenCode's full
  privileges in every later run. That is old issue 58, which names two fixes: load only plugins whose
  hash a person approved, or install them read-only from the agent version. Plugins are code that
  runs in-process; skills are content. They need the same two fixes.

Users want skills, and most of the templates' stories (ADR 0020) would be better with one. The
question is how to offer them without giving up what old issue 84 bought.

**The standard.** [Agent Skills](https://agentskills.io) is the open format Anthropic released; the
repository is [agentskills/agentskills](https://github.com/agentskills/agentskills) (Apache-2.0
code, CC-BY-4.0 docs). The [specification](https://agentskills.io/specification):

- A skill is a directory with a `SKILL.md` and optional `scripts/`, `references/` and `assets/`.
- Frontmatter: `name` (1-64 characters, `[a-z0-9-]`, no leading, trailing or double hyphen, equal to
  the directory name) and `description` (1-1024 characters) are required. `license`, `compatibility`
  (at most 500 characters), `metadata` (string to string) and `allowed-tools` (experimental) are
  optional. There is no version field; the spec suggests `metadata.version`.
- Loading is progressive: the metadata at startup, the body on activation, resources as needed.
- The implementer guidance (`docs/client-implementation/adding-skills-support.mdx`) says that
  sandboxed or cloud agents must provision user and org skills "from an external source … letting
  users upload skill directories through a web UI". That is Koinon's case.
- The showcase lists OpenCode, Claude Code, Codex, Goose, Cursor, Gemini CLI and about 40 other
  clients, so one stored format serves every flavor Koinon may add.

**OpenCode 2.2.0** (read at tag
[`v2.2.0`](https://github.com/anomalyco/opencode/tree/v2.2.0), commit `1097e266`):

- **Config-directory skills.** Every config directory contributes `<dir>/skill` and `<dir>/skills`
  (`packages/core/src/config/plugin/skill.ts:88-91`), and the global config directory is one
  (`config.ts:233-240`). So `/etc/koinon/opencode/skills/` would load if the image had one.
- **The `skills` key.** `skills: string[]` takes "Additional paths or URLs to discover skills from"
  (`packages/schema/src/config.ts:88`). A URL entry makes OpenCode fetch `<url>/index.json` and the
  files it lists into `~/.cache/opencode/skills/<hash>` at run time (`skill/discovery.ts:92-185`):
  runtime egress, unpinned.
- **Scanning.** A directory holding `SKILL.md` is a skill. The scan goes at most 6 levels deep and
  skips dot-entries (`config/plugin/skill-scan.ts`).
- **Hot reload.** Skill roots are watched; a change on disk reloads the list after 100 ms
  (`skill.ts:166-174`). A write to a skill directory takes effect within the turn.
- **`permission.skill`.** The `skill` tool asserts `permission` for action `skill` and the skill's
  ID before it returns the body (`tool/plugin/skill.ts`), and the list the model sees drops every ID
  that evaluates to deny. The docs show allow, deny and ask patterns
  (`packages/web/src/content/docs/skills.mdx`). `allowed-tools` is ignored.
- **Scripts.** OpenCode never runs a skill's scripts itself. The model runs them with its shell
  tool, under the same permissions as any other command.
- **Built-in skills.** The `opencode.skill` plugin adds `opencode` and `report`; `report` tells the
  agent to file an issue with the GitHub CLI (`plugin/skill.ts`).

**Other harnesses** each have a read-only or extra path for skills: Claude Code's managed directory
and `--add-dir` ([docs](https://code.claude.com/docs/en/skills)), Codex's `/etc/codex/skills`
([docs](https://learn.chatgpt.com/docs/build-skills)). One neutral delivery path, pointed at per
flavor, serves them all.

**The attacks are real and common.**

- Liu et al. ([arXiv:2601.10338](https://arxiv.org/abs/2601.10338)) found that 26.1% of skills had
  at least one vulnerability and 5.2% were likely malicious. Data exfiltration was the most common
  category (13.3%), then privilege escalation (11.8%). Skills with scripts were 2.12 times as likely
  to be vulnerable.
- Snyk's ToxicSkills study of ClawHub found a flaw in 36.82% of skills and 76 confirmed malicious
  payloads; ClawHavoc put more than 1,200 malicious skills in the OpenClaw marketplace
  ([CSA research note](https://labs.cloudsecurityalliance.org/research/csa-research-note-skill-md-agent-context-poisoning-20260506/),
  [arXiv:2603.00195](https://arxiv.org/html/2603.00195)).
- The techniques include prompt injection in `SKILL.md` (with invisible Unicode tag characters,
  per the CSA note), a harmless-looking `SKILL.md` that runs a bundled payload script
  ([SkillJect](https://promptfoo.dev/lm-security-db/vuln/automated-stealth-skill-injection-1a4b312b/)),
  over-broad descriptions that trigger everywhere, and memory poisoning.

What Koinon's sandbox already contains (ADR 0045): scripts run as UID 10001 under Kata, Landlock and
seccomp with `/etc` and `/usr` read-only; there are no real credentials in the sandbox; egress is
denied by default; MCP allowlists, ask-tier approvals and spend caps bound tool use. What it does
not contain: a skill that steers the agent to misuse what it was granted (exfiltrate through a PR
body, an allowed MCP tool or a model call; mislead an approver), subtly wrong work, persistence by
editing a skill the harness hot-reloads, and spread to every agent that shares a poisoned skill.
The sandbox makes a malicious skill a confused deputy, not a host or credential compromise. Review,
pinning and scoped grants limit the rest.

**Scanners.** Three were assessed:

- **NVIDIA [SkillSpector](https://github.com/nvidia/skillspector)** (Apache-2.0, about weekly
  releases, v2.13.2 on 2026-10-28). 71 patterns in 17 categories, including prompt injection,
  exfiltration, taint tracking, YARA and supply chain. `--no-llm` is fully local except the OSV
  check, which sends dependency names to `api.osv.dev` and falls back to a bundled list offline. It
  powers [NVIDIA Verified Skills](https://docs.nvidia.com/skills/). The method's paper reports
  86.7% precision and 82.5% recall *with* its LLM step (arXiv:2601.10338).
- **Cisco [skill-scanner](https://github.com/cisco-ai-defense/skill-scanner)** (Apache-2.0, about
  weekly releases). YAML and YARA-X signatures, AST and dataflow analysis, and an optional LLM judge
  that works with a local model or gateway. Its own measured-results page
  (`docs/reference/measured-results.md`) reports that on the MaliciousSkillBench held-out split
  **rules alone catch 7.7% at HIGH**, and that with an LLM judge 66.7% reach MEDIUM or above at a
  15.4% false-positive rate. On 12.5k real skills, static rules flag 3.76% at MEDIUM or above
  against SkillSpector's 26.42% (a vendor measuring a competitor). Its API server had
  [CVE-2026-26057](https://nvd.nist.gov/vuln/detail/CVE-2026-26057) (bound to all interfaces; fixed
  in 1.0.2, off by default).
- **[Snyk agent-scan](https://github.com/snyk/agent-scan)** sends skill content to Snyk's Agent Scan
  API and needs `SNYK_TOKEN`. It cannot run offline or self-hosted.

**Distribution.** People add skills with one line today, and Koinon should take the same lines.

- **The `skills` CLI** ([vercel-labs/skills](https://github.com/vercel-labs/skills), MIT, read at
  `main` `699d35b2` on 2026-10-28; [CLI reference](https://github.com/vercel-labs/skills/blob/main/docs/commands.md),
  [creating skills](https://github.com/vercel-labs/skills/blob/main/docs/creating-skills.md)):
  - `npx skills add <source>` accepts GitHub shorthand `owner/repo`, `owner/repo/path/to/skill`
    and `owner/repo@skill-name`, full GitHub and GitLab tree URLs, Azure Repos URLs, any git URL
    (`git@…`, `ssh://…`, `https://…`), a local path, and a direct URL to a `SKILL.md` or an archive.
    A git ref goes in a `#ref` fragment (`src/source-parser.ts`): in this CLI `@` selects a skill,
    not a ref. `--skill` selects skills by name and `--list` lists them without installing.
  - Arbitrary HTTPS hosts are tried first as **well-known** sources: `/.well-known/agent-skills/index.json`
    (RFC 8615), whose v0.2.0 entries carry `name`, `type` (`skill-md` or `archive`), `url` and a
    `sha256:` `digest` that the CLI checks (`src/providers/wellknown.ts`).
  - In a repository it looks for a root `SKILL.md`, then `skills/` and about 55 agent directories
    (`.claude/skills/`, `.agents/skills/`, …), up to three levels deep, and also the skills listed in
    `.claude-plugin/marketplace.json` or `plugin.json`.
  - It writes into each chosen agent's skills directory in the project (`./<agent>/skills/`) or the
    home (`-g`), as a symbolic link to one canonical copy or as a copy. Downloads are capped at
    10 MiB, extracted content at 25 MiB and archives at 1000 files.
  - `skills find` searches `https://skills.sh/api/search?q=…` (`src/find.ts`). `skills check` and
    `skills update` compare installed skills with upstream. The experimental `skills-lock.json`
    records a content hash per skill. The CLI sends anonymous telemetry unless `DISABLE_TELEMETRY=1`.
  - There is no signing.
- **Claude Code plugin marketplaces**
  ([create](https://code.claude.com/docs/en/plugin-marketplaces),
  [reference](https://code.claude.com/docs/en/plugins/marketplace-reference.md)):
  - A marketplace is a repository with `.claude-plugin/marketplace.json`, which requires `name`,
    `owner` and `plugins`; each plugin entry needs `name` and `source`.
  - `/plugin marketplace add` (or `claude plugin marketplace add`) takes `owner/repo`,
    `owner/repo@ref` or `owner/repo#ref`, a git URL with `#ref`, an HTTPS URL to a
    `marketplace.json`, or a local file or directory. `/plugin install <plugin>@<marketplace>`
    installs one plugin. Skills sit in a plugin's `skills/` directory.
  - Plugin sources are a relative path, `github` (`repo`, `ref`, `sha`), `url` (git), `git-subdir`
    (`url`, `path`, `ref`, `sha`), `archive` (HTTPS zip with `sha256`), `npm`, and `command`, which
    runs a command on the user's machine.
- [anthropics/skills](https://github.com/anthropics/skills) holds reference skills with per-skill
  licences.
- [NVIDIA/skills](https://docs.nvidia.com/skills/) signs each skill with a detached `skill.oms.sig`
  in the OpenSSF Model Signing format, over the whole directory
  ([signing](https://docs.nvidia.com/skills/signing-agent-skills)). NVIDIA says a signature proves
  integrity, not safety.
- In practice a skill's version is a git commit. Many skills are several files: a `SKILL.md` that
  links to `references/`, `scripts/` and `assets/`, so the agent reads only what a task needs (the
  specification's progressive disclosure).

## Decision

1. **Two libraries: the platform's and each org's.** It is the model of templates and providers:
   the platform defines, and an org adds its own.
   - **Platform library.** Platform admins import and curate skills. Every org sees them and can use
     them. Platform rows are platform-scoped; an org's review state for a platform skill version is
     an org row keyed by `(org_id, id)`.
   - **Org library.** Org admins and owners import skills only their org sees. Org tables are keyed
     by `(org_id, id)` with composite foreign keys.
   - A skill's `name` is unique within a library. An agent version may not reference two skills with
     the same name, so a platform skill and an org skill of the same name never meet in one run.

2. **Skill versions are immutable and identified by content digest.**
   - The digest is SHA-256 over a canonical encoding of the tree: sorted relative paths, each with
     its executable bit and the SHA-256 of its contents. `internal/skills` defines the encoding once,
     with a test vector. Symbolic links, devices and paths that leave the tree are refused.
   - The tree is stored in object storage (ADR 0031) as one archive keyed by digest. Postgres holds
     the version row.
   - Each version records its **provenance**: the source (upload with its file name, paste, the
     browser editor with the version it was based on, or a source record from decision 6: git URL,
     ref, path and resolved commit; marketplace and plugin; well-known or direct URL with its
     digest), who imported it, and when. It also records its **scan results**
     (decision 5), its **signature** result (decision 7) and its **approval status** with its
     history (decision 4).
   - Importing an identical tree again creates no new version. It adds a provenance entry to the
     existing one.

3. **Agent versions and templates reference skills by name and digest.**
   - The agent version gains `skills: [{library, name, digest}]`. A template (`templates/*.yaml`)
     gains `skills: [{name, digest}]`, naming platform library skills; instantiating it copies the
     references into the draft agent version.
   - Adding, removing or updating a skill on an agent is a new agent version, so it goes through the
     existing proposed-to-active approval (ADR 0020, 0066). A new skill version never changes a
     running agent by itself. The agent page offers "update to the latest approved version", which
     proposes a version with the skill diff attached.
   - An agent version can be activated only when every skill it references is **Approved** for the
     org. A flavor whose descriptor has no skills path (decision 8) refuses an agent version with
     skills.

4. **Review gates: a platform setting with an org override, defaulting to auto-approve.**
   - **The gate** has three values:
     - `auto` (the default): a version is approved once its scans complete.
     - `auto-if-clean`: a version is approved once its scans complete *and* no finding is at or above
       the gate's severity threshold (default `high`). Otherwise a person reviews it.
     - `human`: a person approves every version.
   - **Where it is set.** The platform setting lives in the Platform area (seeded from Helm
     `skills.review.gate: auto`, `skills.review.threshold: high`). Each org can set its own gate and
     threshold in its settings. **The org's setting wins over the platform's**, in either direction.
     An org without its own setting uses the platform's.
   - **Statuses** per version and org: **Needs review**, **Approved**, **Blocked**.
     - A new version starts in Needs review. When its scans complete, the gate either approves it
       (recorded with the actor `gate:auto` or `gate:auto-if-clean` and the scan IDs) or leaves it
       for a person.
     - A version whose scans did not complete (a scanner failed or timed out) stays in Needs review
       under every gate. The scans always run, so an unscanned version is never auto-approved.
     - The review shows a **diff from the previous version** of the skill (`SKILL.md`, every script
       and reference), next to the findings, as Prices (ADR 0056) and the MCP tool-hash review
       (ADR 0057 amendment, item 6) do.
     - **Blocked is always possible, whatever the gate.** An org admin can block any version for
       the org; a platform admin can block a platform version for every org. Blocking takes a
       reason, is audited, and takes effect at once: a run whose agent version references a Blocked
       skill does not start, and the agent page says why. Unblocking returns the version to Needs
       review.
     - Under `auto-if-clean` and `human`, a rescan (decision 5) that raises a finding at or above the
       threshold on an Approved version moves it back to Needs review. Active agent versions keep
       it until a person approves or blocks it; no new agent version can be activated with it
       meanwhile. Under every gate, such a finding notifies the org's admins in the inbox (ADR 0065).
   - Every status change is audited with the actor, the reason and the scan IDs it rests on.

5. **Scans always run, in the control plane, and record their findings.**
   - **When:** on import, on every new version, and when a scanner is updated (a Koinon release that
     changes a scanner digest, or a rule pack update). An update rescans every version that an agent
     version or template references and the latest version of every skill.
   - **The scanners**, both Apache-2.0:
     - **SkillSpector** with `--no-llm`. Its OSV lookup runs only when the platform allows it
       (Helm `skills.scan.osv: false` by default); otherwise it uses its bundled list.
     - **Cisco skill-scanner**, static only: YARA-X signatures and AST and dataflow analysis. Its
       VirusTotal, AI Defense and OSV options stay off, and its API server is never started
       (CVE-2026-26057); Koinon runs the CLI.
   - **Where.** koinon-api queues a scan, and the reconciler runs it as a Kubernetes Job in the
     control-plane namespace, never in a sandbox. The Job mounts the skill tree read-only, has no
     credentials, and its NetworkPolicy denies egress except `api.osv.dev` when OSV is allowed and
     agentgateway when the judge runs. Its image references go through `koinon.imageRef` with
     `koinon.imagePullSecrets` (ADR 0061).
   - **Pinned.** Each scanner image is pinned by digest in the chart values, and each scan records
     the scanner name, version, image digest and rule-pack version with its results.
   - **Results.** The SARIF output is stored in object storage, and findings are normalized into rows
     (scanner, rule, severity, file and line, message). SkillSpector's risk score is stored too.
     Metrics carry fixed labels only (scanner, outcome), no org or skill IDs (ADR 0023).
   - **The optional LLM judge.** An org can turn on a judge, configured beside its gate: on or off, a
     model from the org's own models, and a per-scan token cap. Both scanners' judges accept an
     OpenAI-compatible base URL, so the Job calls the org's model **through agentgateway**, metered
     to the org's budget in the spend ledger (ADR 0041). Nothing goes to a third party that the org
     did not already configure. Judge findings count toward the gate threshold like any other. An
     org that turns the judge on gets judge findings for the platform skills it uses, on its own
     model. CI runs the judge against the fake model server and never calls a paid model.
   - **Auto-approve is a risk trade-off.** By Cisco's own numbers, static rules alone catch 7.7% of
     the held-out malicious skills at HIGH; with an LLM judge 66.7% reach MEDIUM. SkillSpector's
     method reaches 82.5% recall only with its LLM step. So `auto`, and `auto-if-clean` without a
     judge, will approve most malicious skills a determined author writes. Koinon accepts that as the
     default because of what still limits a malicious skill:
     - the sandbox (ADR 0045): no credentials, no egress beyond grants, read-only system paths,
       Kata isolation, so the skill is a confused deputy, bounded by the agent's grants, ask-tier
       approvals and spend caps;
     - pinning: a skill reaches an agent only through an approved agent version, and nothing updates
       it silently;
     - digests: the delivered content is checked on every prompt and turn (decision 8), so a skill
       cannot be altered to persist;
     - Blocked, which stops every run that uses a version at once, and the rescans, which catch a
       version that a later rule identifies.
     An org that handles sensitive data should choose `auto-if-clean` with the judge, or `human`.

6. **Adding a skill is low friction: one line, an upload or a paste.** Every way in ends the same
   way: koinon-api resolves and fetches the content in the control plane (never the sandbox), pins
   the commit and the content digest, validates the tree, scans it (decision 5) and applies the
   gate (decision 4). Under the default gate a clean import is usable within the scan's run time.
   - **One line, from the ecosystem.** One parser in `internal/skills/source` accepts what people
     already copy:
     - a pasted `npx skills add <source> [--skill <name>…]` command, parsed and never run. It takes
       the `skills` CLI's grammar exactly, so a line copied from skills.sh means the same thing in
       Koinon: `owner/repo`, `owner/repo/path`, `owner/repo@skill`, `#ref`, GitHub, GitLab and
       Azure Repos URLs, any git URL, a well-known HTTPS host, or a direct `SKILL.md` or archive
       URL. Flags about where to install (`-g`, `-a`, `--copy`, `-y`, `--all`) mean nothing in
       Koinon and are ignored with a note;
     - the same `<source>` without the command;
     - a Claude Code marketplace: `/plugin marketplace add <source>` text, `owner/repo[#ref]`, a git
       URL or an HTTPS URL to a `marketplace.json`, and `<plugin>@<marketplace>` for a marketplace
       already added. Koinon reads `marketplace.json` and imports **only the skills** of the chosen
       plugins (their `skills/` directories). Hooks, commands, MCP servers and other plugin code are
       not imported. Plugin sources of type relative path, `github`, `url`, `git-subdir` and
       `archive` (with its `sha256` checked) are supported; `npm` and `command` are refused, because
       one needs a package registry and the other runs a command.
     - `@` is the one ambiguity: the `skills` CLI reads `owner/repo@x` as a skill name, Claude Code's
       marketplace command as a ref. Koinon follows the `skills` CLI for skill sources and
       Claude Code for marketplace sources, and takes `#ref` for a ref in both.
   - **Resolve, then confirm.** The parser produces a source record (kind, URL, ref, path, skill
     filter). koinon-api resolves the ref to a commit, discovers the skills in the tree with the
     `skills` CLI's rules (root `SKILL.md`, `skills/` and the agent directories, three levels; plus
     marketplace manifests), and shows what it found: each skill with its name, description, path,
     licence, file count and size, and the commit. The person picks one, several or all. A source
     with many skills (`anthropics/skills`) is therefore one paste.
   - **Fetching.** Git is fetched with hooks, submodules and LFS off, at the resolved commit. A
     well-known source must match the index entry's `sha256:` digest; a direct URL is fetched once
     and its digest recorded. Private repositories use the org's code-host connection (ADR 0062,
     0063), whose credentials stay in the control plane. The sandbox never fetches a skill.
   - **Where.** The same operation is offered in three places:
     - the UI's **Add skill** box, on the library page and in the skills section of the agent and
       template pages (decision 10);
     - the API: `SkillService.ResolveSource` (parse and discover, no import) and `ImportSkills`
       (fetch, store, scan, gate), plus `UploadSkill` and `CheckUpdates`;
     - the CLI: `koinon skills add <source> [--skill <name>…] [--org <org>]`, with `list`, `check`
       and `update`. Koinon has no user CLI yet (ADR 0003 anticipates one). This adds `cmd/koinon`
       as a thin client of the public API with an API token, and `skills` is its first command
       group; the CLI's wider shape is left to the ADR that grows it.
   - **A catalog browser.** The library page has a **Browse** tab that searches skills.sh through
     its search API (`/api/search`) and lists the skills of the catalogs a platform admin
     configures: Claude Code marketplaces, well-known hosts and git repositories, seeded with
     `anthropics/skills` and `NVIDIA/skills`. Each result shows its source, its install count where
     the catalog has one, and an **Add** button that runs the one-line flow. koinon-api makes these
     requests; a platform setting turns browsing off for installs without egress
     (`skills.catalog.enabled`, default on). Browsing never imports anything by itself.
   - **Upload and paste.**
     - Upload a zip or tar archive, or drop a folder; the browser sends every file with its relative
       path, so the whole tree is kept.
     - Paste a single `SKILL.md`. The skill's directory name comes from its `name`.
     - **Build in the browser.** Start from a pasted or written `SKILL.md`, then add files and
       folders (`references/`, `scripts/`, `assets/` or any other) in a small file-tree editor with
       a text editor for each file. Saving creates a version, like any other import.
   - **Multi-file skills keep their structure exactly.**
     - The tree is stored as imported: paths, contents and executable bits. Nothing is flattened,
       renamed or rewritten.
     - **Validation** against the Agent Skills specification: `SKILL.md` at the root, frontmatter
       with `name` (the `skills-ref` rules, and equal to the directory name) and `description`
       required, the optional fields checked for type and length. Every relative link in
       `SKILL.md` (Markdown links and images) must resolve to a file inside the skill; a link that
       is missing or leaves the tree is an error and the editor shows it on the line. A body over
       the specification's recommended 5000 tokens is a warning.
     - **Limits:** 2 MiB per file, 16 MiB and 500 files per skill, six directory levels (OpenCode
       scans no deeper). Archive entries that escape the tree, symbolic links and special files are
       refused. A platform admin can lower or raise the limits.
     - The digest covers the whole tree (decision 2), and the delivery writes the whole tree
       (decision 8), so the harness's progressive loading works unchanged.
   - **Needs packages.** The image has only Python's standard library and no package network. A
     skill whose `compatibility` names packages, or whose scripts import non-standard modules, run
     `pip` or `npm`, or ship `requirements.txt` or `package.json`, is flagged **needs packages** on
     the version and in the picker. The flag does not block; it warns that the scripts will fail.
   - **Updates.** A skill added from a source keeps that source record.
     - A scheduled check (daily, and **Check now** on the skill page or `koinon skills check`)
       resolves the source's ref again. A newer commit, or a changed well-known digest, is fetched
       as a **new version**, scanned and gated as any other. It never applies itself.
     - The skill page shows the diff from the version agents use, and the agent page offers to
       propose the new version (decision 3). Agents move only through an activated agent version.
     - A skill edited in Koinon after a git import records the commit it is based on. The update
       check still reports upstream commits, and the diff shows upstream against the edited
       version, so a person decides whether to take the upstream change.

7. **Signatures are verified where present.** A skill that ships `skill.oms.sig` is verified with
   OpenSSF Model Signing in strict mode (unsigned extra files fail) against the trust roots a
   platform admin configures; NVIDIA's root is offered as a default. The version records "signed
   by" and the certificate identity. An invalid signature refuses the import. A valid one is shown,
   but it is integrity, not approval: the gate applies as usual.

8. **Delivery: agentd writes the pinned skills into the sandbox, as it writes AGENTS.md.**
   - Before each harness start, agentd writes the run's pinned, Approved skill trees to
     `/sandbox/.koinon/skills/<name>/`, each whole tree with its folders, so `SKILL.md`'s links to
     `references/`, `scripts/` and `assets/` resolve and the harness loads them as it needs them. It uses the pattern of `internal/agentd/instructions.go`:
     temporary file, rename, `umask 077`, and writes chunked under OpenShell's 4 MiB exec stdin cap.
     It removes anything else in that directory first.
   - **Digest checks.** agentd recomputes the directory's digest **before every prompt and after
     every turn**. Any difference, including an added, removed or changed file, is a policy failure:
     agentd records `run.skills_mismatch` (expected and found digests, the paths that differ) and
     fails the run. It does not restore and continue, because the changed content may already have
     been read. The next run writes the tree fresh.
   - **OpenCode.** The read-only config sets `skills: ["/sandbox/.koinon/skills"]`. `skills` never
     holds a URL, and a flavor test asserts it. The entry script receives the pinned names and
     renders permission rules that deny the `skill` action for every ID except them, so a skill the
     agent adds to the directory is neither listed nor loadable. The image ships no
     `/etc/koinon/opencode/skills`.
   - **Other harnesses.** The flavor descriptor (`internal/flavor`) gains `SkillsPath`, the
     directory the harness reads skills from (`/sandbox/.koinon/skills`, or a link agentd keeps to
     it, as `InstructionLink` does), and `SkillsAllowArg`, the launch switch that takes the pinned
     names. A flavor with an empty `SkillsPath` does not support skills. Every flavor uses the same
     delivery and the same checks.
   - **Residual risk.** OpenCode hot-reloads within a turn, so content an agent changes mid-turn can
     be read before the after-turn check fails the run. That gives the agent nothing it could not do
     directly in the same turn; persistence, the real gain, is closed by the fresh write and the
     check.
   - **old issue 58.** The same mechanism closes old issue 58: plugins become pinned, reviewed, digest-checked
     artifacts of the agent version that agentd writes beside the skills, instead of code loaded from
     the writable home. Which scanners apply to plugin code is decided in that issue's change.
   - **Removal condition.** OpenShell 0.1.5 removed custom volume claim templates
     (`internal/reconciler/render.go`). Once OpenShell supports custom volumes again, the skills come
     as a read-only mount, and agentd's writing and the per-prompt checks are removed. Koinon raises
     it with OpenShell upstream.

9. **OpenCode's built-in skills are turned off.** `-opencode.skill` joins the plugin negations in
   `opencode.jsonc`, which removes `opencode` and `report`. That ships in rc.6, before this ADR's
   build, and the old issue 84 test asserts that neither reaches the model.

10. **UI.**
    - **Library page**, with Platform, Org and Browse tabs (platform admins edit Platform): name,
      description, source, latest version, status, signed, needs-packages and update-available
      badges, and how many agents and templates use it.
    - **Add skill:** one box that takes any decision 6 line (`npx skills add …`, `owner/repo`, a git
      URL, a marketplace or well-known URL), shows what it resolved and lets the person pick
      skills; beside it, upload (archive or dropped folder), paste `SKILL.md`, and **New skill**,
      which opens the file-tree editor. Validation errors appear on the file and line.
    - **Browse:** search skills.sh and the configured catalogs, with **Add** on each result.
    - **Skill detail page:** the rendered `SKILL.md` and the whole file tree (sizes, executable
      bits, each file viewable), the source and **Check now**, versions with provenance, scan
      findings per scanner (severity, rule, file and line, scanner version), status and its history,
      the diff from the previous version, and where it is used. Approve and Block with a reason.
    - **Agent and template pages:** a skills section that assigns Approved versions, shows each
      skill's metadata token cost, licence, `compatibility` and flags, and proposes a new agent
      version.
    - **Settings:** the gate, threshold and judge, in the Platform area and in org settings, with
      the org page showing whether it overrides the platform.

11. **Tests.** CI calls no paid model.
    - The old issue 84 test stays as it is: skills in the home never load.
    - A flavor test: a pinned skill loads; a skill added to `/sandbox/.koinon/skills` is denied; a
      changed pinned file fails the run with `run.skills_mismatch`; the built-in skills and
      `/etc/koinon/opencode/skills` are absent.
    - `internal/skills`: the digest test vector, validation, archive escapes, caps and the
      needs-packages check.
    - The gate as a table test: each gate with and without findings, an incomplete scan, org over
      platform in both directions, Blocked under every gate, and a rescan that raises a finding.
    - The scan Job against the pinned scanner images with obviously fake fixture skills (benign and
      malicious), offline, with the judge on the fake model server.
    - The source parser as a table test over the `skills` CLI's and Claude Code's documented forms,
      including `@` and `#ref`, refused `npm` and `command` sources, and ignored install flags;
      discovery against fixture repositories served by the fake git host, with flat, catalog and
      marketplace layouts; well-known digest mismatches refused.
    - Multi-file trees: links that resolve, are missing or leave the tree; the limits; a tree with
      folders delivered whole and its digest unchanged.
    - Chart assertions (`hack/chart-check.sh`): the scanner images go through `koinon.imageRef`,
      the Job's egress is denied by default, and OSV is off by default.

12. **Not in scope.** Fetching skills at run time; skills from the agent's writable home; per-user
    (Idion) skills, which come with v0.2.

## Consequences

- Orgs get reusable skills, reviewed once and updated in one place, with a record of where every
  version came from and what the scanners said about it.
- The default is permissive. Under `auto`, every scanned skill is approved, and the scanners miss
  most deliberate attacks. Safety rests on the sandbox, pinning, the digest checks and Blocked. An
  org that wants a person in the loop must choose it.
- Because the org's setting wins, a platform admin cannot force a stricter gate on an org. The
  platform's levers are its own library's Block and the default.
- A skill version used by many agents has a wide blast radius. Blocked is the brake: one action
  stops every run that would use it.
- The judge costs the org tokens on every scan and rescan, and a scanner update rescans everything in
  use. The per-scan cap and the off-by-default judge bound that.
- Two scanner projects that release weekly become pinned inputs. Updating them is a release change
  that triggers rescans, which can move approved versions back to Needs review under the stricter
  gates.
- Editing a delivered skill now fails the run loudly, where AGENTS.md is restored quietly. An agent
  that writes to `/sandbox/.koinon/skills` by mistake loses its run.
- Adding a skill takes one pasted line, as it does on a laptop, but it does not install anything
  until scans have run and the gate allows it. The catalog browser and the source resolver are new
  egress from koinon-api to skills.sh, git hosts and well-known hosts; the browser can be turned
  off, and imports from public hosts need egress whatever the setting.
- Koinon follows two external grammars (the `skills` CLI's and Claude Code's), which change. The
  parser's table test pins the forms Koinon supports, and an unknown form is refused with the list
  of supported ones rather than guessed.
- Skills that need packages are imported but warned about. Supporting them needs an image or
  package decision of its own.
- agentd's write-and-check is a workaround for OpenShell's missing custom volumes, and it goes away
  with them.

## Alternatives

- **Per-agent skills only, in the agent version.** Simple and pinned, but every agent re-imports
  and re-reviews the same skill, and an update means finding every copy. Rejected; the library keeps
  per-agent pinning through references.
- **Skills baked into flavor images.** Read-only and simple to deliver, but every skill change is an
  image build and a release, orgs cannot add their own, and images grow with content that is not
  the harness's. Rejected.
- **Static scans only, no optional judge.** No cost and no model dependency, but by the vendors' own
  numbers static rules catch a small share of malicious skills (7.7% at HIGH for Cisco's rules). Not
  offering the judge would leave careful orgs no better tool. Rejected; static scans stay the
  always-on floor.
- **Snyk agent-scan.** It needs Snyk's cloud API and a token and sends skill content out. That
  breaks offline and air-gapped installs, sends org content to a third party, and cannot run in CI
  without a secret. Rejected.
