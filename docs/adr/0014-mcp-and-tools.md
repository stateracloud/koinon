# 0014. MCP and tools: shared servers outside sandboxes, one progressive endpoint per agent

- Status: Accepted; amended by [0040](0040-agentgateway-model-and-mcp-gateway.md) and [0057](0057-mcp-servers.md)
- Date: 2026-10-21
- Area: connectors-tools
- Tags: mcp, agentgateway, credentials
- Decision: [design.md#14](../design.md)

## Context

Agents use tools through MCP. Two problems shape the design:
- Loading every tool definition into context wastes tokens and degrades tool choice.
- Many MCP servers need credentials (clouds, SaaS), which must stay out of the sandbox
  ([0006](0006-identity-layers.md)).

## Decision

- **Shared MCP servers** are deployed from the UI and run **outside** sandboxes. Each agent reaches
  them through **one progressive endpoint** (search plus call) that is filtered by the agent's
  grants.
- **Local MCP servers** are declared per agent and run **inside** the sandbox, for tools that need
  no credentials.
- ToolHive is to be evaluated.

**Amendment ([0040](0040-agentgateway-model-and-mcp-gateway.md)):**
- agentgateway is the MCP gateway. It provides the per-agent endpoint, CEL authorization on tool
  name, arguments and JWT claims, tool-list filtering, and ExtMCP callouts. It also hosts the ask
  tier ([0042](0042-ask-tier-park-and-execute.md)).
- The ToolHive evaluation is narrowed to *deploying and running* MCP servers (its `MCPServer` and
  `MCPRemoteProxy` CRDs), not to the gateway role.

## Alternatives considered

- **All MCP servers inside the sandbox** _(reconstructed)_. Credentials would sit in the sandbox.
- **One MCP endpoint per server, all exposed to the agent** _(reconstructed)_. Context bloat, and
  authorization spread over many endpoints.
- **ToolHive vMCP as the gateway**. Recorded during grilling: ToolHive's vMCP optimizer offers
  `find_tool` + `call_tool` and Cedar authorization. agentgateway was preferred because it also
  serves as the model gateway, so there is one component for both.
- **Docker MCP Gateway dynamic tools** _(reconstructed)_. Experimental and Docker-centric.

## Consequences

- Positive: low context overhead, and credentials stay outside the sandbox.
- Positive: one gateway for models and tools.
- Negative: the progressive endpoint (search plus call) has to be built or configured. agentgateway
  documents authorization and filtering, but "search + call" meta-tools must be confirmed or built
  as a Koinon MCP server behind it _(inferred)_.
- Negative: pi-acp does not forward client-provided MCP servers to pi. For the pi flavor, MCP
  configuration must go through pi's own config, not through ACP
  ([0035](0035-v01-flavors.md)).
  > Note (2026-10-26): v0.1 ships OpenCode only ([0059](0059-v01-opencode-only.md)).
- Follow-ups: shared MCP servers ship in v0.5 ([0024](0024-roadmap.md)). v0.1 only needs the GitHub
  MCP tool for the demo approval ([0042](0042-ask-tier-park-and-execute.md)).

## Open questions

- Does agentgateway provide a progressive search + call mode, or does Koinon add a small
  "tool search" MCP server in front?
- Whether to run shared MCP servers through ToolHive or as plain Deployments rendered by the
  reconciler (evaluation issue #6).

## Upstream verification

- 2026-10-21: agentgateway CEL MCP authorization (`mcp.tool.name`, `mcp.tool.arguments`), hidden
  tools filtered out of the list, ExtMCP gRPC callouts.
  https://agentgateway.dev/docs/standalone/main/configuration/security/mcp-authz/ ,
  https://agentgateway.dev/docs/standalone/main/mcp/guardrails/about/
- 2026-10-21: ToolHive v0.55.2 (Apache-2.0); CRDs MCPServer, MCPRemoteProxy, VirtualMCPServer,
  MCPToolConfig, MCPAuthzConfig; vMCP optimizer with `find_tool`/`call_tool`.
  https://docs.stacklok.com/toolhive/guides-vmcp/optimizer
- 2026-10-21: progressive tool discovery is not part of the MCP spec. The client best-practices page
  describes a host-side `search_tools`.
  https://modelcontextprotocol.io/docs/develop/clients/client-best-practices

## Links

- [0006](0006-identity-layers.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0042](0042-ask-tier-park-and-execute.md)
