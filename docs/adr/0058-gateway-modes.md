# 0058. Gateway modes: standalone, kubernetes and shared

- Status: Accepted
- Date: 2026-10-23
- Amends: [0049](0049-v01-scope-decisions.md) (the two gateway modes become three)
- Delivery: standalone and kubernetes (plan item G2) in v0.1; shared in v0.2.

## Context

ADR 0049 chose two agentgateway modes behind one `ModelGateway` interface. Some installs already
run a platform gateway that Koinon does not own, with its own management API. The research
([MCP discovery](../research/mcp-discovery-agentgateway.md)) describes what Koinon can read and
write there.

## Decision

1. **Three modes** behind the `ModelGateway` interface:
   - *standalone*: Koinon installs and fully configures its own agentgateway (as in 0049).
   - *kubernetes*: Koinon creates its own `Gateway` and policies next to an existing controller
     (as in 0049, plan item G2).
   - *shared*: Koinon uses an existing gateway through its management API. v0.2.
2. **No separate "vended keys" mode.** When the platform issues model keys, the platform gateway is
   treated as a provider upstream ([0056](0056-model-providers-pricing-and-budgets-per-org.md)) in
   standalone mode.
3. **In shared mode Koinon changes only the models it registered** and never edits other
   configuration on the gateway.

## Alternatives considered

- **A fourth mode for platform-vended keys** _(reconstructed)_. The provider upstream already
  covers it.
- **Koinon edits any gateway config in shared mode** _(reconstructed)_. Could break other tenants.

## Consequences

- Positive: Koinon fits into an existing platform gateway without owning it.
- Negative: a third driver to build and test against the same suite as the others _(inferred)_.
- Negative: the management API has no auth or tenancy in v1.6.0, so access is arranged with the
  platform _(inferred)_.
- Follow-ups: the shared driver (v0.2), REL2 run B.

## Open questions

- How Koinon finds and authenticates to the management API in shared mode.
- How Koinon marks the models it registered, so it can tell them from others.
- Budget enforcement and spend export in shared mode.

## Upstream verification

See the MCP discovery research report (agentgateway v1.6.0, 2026-10-23).

## Links

- ADRs [0040](0040-agentgateway-model-and-mcp-gateway.md), [0049](0049-v01-scope-decisions.md),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0057](0057-mcp-servers.md)
