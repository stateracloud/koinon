# 0112. Harness definitions: any harness that speaks ACP, described as data

- Status: Accepted (direction by procoduck, 2026-11-11). Not built: delivery comes after the core
  release, except the groundwork under "Timing"
- Date: 2026-11-11
- Amends: [0035](0035-v01-flavors.md) and [0059](0059-v01-opencode-only.md) (the flavor concept and how
  a harness is added), [0079](0079-dependency-cooldown.md) (the cooldown also covers harness packages)
- Relates to: [0016](0016-harness-and-deployment.md) (agentd drives the harness over ACP),
  [0032](0032-agent-home-checkpoints.md) (the upload channel and the object store),
  [0045](0045-openshell-hardening-defaults.md), [0051](0051-session-home-generations.md) (fresh session
  with a handoff), [0052](0052-opencode-v2-flavor.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md)
  (one retrying layer), [0068](0068-prompt-caching-always-on.md), [0069](0069-native-model-formats.md),
  [0080](0080-tool-set-matches-egress-and-escape-hosts.md), [0093](0093-koinon-core-opencode-plugin.md),
  [0097](0097-koinon-owned-gateway.md), [0113](0113-agent-package-access.md),
  [0114](0114-model-credentials-by-type.md)
- Verification: Pending, no tracking issue yet: the spike on detecting a changed harness in the agent's
  home (decision 14)

## Context

Koinon ships one harness, OpenCode, as a sandbox image with a Go descriptor, an entry script and two
plugins (ADR 0052, 0059, 0093). Adding a harness means a table entry in `internal/flavor`, a database
enum value, a proto enum value, a schema enum value and a Koinon-built image, so it needs a Koinon
release. Comparable products let people bring the harness they already use. procoduck wants a core that
has no opinion about the harness.

Facts checked on 2026-11-11 (sources under "Upstream verification"):

- **OpenShell gives Koinon no way to add a volume to a sandbox**, and the admission policy and ADR 0045
  deny shared claims. A harness reaches a sandbox in its image or in the agent's home, nowhere else.
- **Restoring a home already pushes an archive into a sandbox** over OpenShell's upload channel (ADR
  0032): 16 s from run queued to restored for a 210 MB home.
- **The home is fully writable by the agent**, and the filesystem policy is per sandbox, not per process.
- **Network rules name the real path of the executable.** A harness written in JavaScript runs as `node`.
- **Eight harnesses were read against the controls OpenCode gives** (OpenCode, Claude Code, Codex, Gemini
  CLI, pi, Goose, Hermes, Copilot CLI). All can be given instructions. Gemini CLI, Goose and Hermes cannot
  cleanly ignore a repository's own agent files. Claude Code speaks only Anthropic Messages, Codex only
  the OpenAI Responses API, Gemini CLI only Gemini's format. Most let the retry count be capped; almost
  none lets the policy be replaced. Every one except Hermes is on npm and should install with scripts off.
- **Products that drive several harnesses** either use ACP only (Zed, OpenHands, Goose) or keep a native
  adapter per harness at roughly 8,000 to 12,000 lines each (T3 Code). T3 Code's own rule is ACP first,
  with a dedicated driver only for behaviour the spec does not cover. What ACP v1 lacks (steering inside a
  turn, forking a session, usage per step) Koinon does not use: it has no steering, and usage comes from
  the gateway.
- Claude Code and Codex reach ACP through adapters kept under the ACP organisation.

"Flavor" is defined as "Koinon's tested packaging of one harness as a sandbox image" (GLOSSARY.md). A
harness someone brings is neither tested by Koinon nor an image, and the word suggests variants of one
product.

## Decision

### Boundary and words

1. **An agent selects a harness. A harness definition is Koinon's record of how to obtain and run one.**
   A definition is built-in (shipped in the sandbox image) or custom (registered on an install). The term
   "flavor" is retired; the rename is part of the groundwork under "Timing".
2. **Koinon owns everything around the agent loop, whatever the harness:** the sandbox and its network
   policy, credentials, model routing and budgets (ADR 0114), approvals and the tools that reach external
   systems, the task and run model, the agent's home and its instructions. The harness owns the loop, its
   built-in shell, file and search tools, and its session format. Built-in tools are accepted as they are:
   the sandbox is the boundary (ADR 0093, decision 2).
3. **ACP over stdio on a non-TTY exec is the only way Koinon drives a harness.** A harness without native
   ACP brings an adapter in its package. A native driver is considered only when a harness needs behaviour
   the spec cannot express.
4. **A harness is fixed for the life of an agent's sessions.** Changing it is a fresh session with a
   handoff (ADR 0051). Koinon never reads another harness's session files.

### Contract

5. **Two tiers.**
   - *Experimental*: the harness speaks ACP without a terminal, takes its model endpoint, model name and
     key from Koinon, and never prompts a person directly. Nothing else is required. The definition is
     marked, and each behaviour it lacks is stated on it.
   - *Verified*: it also passes every other check: reloads a session by ID, cancels a turn, makes Koinon's
     MCP tools reach the model, and receives Koinon's instructions. Templates default to verified
     definitions. A platform admin can restrict an install to verified definitions.
6. **Optional capabilities are declared, tested and shown:** following Koinon's retry policy, sending
   prompt-cache markers, dropping built-in web tools, ignoring a repository's own agent files, and
   announcing readiness.
7. **A missing capability narrows where a harness may be used. It never relaxes a guarantee.** A harness
   that cannot send cache markers is not offered the models that need them (ADR 0068). One that cannot
   drop its web tools cannot serve an agent without web access (ADR 0080). One that cannot follow Koinon's
   retry policy has its own retries capped as low as it allows, and agentd does not retry on top (ADR
   0056).
8. **Instructions.** Koinon's instructions must reach every harness. A harness that can ignore a
   repository's own agent files does so. One that cannot carries a warning on its definition and on its
   agents. A platform admin setting lets agents load a repository's own agent files on purpose; the
   setting states that repository content can then change an agent's behaviour.
9. **Model formats.** A definition declares the wire formats its harness speaks. An agent is limited to
   models the gateway serves in one of them. The gateway gains the OpenAI Responses API so that Codex is in
   scope (a follow-up to ADR 0069 and 0097). Koinon does not translate between formats.

### Definition and delivery

10. **A definition is data only:** the npm package, an exact version, the integrity hashes Koinon recorded
    when it fetched it, the command to run, and templates for arguments, environment variables and config
    files, with placeholders for what Koinon supplies (model URL, model name, MCP URL, instruction file
    path, session ID). It also lists the model formats and the optional capabilities it claims. There are
    no version ranges, no "latest", and no script written by the registrant.
11. **The first version supports npm packages only, with Node.js in the base sandbox image.** Other source
    kinds (release archives, Python packages) are later additions to the same format.
12. **The control plane fetches; the sandbox receives.** A Koinon component downloads the package with
    install scripts off, using the registry login and the private CA there (decision 16). It stores the
    result as a snapshot: in the object store when the install has one (ADR 0032), otherwise on a volume
    owned by the control plane. When an agent's sandbox is created, Koinon pushes the snapshot into it
    over the upload channel. A sandbox needs no registry access for its harness, and no registry login
    reaches a sandbox.
13. **The three-day cooldown of ADR 0079 applies to harness packages**, on by default. A platform admin can
    turn it off for the install or waive one version.
14. **A harness in the agent's home can be changed by the agent.** The boundaries of decision 2 do not
    depend on it. Whether Koinon checks the installed files against the recorded hashes before each start,
    and reinstalls on a mismatch, is decided after a spike (Verification).
15. **Known limit: an interpreter-based harness shares its network access with its interpreter.** The rule
    names `node`, so any Node script the agent runs reaches the same hosts as its harness: the model and
    MCP gateways, which still require the agent's own key and count against its own budget. This is
    accepted and stated on the definition.
16. **Registries.** The public npm registry is the default. A platform admin can replace it with a registry
    connection (URL, login in the secret store, CA bundle, registries per scope) and can forbid the public
    registry. An org can add its own connection for its own definitions when the platform admin allows it.
    ADR 0113 uses the same connections for agent installs.
17. **OpenCode stays in the sandbox image** and is described by the same definition format, so a new
    install runs an agent with no registry and no snapshot store. Its Koinon plugins (ADR 0093) stay
    extras of that one definition.
18. **Updates.** A new version of a definition reaches an agent when its sandbox is next rebuilt, never
    during a run or while a run is parked. Koinon tries to load the session; if the new version cannot read
    it, the agent starts a fresh session with a handoff. The previous snapshot is kept so that an admin can
    go back.

### Governance and checks

19. **Two levels.** A platform definition is available to every org. An org definition is visible to that
    org only; a personal org covers an individual. Definitions are usable until blocked: the platform admin
    has a block list of packages, versions and whole definitions, and a blocked one stops new starts.
20. **Checks run on every registration and every change.** Koinon starts a sandbox with the harness and
    runs the contract suite against the fake model server. The stored result decides the tier and the
    capability list, and is shown on the definition. What the author claims decides nothing.
21. **Koinon ships and tests a small set of definitions:** OpenCode, then Claude Code and Codex through the
    ACP organisation's adapters. They are tested in CI and re-pinned on a schedule.

### Timing

22. **Build after the core release.** Before it, only what avoids rework later: the rename from flavor to
    harness in templates, API and database while there is no upgrade promise (ADR 0099), and moving
    OpenCode's Go descriptor to the definition format.

## Alternatives considered

- **An image per harness, built by Koinon or by the user.** The agent could not alter its harness and
  starts would be fast. Rejected by procoduck: Koinon cannot bake an image for every harness, and asking a
  client to build one is unreasonable.
- **A shared read-only volume holding the harness.** Not available: OpenShell has no such input and the
  admission policy denies it. Asking upstream for it would tie this work to another project's schedule.
- **Install inside the sandbox with npm.** Uses stock npm, but the sandbox needs registry access and a
  login, and the first copy comes from a place the agent could have touched.
- **Native adapters for Claude Code and Codex.** Full access to each harness's features, at thousands of
  lines per harness and breakage on vendor protocol changes, which is the cost ADR 0059 removed.
- **A second protocol (vendor stream formats) or a terminal with screen reading.** The first doubles the
  driver and the event mapping. The second gives coarse state and breaks on harness UI releases.
- **One strict list of requirements.** Predictable, but a new harness that misses one requirement could
  not even be tried.
- **Relax a guarantee with a warning when a capability is missing.** Widest use, but ADR 0056, 0068 and
  0080 would each need an exception.
- **The gateway translates model formats.** Any harness could use any model, but translation loses caching,
  thinking and tool-call detail, and Claude Code's own documentation says non-Claude models through a
  gateway are unsupported.
- **A definition with a start script.** Describes anything, but a script is code a platform admin has to
  trust and nothing about it can be validated without running it.
- **Keep the word flavor, widened or beside a new term.** One would stretch a word that already misleads;
  the other doubles the concept through templates, API and database.

## Consequences

- Positive: a harness is added without a Koinon release, and the default harness exercises the same
  definition format.
- Positive: every existing guarantee stays true for every agent; weaker harnesses serve fewer cases.
- Positive: registry logins and private CAs stay in the control plane.
- Negative: Koinon tracks two more harnesses again (decision 21), as definition files instead of images.
- Negative: Koinon has to fetch npm packages itself, including dependency resolution with scripts off.
- Negative: a custom harness on an install without an object store needs a control-plane volume, a second
  storage path to build and test.
- Negative: the gateway grows by one wire format.
- Negative: pi cannot be verified today: its ACP adapter does not forward client MCP servers and passes no
  flags to pi.
- Follow-ups: the rename and the descriptor move (decision 22); the Responses API in the gateway; the
  definition schema; the fetcher and the snapshot store; the registration check; the GLOSSARY.md entries
  for Harness and Flavor, with the rename.

## Open questions

No tracking issue exists yet for these; each needs one (ADR 0046, decision 7).

- The tamper spike of decision 14: what a check before each start costs, and what Koinon does on a
  mismatch.
- How much of npm's resolution the fetcher needs when install scripts are off and versions are pinned.
- The size and number of snapshots to keep for going back (decision 18).

## Upstream verification

Checked 2026-11-11 by reading source and documentation; nothing was run.

- Koinon: `third_party/openshell/proto/openshell.proto` (no volume or pod-template input;
  `volume_claim_templates` reserved), `charts/koinon/templates/sandbox-policy.yaml`,
  `internal/reconciler/render.go` (filesystem policy), `internal/agentd/checkpoint.go` and ADR 0032 (upload
  channel, timings), `internal/flavor/flavor.go` (binary matching).
- Harnesses, each at the commit read: OpenCode 2.2.7, Claude Code 2.1.322 with
  `agentclientprotocol/claude-agent-acp` 0.106.0, Codex with `agentclientprotocol/codex-acp` 2.6.0, Gemini
  CLI 0.70.0-nightly, pi 1.5.2 with `svkozak/pi-acp` 0.0.34, Goose 1.59.0, Hermes, Copilot CLI 1.0.109
  (closed source: documentation only).
- Products: T3 Code, Zed, OpenHands, Goose, Happy, Nimbalyst, Vercel's harness layer, Rivet Sandbox Agent,
  coder/agentapi (deprecated 2026-09-13), herdr.
- ACP: protocol v1 is stable; v2 has been a draft since 2026-07-20 and adds steering.

## Links

- [GLOSSARY.md](../../GLOSSARY.md) (Harness, Flavor)
- `internal/flavor/flavor.go`, `flavors/opencode/README.md`
- https://github.com/agentclientprotocol/agent-client-protocol
- https://github.com/agentclientprotocol/claude-agent-acp, https://github.com/agentclientprotocol/codex-acp
- https://github.com/pingdotgg/t3code (`docs/internals/adding-a-provider.md`)
