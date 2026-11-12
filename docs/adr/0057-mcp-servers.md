# 0057. MCP servers: remote and container, catalog, allowlists and discovery

- Status: Accepted; amended 2026-10-24, 2026-10-30, 2026-10-31 and 2026-11-01 by [0084](0084-leak-guard.md) (below)
- Date: 2026-10-23
- Area: connectors-tools
- Tags: mcp, connectors, approvals, egress, credentials
- Amended by: [0096](0096-mcp-oauth-sign-in.md) (amendment 2026-10-24, items 2 and 5: the OAuth refusal and the injection path), [0097](0097-koinon-owned-gateway.md) (items 4, 5 and 7 in step 2)
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

procoduck decided these points on 2026-10-24. They answer the open questions above for the v0.1 column.

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
   so it covers MCP targets too. Every call to a server goes through the gateway, so its egress policy
   applies to all of them. The one direct request is the anonymous OAuth check when a server is added (item
   2), which dials only addresses the policy allows and carries no credential.
4. **Gateway targets and tool names.** The target is named `<org slug>-<server>` (at most 63 characters,
   which the 24 and 40 character limits guarantee). The gateway runs with `prefixMode: always`, so a tool is
   always `<target>_<tool>` and never changes when an org adds a server: `github_create_pull_request`,
   `acme-tracker_search`, and the readiness prompt `github_koinon-ready`. The `mcpAuthorization` rules match
   the unprefixed target and tool names (verified with the real binary), so grants and the GitHub ask-tier
   tools are unchanged; the harness' own prefix still goes in front (`koinon_github_create_pull_request`).
   Agents see at most `mcp__koinon__<org>-<server>-ask_<tool>`, and model APIs accept 64 characters of
   `[A-Za-z0-9_-]`, so a tool whose name would exceed that, or has other characters, is not offered and the
   Servers page says "unavailable". Two orgs can reach the same name (`a-b` with `c`, and `a` with `b-c`):
   the reconciler serves the first and reports the other, instead of letting one config error stop the
   gateway. The approval-gated targets (`<target>-ask`, below) take part in that check, and the server
   names `ask` and `*-ask` are refused. Each org target has a CEL
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
   schema changes (a SHA-256 over both, canonical JSON) is flagged for review on the Servers page. An
   **auto** tool that changes drops to **ask** until an admin reviews it and sets auto again ("mark
   reviewed" clears the flag but does not restore auto); **ask** and **never** tools are only flagged.
   A tool can require **two approvers** (off by default, `requires_two_approvers`): two distinct admins or
   owners must approve the exact call, and neither may be the person who created the ticket (they asked for
   the work; the second pair of eyes is the point). The first approval leaves the request pending. Either
   approver can deny. An org with one admin simply leaves the flag off.
7. **How the ask tier works for a server Koinon does not run.** agentgateway cannot park a call, so an
   agent's allowed ask-tier tools are offered by a second target, `<org slug>-<server>-ask`, that points at
   koinon-api's tools listener (`/mcp-ask/<server>`, reachable from the gateway only). It lists the same
   tools with the server's own schema and answers a call at once with "approval requested", recording the
   exact canonical arguments as an approval request (ADR 0042), bound to the server's ID, a hash of its
   URL and the tool's content hash. After approval, koinon-api makes the call **through the gateway** with a
   per-org executor key (the reconciler issues it and keeps it in a Secret koinon-api reads; it opens only
   that org's targets), so the gateway's egress policy and credential injection apply and koinon-api never
   holds or sends a server credential. The arguments are passed as the approved bytes, not decoded and
   re-encoded (big integers and `1.0` survive). The executor refuses the call (permanently) if the server,
   its URL or the tool's content changed, the tool is in the never tier, or the agent's allowance is gone.
   Only a failure before the request is sent is retried; once it is sent, a lost answer fails the request
   with "outcome unknown" and it is never run again. The auto tier is offered on the server's own target,
   directly through the gateway.
8. **Health.** The reconciler probes each served server every 60 s through the gateway with a probe key,
   and also right after the pass that first serves a server or serves it with another URL or credential (old issue 226)
   (initialize and `tools/list`, as an agent would), which also syncs the tool list. With `failOpen` an
   unreachable target is skipped without an error, so "no tools came back" is the failure the prober sees,
   and the Servers page shows that: the credential never leaves the gateway, so the prober cannot ask the
   server why. Three failures in a row make a server **unhealthy**: its tools leave every agent's grants on
   the next pass (its target stays, so recovery is seen), and agentd tells the agent at the start of a
   session which allowed servers are down. One successful probe makes it healthy again.
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
- With `prefixMode: always`, tools and prompts are named `<target>_<name>` for every key, and the
  `mcpAuthorization` rules still match the unprefixed `mcp.tool.target` and `mcp.tool.name`.
- A lone org target with a condition is rejected; Koinon renders a placeholder second target whose condition
  is false, so the per-org condition always applies to `initialize` and lists too.
- A static `credentials[]` entry is the only way to write a credential header; with no target using
  `Authorization`, the agent's gateway key is still not forwarded.

### Consequences of the amendment

- Positive: orgs add remote servers without a platform admin, and each tool is human-approved until an admin
  trusts it.
- Negative: a new credential takes about three minutes to reach the gateway (the kubelet's Secret sync);
  the server is not served until it has.
- Negative: koinon-api makes one anonymous request to a new server to detect OAuth, so it needs egress to
  it at that moment (the chart does not restrict koinon-api's egress); nothing else it does reaches a server.
- Negative: a rotated executor key and a restarted reconciler cost nothing, but a Secret deleted by hand
  stops approved calls (they retry) until the next reconcile pass writes a new key.
- Negative: one target-name clash between orgs leaves one server unserved until a name changes.

## Amendment 2026-10-30: third-party descriptions are sanitised at sync (#52)

A server's tool descriptions, and the description strings of its input schemas, reach the model verbatim,
so they are untrusted input. `internal/mcpsanitize` normalises them when `SyncTools` records a tool:

- ANSI escapes, control characters (newline and tab stay), zero-width and other invisible format
  characters, bidi overrides and isolates, variation selectors and Unicode tag characters (U+E0000 to
  U+E007F) are removed;
- tags that imitate a harness or chat-role block (`<system>`, `<system-reminder>`, `<koinon-*>`,
  `<untrusted_context`, `<tool_result>`, `<|im_start|>` and similar, opening or closing, also with a
  fullwidth bracket or a Unicode space) have their `<` escaped to `&lt;`, so an admin still reads them.
  This runs after the removals, so a zero-width character inside a name does not hide it. It is a list
  of names: a homoglyph name (Cyrillic `ѕ`) or an ordinary `<b>` passes;
- a tool description is cut to 1,024 characters and each `description` or `title` in the input schema to
  256 (research TP22), ending in an ellipsis. This replaces the old 20,000 byte cut. Instance values in
  the schema (`default`, `const`, `enum`, `examples`), names and numbers are left as published: they
  are what the tool receives or compares against.

What is stored, hashed (`content_hash`) and offered by the ask-tier proxy is the sanitised text, so the
review flow is unchanged: a visible change flags the tool, and a change that only differs in removed
characters does not. Both tool APIs (the admin API and `mcpserver.v1alpha1.Tool`) carry
the original length and the capped and cleaned flags, and the servers page says when a tool's text was
sanitised. Tools synced before this change get the values on their next sync.

Upgrade: the first sync after this change compares the sanitised text with the stored raw text. A tool
whose description was over 1,024 characters, whose schema had a description over 256, or whose text had
anything removed or escaped (CRLF counts) hashes differently, so it is flagged for review and, if it was
in the auto tier, moves to ask. That fails closed: calls to such a tool need approval until an admin
reviews the sanitised text and sets the tier back. Tools with short, clean text are not affected.

Limit: an **auto** tool is served by agentgateway, which federates the upstream server's `tools/list`
unchanged. Koinon renders `mcpAuthorization` rules (allow or deny by tool name) and has no hook that
rewrites a description, so the model sees the raw text of an auto tool. The sustainable fix is in the
harness side of the path (research TP22, TP25): the harness (agentd's MCP bridge, or koinon-harness) applies
the same `mcpsanitize` policy to what it hands the model, and pins each tool to the approved hash. Until
then an admin sees the sanitised text and the flags at approval, and the ask tier is the safe choice for a
server that is not trusted.

## Amendment 2026-10-31: the leak guard on every org route (old issue 347)

[0084](0084-leak-guard.md) puts a leak guard in front of every MCP tool call:

- **Every org's MCP route carries `mcpGuardrails`** (ExtMcp, remote, `failClosed`) pointing at
  koinon-api's guard. This is a separate setting from the targets' `failureMode: failOpen` (decision 5
  and item 4 of the 2026-10-24 amendment): an unreachable *server* is still skipped, but when the
  *guard* is unreachable every MCP call on the route is refused. The guard fingerprints MCP results
  into the home generation's taint store and blocks a call whose arguments carry private data; a
  match becomes an ADR 0042 approval.
- **The auto tier is no longer unchecked.** An auto tool still runs without a person, but its
  arguments pass the taint check and, for fetch URLs and search queries, an automatic
  shape-and-budget URL rule. Hosts exempt from the rule are on a per-org fetch-trusted host list,
  empty by default.
- **Servers declare a data class and kind.** Each server record says which of untrusted input,
  private data and outbound tool it carries, and its kind (for example fetch or search), so the
  guard and the templates test read a declaration rather than a name (old issue 350). The identifiers and
  the class existing servers migrate to are open in 0084.

## Amendment 2026-11-01: a live check with the agent's own key (old issue 488)

The health record of decision 8 is up to one probe interval (60 s) old, and agentgateway runs `failOpen`,
so a server that drops between two probes is left out of the agent's tool list without a word (the rc.9
incident). Before the harness starts, agentd (old issue 326, `mcpwait.go`) therefore also asks the gateway what it
lists to the agent right now.

- **The credential is the agent's own gateway key.** `tools/list` under that key shows exactly what the
  harness will see, so it proves the agent's actual grants (an unreachable server, a rejected server
  credential and a missing grant all look the same: none of the server's auto-tier tools). Reachability
  is judged by the auto target only: the ask target (`<org>-<server>-ask`) is koinon-api's `/mcp-ask`
  proxy, which lists the tools from Postgres and never contacts the remote server, so a listed ask tool
  proves the grant, not that the server is up. A server with auto-tier tools must have one listed; a
  server with ask-tier tools only is checked for the grant and is not proven reachable (the health
  record still applies). It needs no new
  credential. Rejected: a Koinon check key with every MCP grant (a cross-org privileged credential that
  would not prove the agent's own grants), and a harness report over ACP (it needs a Koinon ACP
  extension, and runs only after the harness is up).
- **agentd never holds the key.** The key lives in the agent's key Secret, which koinon-api can already
  read by name (it reads the executor keys the same way). agentd asks koinon-api over the saved-work
  listener (`POST /internal/mcp-tools`, package `mcpcheck`), signed with the HMAC key it already holds,
  over a domain-separated message good for one agent for 30 s, as for the home repository password
  (ADR 0054 amendment). koinon-api lists the tools through the gateway as the agent and answers with the
  tool names only. The key is not in the request, the reply, an error, a log line, a trace or a run
  event, and it never reaches a sandbox, an environment variable, an image or a home volume.
  The check runs once the sandbox is ready, when the key exists.
- **What counts as a miss.** For each allowed, served server that is not unhealthy (the health record
  already covers those) and has present tools in the auto or ask tier within the agent's allowlist, the
  gateway must list at least one of them under `<org>-<server>_<tool>` (auto) or
  `<org>-<server>-ask_<tool>` (ask). A server with none to offer is not expected to list anything.
- **Miss handling is old issue 326's, not a second path.** The live misses join the health-record misses in the
  one `waitMCPServers`: the `run.mcp_server_missing` event, the `koinon.mcp.missing_servers` trace
  attribute, one retry after `MCPRetryDelay`, a failed run on the second miss. The reason says the
  gateway does not list the server's tools and, as the gateway does not say why, names the causes.
- **A check that cannot be made is not a miss.** If koinon-api or the gateway cannot be reached, or the
  agent has no key yet, agentd logs it and relies on the health record: the harness's own connection
  would fail the same way, and a failed check must not fail a run whose servers may be fine.
- Cost: one `tools/list` per run, plus one per retry. The check runs on every run, not only when the
  health record is old: the call is cheap and the record's age is not a safe test of a drop.
- Negative: koinon-api now sends the agent's key to the gateway on agentd's request. Only agentd's
  signed request triggers it, the listener is reachable by koinon-agentd only, and what comes back is
  names.

## Amendment 2026-11-12: every schema string is sanitised, and the published text is hashed (#684)

This changes two points of the 2026-10-30 amendment.

1. **Every string of an input schema is sanitised**, not only `description` and `title`. The model reads the
   whole schema, so an instance value (`enum`, `const`, `default`, `examples`), a `pattern`, an unknown
   keyword's value and a property name are as much untrusted text as a description. Each gets the removals
   and the tag escape of the 2026-10-30 amendment. Only prose is cut: `description` and `title` keep their
   256-character cap, while other strings keep their length, because a value cut short is a different value.
   Property names (the keys of `properties`, `patternProperties`, `$defs`, `definitions` and
   `dependentSchemas`, and the strings of `required`) are cleaned the same way. Two names that clean to the
   same name keep the one that was already clean. Numbers and booleans are left as published.

   The cost: a tool whose server compares against a value that had an invisible character in it receives
   the cleaned value. No legitimate value needs a zero-width, bidi-control or tag character to be told
   apart from its neighbours, so this is accepted.

2. **The content hash covers the published text as well as the stored one.** Before this change, a server
   could change only what sanitising removes or cuts (an invisible character, the text past a cap) and the
   tool kept its hash, so it was not flagged. An auto tool is served raw by agentgateway (the limit above),
   so that change reached the model unreviewed. `mcpservers.ToolContentHash` now works as follows:
   - A tool whose published text is the stored text keeps `ContentHash(description, schema)`. A clean tool
     therefore does not change hash on upgrade.
   - Any other tool hashes the stored text together with the published description and schema (canonical
     JSON).
   Any change of the published bytes flags the tool, and an auto tool moves to ask as for any other change.

   Upgrade: on the first sync after this change, a tool whose published text was sanitised gets the new
   hash, and is flagged for review once. This includes a tool whose schema had a string that is now
   cleaned. It fails closed, as the 2026-10-30 upgrade did.

The leak guard (ADR 0091) is unaffected, except in one respect: a tool result's string that is itself a
JSON document is now decoded before its text is fingerprinted, up to three levels deep (#684).

## Upstream verification

Checked 2026-10-23 against agentgateway v1.6.0 and the MCP Registry; see the research report. The
amendment of 2026-10-24 was checked against the agentgateway v1.6.0 binary in the gateway spec suite
(`internal/gateway/standalone/mcp_servers_test.go`).

## Links

- [MCP discovery research](../research/mcp-discovery-agentgateway.md)
- Published design: kept privately by the maintainers
- ADRs [0014](0014-mcp-and-tools.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0058](0058-gateway-modes.md)
