# 0044. In-sandbox credentials as a gated exception

- Status: Accepted; amended 2026-11-11 (below)
- Date: 2026-10-21
- Amends: ADR 0006 (identity layers), ADR 0013 (model access, GitHub Copilot), ADR 0015 (agent
  environment and browser)

> Note (2026-10-26): v0.1 ships OpenCode only ([0059](0059-v01-opencode-only.md)); mentions of Hermes and pi below are history.

## Context

Koinon's core promise is that real credentials stay outside the sandbox (decisions 6 and 15).
Decision 13 still listed "GitHub Copilot via OpenCode" for v1, with subscriptions personal-only.

Research on 2026-10-21: no model gateway supports per-user Copilot login. agentgateway's `copilot`
provider takes a static token; Bifrost uses GitHub App endpoints GitHub has not published; OpenCode,
Hermes and pi each do their own device-flow login, which stores a token inside the sandbox.

procoduck decided Koinon must support both a gateway-held credential and in-sandbox auth, because other
use cases besides Copilot will need in-sandbox auth.

## Decision

1. Two modes are supported:
   - **Gateway-held**: the credential (for example an org Copilot token) is an agentgateway provider
     credential and never enters the sandbox (ADR 0040). This is the default whenever it is possible.
   - **In-sandbox**: the credential lives inside the sandbox. This is an explicit exception.
2. **Enabling in-sandbox auth.** A platform admin allowlists in-sandbox auth types platform-wide
   (for example `copilot-device-flow`). An org admin or owner opts in per agent, after an explicit
   "credential inside sandbox" warning. Enabling is audited and shown on the agent card.
3. **Obtaining.** A human completes the login through takeover (device code or live view), as with
   the browser in decision 15. The agent never receives the user's password or a pasted token in
   chat.
4. **Storage.** The token is stored in an encrypted path on the home volume, treated as a secret in
   checkpoints (decision 32), and excluded from export.
5. **Egress.** Network policy limits the binary using the credential to the provider's hosts.
6. **Revocation.** Revoking from the UI wipes the encrypted path and, where the provider allows it,
   revokes the token upstream.
7. **Scope.** Personal-org agents only by default. Team orgs only if the platform admin allows it and
   the vendor's terms permit it (vendor terms review, open item #4).
8. **Timing.** Both modes ship in v0.2 with the connector framework; not in the v0.1 demo.

## Alternatives considered

- **Gateway-held only**: keeps the promise absolute, but blocks Copilot subscriptions and any tool
  whose auth cannot be brokered.
- **Secure input to Vault, mounted at start**: the human pastes a token through a secure input
  (decision 21) and Koinon mounts it. Rejected in favour of takeover because many flows (device code,
  OAuth in a browser) do not yield a pasteable token, and it would create a second storage path.
- **Any org by default**: rejected; subscription terms are usually personal.
- **Platform admin enables per agent**: too heavy for personal agents; the platform allowlist keeps
  control.

## Consequences

- Positive: Copilot and similar subscription or tool logins become possible without weakening the
  default model.
- Negative: an agent with an in-sandbox credential can misuse it within its allowed hosts; the
  escape suite must cover exfiltration attempts for these agents. _(inferred)_
- Negative: home-volume encryption and the export exclusion become security-relevant code.
- Follow-ups: auth-type allowlist in platform settings; agent-card badge; takeover flow for device
  codes; encrypted path in flavor images; vendor terms review before release.

## Open questions

- GitHub Copilot terms of service for agent and gateway use (open item #4).
- Which other auth types to allowlist first.
- Encryption key custody for the encrypted path (per agent key in Vault?).

## Amendment (2026-11-11): the exception is narrowed to credentials that cannot be held outside

Direction agreed with the maintainer on 2026-11-11; nothing of this ADR was built before the
amendment, so no code or data changes with it.

### Why

- **The premise has moved.** This ADR was written when no model gateway could hold a per-user login.
  Koinon has since decided to own its gateway ([0097](0097-koinon-owned-gateway.md)) and to be the
  OAuth client for MCP sign-ins, with the tokens held by koinon-api and the gateway only
  ([0096](0096-mcp-oauth-sign-in.md)). A login that Koinon can run and hold outside the sandbox no
  longer needs an exception.
- **The exception was read as a general permission.** New features kept pointing at it as the place
  where a credential in the sandbox would be allowed (a human shell in the sandbox, a harness that
  logs in with a vendor subscription). The rule "no secrets in sandboxes" is only a rule if its
  exception is narrow and named.
- **The storage rule promised more than it gives.** The harness must read the token to use it, and
  the agent runs as the harness. Encryption of the path protects the token at rest (the volume, a
  checkpoint, an export); it does not protect it from the agent.
- **The open questions stayed open.** Key custody, vendor terms and the first auth types were never
  answered, because no concrete type forced an answer.

### Decision

1. **Outside the sandbox is the rule, in this order.** A credential is held as an OpenShell provider
   record behind a placeholder, or by Koinon's gateway (a static provider credential, or a sign-in
   that Koinon runs and stores, following the pattern of ADR 0096). In-sandbox is allowed only for a
   named auth type for which both are shown to be impossible. "Easier" and "the harness already does
   it" are not reasons.
2. **No generic allowlist framework.** Decision 2's platform-wide list of in-sandbox auth types is
   not built as a framework that types are added to by configuration. Each auth type that uses the
   exception is added by its own ADR or by an amendment to this one, which records why the
   credential cannot be held outside, which hosts its binary may reach, and how it is revoked. The
   platform setting then offers exactly those types. The per-agent opt-in, the warning, the audit
   entry and the badge on the agent card stay as decided.
3. **Subscription and provider logins are no longer the example.** The default for a model or tool
   login is a sign-in held outside the sandbox. `copilot-device-flow` is not a qualifying type until
   the verification below shows that Koinon's gateway cannot hold it. The second way of ADR
   [0013](0013-model-access.md) decision 4 (Copilot as an in-sandbox credential) holds only under
   that condition.
4. **Browser session state is the inherent case.** Cookies of the in-sandbox browser exist in the
   sandbox by nature. They keep the rules of ADR [0015](0015-agent-environment-and-browser.md) and
   are decided with the browser work, not here.
5. **What the controls are.** Storage encryption (decision 4) covers the volume, checkpoints and
   exports. The controls against misuse by the agent are the egress limit on the binary that uses
   the credential (decision 5) and revocation (decision 6). An auth type whose use cannot be limited
   to the provider's hosts does not qualify.
6. **Not a precedent.** A feature that would put a credential into a sandbox does not cite this ADR
   as its permission; it needs its own decision under point 2. A credential that a person types or
   pastes into a sandbox is not covered by this ADR.
7. **Timing.** This replaces decision 8: no release is assigned. Nothing of the exception is built
   until the first qualifying auth type has its decision.

Decisions 3 (obtained by a human through takeover), 4, 5, 6 and 7 (personal-org agents by default)
are unchanged.

### Pending verification

No tracking issue exists yet for these; each needs one (ADR 0046, decision 7).

- Whether Koinon's gateway can run and hold a GitHub Copilot device-flow login per user (the token
  exchange, the refresh, and GitHub's terms for agent and gateway use). Not checked for this
  amendment.
- Whether a vendor harness that logs in with a subscription (Claude Code, Codex) can be given a
  credential held outside the sandbox, or whether its login is bound to the vendor's own client.
- A sign-in for a model provider held by the gateway is not designed: ADR 0096 covers MCP servers
  only. It needs its own decision before point 3 can be used.

### Consequences

- Positive: the hard rule has one inherent exception (the browser) and otherwise only named ones,
  each with its reason on record.
- Positive: the encrypted path, its key custody, the checkpoint and export handling and the extra
  escape cases are not built ahead of a need.
- Negative: until a gateway-held sign-in for model providers exists, personal subscriptions cannot
  be used at all. Before the amendment they were planned for v0.2.
- Negative: every new auth type costs a decision instead of a configuration entry.

## Upstream verification

Checked 2026-10-21: agentgateway `copilot` provider (static, non-refreshing credential); Bifrost
Copilot provider docs; OpenCode `/connect` device login; Hermes `copilot` and `copilot-acp`
providers; pi `COPILOT_GITHUB_TOKEN`.

## Links

- https://github.com/agentgateway/agentgateway/tree/main/examples/llm-copilot
- https://github.com/maximhq/bifrost/blob/main/docs/providers/supported-providers/github-copilot.mdx
- https://opencode.ai/docs/providers/
- ADR 0006, ADR 0013, ADR 0015, ADR 0021, ADR 0032, ADR 0040
