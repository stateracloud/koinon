# 0113. Agent package access: straight to allow-listed registries, in four levels

- Status: Accepted (direction by procoduck, 2026-11-11). Not built: delivery comes after the core release
- Date: 2026-11-11
- Area: runtime-sandbox
- Tags: egress, dependencies, registry, openshell
- Amends: [0015](0015-agent-environment-and-browser.md) (self-installs no longer go through a pull-through
  cache that Koinon runs), [0079](0079-dependency-cooldown.md) (an advisory minimum age for what agents
  install), [0080](0080-tool-set-matches-egress-and-escape-hosts.md) (the tool set follows four access
  levels)
- Relates to: [0006](0006-identity-layers.md) (credentials as provider records),
  [0045](0045-openshell-hardening-defaults.md), [0061](0061-image-pull-secrets-and-registry-mirrors.md),
  [0084](0084-leak-guard.md), [0091](0091-leak-guard-by-destination.md),
  [0112](0112-harness-definitions.md) (registry connections, the block list)
- Verification: Pending, no tracking issue yet: whether an OpenShell network rule can cover any program
  (decision 2)

## Context

Agents need to install tools and a project's dependencies to do their work. ADR 0015 decided that they
install into their home with ordinary tools (mise, uv, npm) "from allowlisted sources through a
pull-through cache", and left two questions open: which cache product, and how the network policy
expresses "allowlisted sources" when installs go through many programs. Nothing of it is built; a sandbox
has no web access today (`agentconfig.Config.WebEgress` returns false).

procoduck wants a default list that can be extended, support for company package proxies with a login and
a private CA, and does not want Koinon to rebuild Artifactory.

Facts checked on 2026-11-11 (sources under "Upstream verification"):

- **Cloud-hosted agents control installs by destination host, through an egress proxy.** None documents a
  package mirror it runs, a per-package list, a minimum age or lockfile enforcement. Claude Code's cloud
  environments publish a default list of 204 hosts, GitHub's Copilot cloud agent 217; Codex's preset is
  reported at about 70. The categories are the same: language registries, OS mirrors, container
  registries, code hosts and tool download sites.
- **The levels are the same everywhere:** none, a curated default with custom additions, and unrestricted.
- **Broad entries on a default list become exfiltration channels.** A documented attack on Codex used
  `azure.com` from its preset. Claude's list contains `*.amazonaws.com` and `*.googleapis.com`. Copilot's
  is narrower and scopes some entries by URL path.
- **Private registry logins follow two models:** environment variables inside the sandbox (Copilot, Cursor,
  Codex's setup phase), or a header injected at the egress proxy so that the secret never enters the
  sandbox (Claude's network secrets, E2B, Modal).
- **OpenShell already gives Koinon the second model:** egress is denied by default, a rule matches host,
  method and URL path, and a placeholder is swapped for the real credential on the way out.
- **npm, pnpm, Yarn, Bun, uv and pip each have a native minimum-release-age setting.** A setting inside the
  sandbox can be changed by the agent.
- **Small proxies with package semantics exist:** Verdaccio for npm (allow and block lists and a minimum
  age), Athens for Go modules, and newer single binaries that cover several ecosystems.

## Decision

1. **Agents install straight from allow-listed registries, read-only.** A sandbox may send read requests
   (GET and HEAD) to the registry hosts its level allows. A private registry's login is an OpenShell
   provider record, injected on the way out; it never enters the sandbox. The company's CA certificate is
   added to the sandbox's trust store: it is public material.
2. **How the rule names the calling program is decided after a spike** (Verification). Installs go through
   many programs (`node`, `uv`, `curl`, `git`), and today's rules name each program's real path.
3. **Koinon builds no package proxy.** An install that wants per-package control, a cache or an enforced
   minimum age points its registry connection (ADR 0112, decision 16) at a proxy the customer already runs
   or a small open-source one. Koinon documents this and tests one small option as a reference setup.
4. **Four levels, set per agent:**

   | Level | The sandbox may reach | Who sets it |
   | --- | --- | --- |
   | Off | no registry | org admin |
   | Default | Koinon's default list | org admin |
   | Default plus custom | the default list and hosts the org adds, within a ceiling the platform admin sets | org admin |
   | Unrestricted | any host | org admin, once a platform admin has enabled the level for the install |

5. **The default list is narrow:** language registries, OS mirrors and tool download sites, scoped by URL
   path where a host serves more than packages, and with no wildcard over a cloud provider's domain.
6. **Unrestricted is off for an install until a platform admin enables it**, with the consequence stated:
   an unrestricted agent can send data to any host, so Koinon's egress controls no longer protect it. Each
   such agent shows a badge, and setting the level is audited. Unrestricted lowers only Koinon's own
   egress limit for that agent. It does not change [0045](0045-openshell-hardening-defaults.md): the policy
   still runs with `enforcement: enforce` in every tier, no secret enters the sandbox environment, no
   credential goes uninspected, and this level does not relax the default-deny NetworkPolicy of that ADR.
   Nothing here leaves room to weaken [0045](0045-openshell-hardening-defaults.md).
7. **The tool set follows the level** (ADR 0080): a harness's built-in web tools are offered only when the
   sandbox can use them, which is the unrestricted level.
8. **A minimum age is set by default and is advisory.** Koinon configures each package manager's native
   setting in the sandbox to the three days of ADR 0079. It stops the accidental install of a release
   published minutes ago. The agent can change it; the UI and the documentation say so. Only a proxy
   (decision 3) enforces it.
9. **The platform block list of ADR 0112, decision 19, also applies here** where a rule can express it (a
   package's path on a registry host).
10. **Harness delivery is not affected.** The control plane fetches harness packages itself (ADR 0112,
    decision 12).

## Alternatives considered

- **A package proxy that Koinon builds.** Per-package lists, an enforced minimum age and a cache on every
  install. Rejected: it is the Artifactory-like component procoduck does not want to build, and no
  cloud-hosted agent runs one.
- **Bundle an existing proxy in every install.** Control and caching without writing one, at the price of
  extra services to deploy, upgrade and secure, small installs included.
- **The agent asks Koinon and the control plane delivers.** Strongest control and audit. An ordinary
  `npm install` in a repository would not work, so real projects could not be built.
- **Three levels, without unrestricted.** Keeps the egress model for every agent. procoduck chose to offer
  the level, gated by the platform admin (decision 6).
- **A broad default list.** More works without configuration, and the default then contains known
  exfiltration channels.
- **No minimum age without a proxy, or a proxy required for any install.** The first lets agents install
  releases minutes after publication by default. The second leaves a laptop or small install with agents
  that cannot install anything.

## Consequences

- Positive: no new component; the pattern is the one cloud-hosted agents use, with the stronger of their
  two login models.
- Positive: a company's existing proxy works as it is, behind the registry connection.
- Negative: without a proxy there is no per-package control, no cache and no enforced minimum age.
- Negative: the default list is a product Koinon has to keep current.
- Negative: an unrestricted agent is outside the egress model, and the leak guard by destination (ADR
  0091) has no fixed set of sinks to reason about for it.
- Negative: installs repeat after a home is restored, because home checkpoints exclude package caches and
  `node_modules` (ADR 0032).
- Follow-ups: the default list; the level on the agent and its API; the registry connection as a provider
  record with a CA bundle; the reference proxy setup; the amendments to ADR 0015 and 0080 below.

## Open questions

No tracking issue exists yet for these; each needs one (ADR 0046, decision 7).

- The spike of decision 2, and what Koinon does if a rule cannot cover any program.
- Whether `environment.lock` and the promotion of common tools by org admins (ADR 0015) stay as decided.
- How the leak guard treats an unrestricted agent.

## Upstream verification

Checked 2026-11-11 by reading documentation and source; nothing was run. Several documentation hosts were
not reachable, so Codex cloud and Cursor rest on search summaries.

- Claude Code cloud environments: https://code.claude.com/docs/en/cloud-environments.md (default allowed
  domains, network secrets, levels None, Limited and Full).
- GitHub Copilot cloud agent: `github/docs`, `content/copilot/reference/copilot-allowlist-reference.md`
  and `customize-the-firewall.md`.
- OpenAI Codex: `openai/codex`, `codex-rs/network-proxy/README.md` (the local proxy: read-only mode,
  credential substitution).
- E2B and Modal SDK source (header rules applied outside the sandbox).
- OpenShell: `third_party/openshell/proto/sandbox.proto` (L7 rule: method, path, query; `NetworkBinary`
  has a path only).
- Package managers: npm `min-release-age`, pnpm `minimumReleaseAge`, Yarn `npmMinimalAgeGate`, Bun
  `minimumReleaseAge`, uv `exclude-newer`, pip `--uploaded-prior-to`, each read in its repository.
- Proxies: Verdaccio 7.1.0 (`@verdaccio/package-filter`, `minAgeDays`), Athens v0.20.0 (`FilterFile`).

## Links

- [0015](0015-agent-environment-and-browser.md), [0080](0080-tool-set-matches-egress-and-escape-hosts.md),
  [0112](0112-harness-definitions.md)
- `internal/reconciler/plugins.go` (today's `npm_registry` rule), `internal/agentconfig/agentconfig.go`
