# 0057. MCP servers: remote and container, catalog, allowlists and discovery

- Status: Accepted
- Date: 2026-10-23
- Amends: [0014](0014-mcp-and-tools.md) (how MCP servers are provided and allowed per agent)
- Delivery: remote MCP servers and per-agent allowlists land in v0.1. Container servers, the
  catalog, discovery and platform-offered servers land in v0.2.

## Context

ADR 0014 and ADR 0040 put MCP servers behind agentgateway. They did not say how servers are added,
where the list of available servers comes from, or how one agent is limited to some servers and
tools. The research
([MCP discovery through agentgateway](../research/mcp-discovery-agentgateway.md)) shows what
agentgateway v1.6.0 offers: targets, `mcpAuthorization` rules, a management API and Kubernetes
backends, but no client API to list targets.

## Decision

1. **Two kinds of server.** A *remote* server is reached by URL. A *container* server is deployed
   by Koinon in the org's namespace. A stdio-only server runs inside a container wrapper.
2. **Catalog.** The catalog draws on the MCP Registry, curated by platform admins, plus custom
   servers.
3. **Secrets.** Org secrets are injected into the server. Agents never see them.
4. **Allowlist per agent and server**, optionally narrowed to tools.
5. **`failClosed` is false**, and servers are health-checked, so one broken server does not break
   every agent.
6. **Discovery depends on the gateway mode** ([0058](0058-gateway-modes.md)): the management API
   (standalone), the CRDs (kubernetes) or `tools/list`. A platform admin offers discovered servers
   to orgs.

## Split between v0.1 and v0.2

| v0.1 | v0.2 |
| --- | --- |
| Remote servers | Container servers (stdio wrapper included) |
| Per-agent server and tool allowlists | Catalog from the MCP Registry |
| Health checks, `failClosed` false | Discovery per gateway mode |
| | Platform-offered servers |

## Alternatives considered

- **stdio targets inside the gateway** _(reconstructed)_. The research notes that these run with the
  gateway's privileges.
- **Allowlists only in agent config** _(reconstructed)_. Not enforced in the path.
- **`failClosed`** _(reconstructed)_. One broken server would break all agents on the backend.

## Consequences

- Positive: orgs add the servers they need and give each agent only a part of them.
- Negative: Koinon renders and keeps per-agent `mcpAuthorization` rules in step with the allowlists,
  and the research notes that the whole policy is one resource in the management API, so lost
  updates need care _(inferred)_.
- Negative: no list-changed push from agentgateway, so discovery is by polling _(inferred)_.
- Follow-ups: plan item MC1; later items for the v0.2 column.

## Open questions

- Naming of gateway targets across orgs (the research suggests `<org>-<server>`).
- Health check interval and what an unhealthy server does to its agents' tool lists.
- How remote servers that need per-user consent (OAuth) are handled, since agentgateway has no flow
  for it.
- The container wrapper for stdio servers.

## Upstream verification

Checked 2026-10-23 against agentgateway v1.6.0 and the MCP Registry; see the research report.

## Links

- [MCP discovery research](../research/mcp-discovery-agentgateway.md)
- Published design: kept privately by the maintainers
- ADRs [0014](0014-mcp-and-tools.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0058](0058-gateway-modes.md)
