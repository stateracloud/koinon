# 0114. Model traffic always through the gateway; credentials handled by type

- Status: Accepted (direction by procoduck, 2026-11-11). Not built: delivery comes after the core
  release. Decision 5 names no qualifying auth type yet (Verification)
- Date: 2026-11-11
- Amends: [0013](0013-model-access.md) (decision 4, GitHub Copilot), [0044](0044-in-sandbox-credential-exception.md)
  (how its amendment of 2026-11-11 applies to subscription logins)
- Relates to: [0005](0005-agent-identity.md), [0019](0019-personal-agents-and-teams.md),
  [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md),
  [0084](0084-leak-guard.md), [0096](0096-mcp-oauth-sign-in.md), [0097](0097-koinon-owned-gateway.md),
  [0112](0112-harness-definitions.md)
- Verification: Pending, no tracking issue yet: a legal read of Anthropic's and OpenAI's terms for
  subscription logins on a hosting platform (decision 5); whether Codex's static access token is available
  outside Enterprise workspaces

## Context

ADR 0112 lets an install run harnesses other than OpenCode, among them Claude Code and Codex. Many people
use those with a subscription instead of an API key, and many companies buy seats or organisation plans.
The question was whether Koinon can keep its rule that a harness never calls a model provider directly,
and its rule that no credential lives in a sandbox, for those users.

Facts checked on 2026-11-11 (sources under "Upstream verification"):

| Subscription | Traffic through a gateway | Credential outside the sandbox | Vendor terms |
| --- | --- | --- | --- |
| GitHub Copilot | Yes | Yes: a static token can be supplied from outside, and agentgateway has a provider for it. GitHub also offers server-to-server tokens billed to the organisation | GitHub documents server-side and third-party client use |
| ChatGPT plan (Codex) | Yes: a custom provider with `requires_openai_auth` sends the login to the configured URL | No for a normal login: Codex parses, refreshes and rewrites its tokens itself | No primary text obtained |
| Claude plan (Claude Code) | Yes: documented by Anthropic for `ANTHROPIC_BASE_URL` without a gateway credential | Technically plausible with a long-lived token; contractually not (below) | Quoted below |

Anthropic's terms, fetched 2026-11-11: "developers may not collect, store, or intermediate Claude.ai
credentials or session tokens — sign-in to a Claude account must complete through Anthropic's own flow."
The same page says the restriction does not "prevent an end user from signing in to the unmodified Claude
Code binary with their own Claude subscription, including where a platform hosts Claude Code".

All three return token usage in their responses, so a gateway can keep a ledger. A subscription has no
price per token.

"Subscription" covers three different things: a personal plan, a per-person seat on a company plan, and a
credential that belongs to an organisation.

## Decision

1. **Model traffic always goes through Koinon's gateway.** No harness calls a model provider directly, and
   there is no exception. Budgets, the spend ledger and the leak guard therefore hold for every harness.
2. **Koinon handles a model credential by its type, not by the name of the plan:**

   | Type | Example | Where it lives | Who may use it |
   | --- | --- | --- | --- |
   | API key | any provider key | gateway | agents of the org that owns it |
   | Organisation credential | Copilot server-to-server token billed to the organisation | gateway | any agent of that team org |
   | Personal token the gateway can hold | a GitHub Copilot personal plan | gateway | that person's agents, for their own work |
   | Personal login the client manages itself | a Claude plan or seat with Claude Code; a ChatGPT plan with Codex | in the sandbox, as a named exception of ADR 0044 | that person's agents, for their own work |

3. **"That person's agents, for their own work" means:** agents in the person's personal org (ADR 0019),
   and runs that person started or scheduled. No team org, and no run triggered by another person or by
   another agent. A per-person seat on a company plan is treated the same wherever it was bought.
4. **Sign-in for the last type is relayed, not held.** Koinon runs the harness's own login command in the
   sandbox and shows its sign-in link or device code in Koinon's UI. The person signs in on the vendor's
   site in their own browser. Where the harness asks for a code in return (Claude Code), the person types
   it into Koinon, which passes it to the harness. Koinon stores no subscription credential outside the
   sandbox. This is the human takeover of ADR 0044, decision 3, without a live view.
5. **Each in-sandbox login type is its own named exception under ADR 0044**, added by its own decision
   after a legal read of the vendor's terms. This ADR names none. The read has to answer whether relaying a
   code (decision 4) and forwarding traffic (decision 1) count as "intermediating" under Anthropic's terms,
   and what OpenAI's terms say about a ChatGPT-plan login on a platform several people share.
6. **GitHub Copilot needs no exception.** Its token is a gateway credential. This replaces ADR 0013,
   decision 4, second way (Copilot as an in-sandbox credential) and answers the first pending point of ADR
   0044's amendment for Copilot: the gateway can hold it.
7. **Budgets for subscription agents count tokens and requests.** The ledger records usage for every
   request as it does today, with cost shown as covered by the subscription. Limits for these agents are
   set in tokens and requests. Koinon shows the plan-limit state the vendor returns. Money budgets do not
   apply to them, and no notional price is used.

## Alternatives considered

- **Let a subscription harness call its vendor directly.** Least work; those agents would have no budget,
  no ledger and no leak guard, which removes most of what Koinon adds.
- **No in-sandbox login at all.** Keeps the hard rule absolute. Copilot would work; Claude and ChatGPT
  plans could only be used through API keys.
- **The person supplies a long-lived token.** Simplest to build. Koinon would then store a subscription
  credential, which Anthropic's terms forbid a third party to do.
- **Wait for the live view** (ADR 0022). Koinon would touch no code or token, and the work would depend on
  a large feature that is not built.
- **Seats usable in team orgs.** Matches how employees work in teams, but a personal login would sit in a
  shared org's sandbox, and the likeliest reading of the vendors' terms treats that as sharing.
- **Any trigger for personal-plan agents.** More useful; other people's work would then consume one
  person's plan.
- **A notional price per token.** One budget mechanism, with numbers nobody pays.

## Consequences

- Positive: the rule "all model traffic through the gateway" has no exception, so the guarantees built on
  it need none.
- Positive: organisation credentials and Copilot work without any credential in a sandbox.
- Negative: Claude-plan and ChatGPT-plan logins cannot be offered until legal review clears each one.
- Negative: a personal login in a sandbox is readable by the agent; the controls are the egress limit and
  revocation, as ADR 0044's amendment says.
- Negative: the gateway has to forward subscription traffic exactly as each vendor requires (for
  Anthropic, the `Authorization` header and the OAuth capability in `anthropic-beta`), and it gains a
  Copilot provider with the plan-specific hosts.
- Negative: budgets get a second unit (tokens and requests) beside money.
- Follow-ups: the legal read; one decision per login type; the sign-in relay; token and request limits in
  the budget hierarchy; the Copilot and organisation credentials in the gateway (ADR 0097).

## Open questions

No tracking issue exists yet for these; each needs one (ADR 0046, decision 7).

- The legal read of decision 5.
- Whether Codex accepts a static access token outside Enterprise workspaces, which would move the ChatGPT
  plan to the gateway-held type.
- How a relayed sign-in is renewed when the vendor ends the session.
- What the leak guard does with traffic it forwards under a personal login.

## Upstream verification

Checked 2026-11-11 by reading documentation and source; nothing was run.

- Anthropic: https://code.claude.com/docs/en/legal-and-compliance (the quotations),
  https://code.claude.com/docs/en/llm-gateway and `llm-gateway-protocol` (base URL without a gateway
  credential; headers a gateway must forward), https://code.claude.com/docs/en/authentication.
- OpenAI Codex: `openai/codex`, `codex-rs/login`, `codex-rs/model-provider-info/src/lib.rs`
  (`requires_openai_auth`), `auth/manager.rs` (refresh and storage), `codex-api/src/sse/responses.rs`
  (usage fields).
- GitHub Copilot: `github/docs`, `authenticate-copilot-cli.md`; `github/copilot-sdk`,
  `docs/auth/authenticate.md` and `server-to-server-tokens.md`; agentgateway's `copilot` provider.
- Not verified: OpenAI's terms for a shared platform; Claude Code's behaviour with a placeholder token
  (closed source).

## Links

- [0013](0013-model-access.md), [0044](0044-in-sandbox-credential-exception.md),
  [0097](0097-koinon-owned-gateway.md), [0112](0112-harness-definitions.md)
