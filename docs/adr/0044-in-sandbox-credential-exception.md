# 0044. In-sandbox credentials as a gated exception

- Status: Accepted
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

Andre decided Koinon must support both a gateway-held credential and in-sandbox auth, because other
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

## Upstream verification

Checked 2026-10-21: agentgateway `copilot` provider (static, non-refreshing credential); Bifrost
Copilot provider docs; OpenCode `/connect` device login; Hermes `copilot` and `copilot-acp`
providers; pi `COPILOT_GITHUB_TOKEN`.

## Links

- https://github.com/agentgateway/agentgateway/tree/main/examples/llm-copilot
- https://github.com/maximhq/bifrost/blob/main/docs/providers/supported-providers/github-copilot.mdx
- https://opencode.ai/docs/providers/
- ADR 0006, ADR 0013, ADR 0015, ADR 0021, ADR 0032, ADR 0040
