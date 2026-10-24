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

- Naming of gateway targets across orgs: answered for v0.1 by the amendment below.
- Health check interval and what an unhealthy server does to its agents' tool lists: answered by the amendment.
- How remote servers that need per-user consent (OAuth) are handled, since agentgateway has no flow
  for it: refused in v0.1, planned for v0.2 (amendment).
- The container wrapper for stdio servers (v0.2).

## Amendment 2026-10-24: remote servers in v0.1 (plan item MC1)

Andre decided these points on 2026-10-24. They answer the open questions above for the v0.1 column.

1. **Remote servers only, with at most one static credential.** A server has a name (a DNS label of at
   most 24 characters), a URL (`http` or `https`; Streamable HTTP, or SSE), and optionally a header name
   and a secret value (a bearer token or any custom header), sealed with the secret store like provider
   keys. Servers are created in the UI, with `/admin/v1/mcp-servers`, or by Secrets labelled
   `koinon.dev/secret-type: mcp-server`, under the rules ADR 0056 sets for providers: a declared server is
   read-only in the UI and API, wins a name collision (the UI server of that name is disabled with a
   reason), and deleting its Secret disables it (the row stays; an admin may delete it then). Every change
   is audited.
2. **No per-user OAuth in v0.1.** agentgateway has no flow for it (see the research). A server whose answer
   to `initialize` is a 401 with a `WWW-Authenticate` that carries an RFC 9728 `resource_metadata` URL,
   which the MCP authorization specification requires of such servers, is refused when it is added, with a
   message saying so. A 401 without it is a rejected credential, which is not refused: the server may be
   declared before it runs, and its health says what is wrong. There is no explicit flag to mark a server as
   needing OAuth: the detection covers the case the spec describes. OAuth is v0.2.
3. **One URL policy for everything the gateway is pointed at.** The platform allowlist of internal hosts is
   now `egress.allowedInternalHosts` (chart value; `--egress-allowed-hosts`). `providers.allowedInternalHosts`
   (`--provider-allowed-hosts`) stays as a deprecated alias for one release; both are read and joined. The
   gateway's egress NetworkPolicy already allows the public internet and `networkPolicy.gatewayEgressTo`,
   so it covers MCP targets too. koinon-api's own calls to a server (below) use a dialer that refuses the
   addresses the policy denies.
4. **Gateway targets.** The target is named `<org slug>-<server>` (at most 63 characters, which the 24 and
   40 character limits guarantee), so an agent sees its tools as `<org slug>-<server>_<tool>` whenever
   it has more than one visible target. Two orgs can reach the same name (`a-b` with `c`, and `a` with `b-c`):
   the reconciler serves the first and reports the other, instead of letting one config error stop the
   gateway. The names `...-ask` are reserved for the approval-gated targets below. Each org target has a CEL
   `condition` (`apiKey.org == "<slug>"`), so another org's key never reaches the server, besides the agents'
   own `mcpAuthorization` allow rules. `failureMode` is `failOpen`: a server that cannot be reached is
   skipped.
5. **The credential is injected by the gateway.** It sits in a file in the gateway-only Secret that already
   holds the provider keys, named `mcp-<server id>-<8 hex of the envelope's SHA-256>`, with the same
   settle logic (kubelet propagates the file before the config names it; a changed credential keeps the old
   file meanwhile). The target's `backendAuth.credentials[]` puts it in the configured header; the gateway
   removes a client-supplied copy of that header first.
6. **Tool tiers.** Every tool of a new server starts in the **ask** tier, until an admin marks it **auto**
   or **never**. An MCP `readOnlyHint` is only a suggestion, shown in the UI. A tool a server adds later
   enters the ask tier for an agent that allows the whole server. A known tool whose description or input
   schema changes (a SHA-256 over both, canonical JSON) keeps its tier and is flagged for review on the
   Servers page; the flag does not block, and a tier decision or "mark reviewed" clears it.
7. **How the ask tier works for a server Koinon does not run.** agentgateway cannot park a call, so an
   agent's allowed ask-tier tools are offered by a second target, `<org slug>-<server>-ask`, that points at
   koinon-api's tools listener (`/mcp-ask/<server>`, reachable from the gateway only). It lists the same
   tools with the server's own schema and answers a call at once with "approval requested", recording the
   exact canonical arguments as an approval request (ADR 0042). After approval, koinon-api opens the
   credential and calls the server itself with the exact arguments, and the run resumes with the result. A
   tool moved to never since the request is not called. The auto tier is offered on the server's own target,
   directly through the gateway.
8. **Health.** The reconciler probes each served server every 60 s through the gateway with a probe key
   (initialize and `tools/list`, as an agent would), which also syncs the tool list. With `failOpen` an
   unreachable target is skipped without an error, so "no tools came back" is the failure the prober sees;
   before it records the failure that makes a server unhealthy it asks the server directly why (connection
   refused, rejected credential, OAuth) so the Servers page can say. Three failures in a row make a server
   **unhealthy**: its tools leave every agent's grants on the next pass (its target stays, so recovery is
   seen), and agentd tells the agent at the start of a session which allowed servers are down. One
   successful probe makes it healthy again.
9. **Allowlists** are stored per agent and server, optionally narrowed to tools, and edited live by admins
   (not through agent versions, whose approval covers connector grants). The reconciler renders them as
   `mcpAuthorization` rules by tier. Nothing is allowed by default.

### What agentgateway v1.6.0 showed

Verified in the gateway spec suite with the real binary:

- A target `condition` needs at least two configured targets; with one it is left out and the allow rules
  are the control.
- `backendAuth` with a bare `key` ignores its `location` and writes `Authorization: Bearer`; the
  `credentials[]` form with `location.header` writes the header named. A client-supplied header of the same
  name otherwise reaches the server; an `mcp` level `transformations.request.remove` drops it first. A
  per-target `requestHeaderModifier.remove` runs after the credential is set and removes the credential too.
- The agent's own `Authorization` header (its gateway key) is not forwarded to a target that has no
  credential.
- Another org's target is invisible in `tools/list` and `tools/call` (`unknown service`) even to an agent
  whose rules name it; the server is not contacted.
- With `failOpen` an unreachable target is skipped in `initialize`, `tools/list` and notifications.
- Tool names are prefixed `<target>_` whenever more than one target is visible to the key. The platform's
  `github` tool target is visible to every key, so an org that adds a server sees `github_` in front of the
  GitHub tools too; the harness' own prefix is in front of that.

### Consequences of the amendment

- Positive: orgs add remote servers without a platform admin, and each tool is human-approved until an admin
  trusts it.
- Negative: a new credential takes about three minutes to reach the gateway (the kubelet's Secret sync);
  the server is not served until it has.
- Negative: koinon-api calls remote servers for approved ask-tier calls and for the add-time OAuth check,
  so it needs egress to them (the chart does not restrict koinon-api's egress).
- Negative: one target-name clash between orgs leaves one server unserved until a name changes.

## Upstream verification

Checked 2026-10-23 against agentgateway v1.6.0 and the MCP Registry; see the research report. The
amendment of 2026-10-24 was checked against the agentgateway v1.6.0 binary in the gateway spec suite
(`internal/gateway/standalone/mcp_servers_test.go`).

## Links

- [MCP discovery research](../research/mcp-discovery-agentgateway.md)
- Published design: kept privately by the maintainers
- ADRs [0014](0014-mcp-and-tools.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0058](0058-gateway-modes.md)
