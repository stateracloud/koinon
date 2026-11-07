# MCP server registration and discovery through agentgateway: research for Koinon

Date: 2026-10-23. Sources: agentgateway source at tag **v1.6.0** (commit `ea56086`), agentgateway.dev docs ("latest"), the MCP specification at modelcontextprotocol.io (current revision **2026-07-28**), and the MCP Registry repo at commit `fa509e0`.

Each claim is tagged **[E]** (established: read directly in source or official docs) or **[I]** (inference: my reading of what follows from those sources). Short paths such as `types/agent.rs` are relative to `crates/agentgateway/src/` in the agentgateway repo unless they say otherwise. The full URLs are in the Sources section.

---

## 1. How agentgateway models MCP

### 1.1 Targets and backends

- An **MCP backend** is a list of **targets** plus settings for the whole backend: `stateful`, `prefixMode`, `failureMode`, `sessionIdleTtl`, `sseKeepAlive`, `dnsRebindingProtection` and `server` overrides. [E] `types/agent.rs` `McpBackend` (around L1860).
- Each target has a `name`, an optional CEL `condition`, and one spec out of `sse`, `mcp` (Streamable HTTP), `stdio` (`cmd`, `args`, `env`, `clearEnv`) or `openapi` (a backend plus an OpenAPI `schema` that is turned into tools). [E] `types/agent.rs` `McpTarget`, `McpTargetSpec` (around L1939–2000). In the local config file these are `LocalMcpTarget` and `LocalMcpTargetSpec`. [E] `types/local.rs` around L1921–2104.
- A target address is either inline (`host: https://x/mcp`, or `host`, `port` and `path`) or a reference to a named `backend`. [E] `types/local.rs` `McpBackendHost`.
- Target names must match the Gateway API SectionName pattern: lowercase letters, digits, `-` and `.`. The characters `_` and `+` are reserved as MCP delimiters. [E] `types/agent.rs` `validate_mcp_target_name`.
- There are two ways to configure MCP in the standalone file. [E] docs, *configuration-modes*; `types/local.rs` L414–418.
  - **Simplified:** the top-level `mcp:` section. It defines one virtual MCP server, served at `/mcp` and `/sse`, and "all MCP servers listed will be served as a single virtual MCP server".
  - **Routing-based:** `routes[].backends[].mcp`.
- Each target can carry its own `policies`, but only transport policies: `requestHeaderModifier`, `transformations`, `backendTLS`, `backendAuth`, `http`, `tcp` and `backendTunnel`. `mcpAuthorization` and `mcpGuardrails` "apply to the full target set and belong on the route or `mcp.policies`". [E] `types/local.rs` `LocalMcpTarget` doc comment and `SimpleLocalBackendPolicies` (L2665).

### 1.2 Multiplexing: tool names and how `tools/list` is merged

- With N>1 targets, agentgateway sends `tools/list` to every target, merges the answers into one list, and gives the merged result `ttlMs: 0` and `cacheScope: private`. [E] `mcp/handler.rs` `merge_tools` (L772–820). The docs call this "multiplexing", also "federation". [E] docs, *configuration-modes*.
- Prefixing uses `DELIMITER = "_"`, so an exposed tool name is `<target>_<tool>`. [E] `mcp/handler.rs` L37 and `resource_name`. Resource URIs get the prefix `<target>+<uri>`. [E] `parse_resource_uri`.
- `prefixMode` has three values. [E] `types/agent.rs` `McpPrefixMode`; `mcp/upstream/mod.rs` L494; `mcp/handler.rs` `needs_resolution` and `resolve_unprefixed`.
  - `conditional` (default): prefix only when there are several targets. A single target is used as the "default target" and gets no prefix.
  - `always`: prefix even with one target.
  - `never`: no prefix. A call is routed by listing every target at call time to find the one that owns the name. Names served by more than one target are dropped. The source has a TODO to cache these list results.
- `initialize` / `server/discover` with several targets do three things. [E] `merge_initialize`, `merge_discover` and `get_info` in `mcp/handler.rs`.
  - They pick the lowest protocol version (for `initialize`), or the intersection of supported versions (for discover).
  - They merge each upstream's `instructions` under a `[target-name]` header.
  - They report `serverInfo` `agentgateway`, unless overridden.
- `failureMode` defaults to `failClosed`: if any target fails to initialize or fails during fanout, the whole session fails. `failOpen` skips failed targets. [E] `mcp/mod.rs` `FailureMode`.
- **Target conditions:** a target-level CEL `condition` is evaluated once per request against the authenticated request context, with `mcp.target.name` available. A target that evaluates to false "is excluded before it is initialized or contacted". A condition requires at least 2 configured targets. If every target is conditioned out, the result is an empty virtual MCP. [E] `mcp/README.md` "Target conditions"; `mcp/upstream/mod.rs` `new_for_request` (L494–510); `types/local.rs` L1731.

### 1.3 Authentication to upstream MCP servers, per target

The `backendAuth` kinds are: [E] `http/auth/mod.rs` `BackendAuthKind` (L73–110).

- `passthrough`: forward the validated incoming JWT.
- `key`: a static secret value, with a configurable location (header, query parameter or cookie).
- `gcp`, `aws`, `azure` and `copilot`.
- `jwtSign`: sign a short-lived JWT with a private key on each request.
- `oauthTokenExchange`: RFC 8693 token exchange, or RFC 7523 jwt-bearer. [E] `http/auth/oauth/mod.rs` `OAuthGrantType`.
- `crossAppAccess`: ID-JAG.
- In addition, `BackendAuth.credentials[]` can insert several credentials at set locations. [E]

Static headers can be set with `requestHeaderModifier` in the target's `policies`. [E] `SimpleLocalBackendPolicies`.

Limits:
- I found no backend-auth kind that runs an interactive, per-user OAuth authorization-code flow against a third-party MCP server (for example a remote SaaS MCP server that asks each user to consent). The OAuth kinds exchange a subject token the gateway already has. [I]
- `passthrough` and the token-exchange kinds forward or exchange an incoming JWT. With Koinon's per-agent API keys there is no JWT to forward, so in practice the useful options are a static `key` or header per target, or `jwtSign`. [I]

### 1.4 How `mcpAuthorization` filters `tools/list` and `tools/call`

- A policy is a `RuleSet` of CEL rules. A plain string means `allow`; the object forms are `allow`, `deny` and `require`. [E] `http/authorization.rs` `RuleSet` and `PolicySet` (L99–140).
- Rules are evaluated in this order: [E] `RuleSets::validate` (L254–274).
  1. Any `deny` that matches → deny.
  2. Any `require` that fails → deny.
  3. Any `allow` that matches → allow.
  4. Otherwise: deny if any allow rules exist (allowlist behaviour), allow if only deny rules exist.
- Rule sets from route level and backend level are merged so that both apply. [E] `mcp/rbac.rs` `McpAuthorizationSet::merge`.
- **On `tools/list`:** each upstream tool is checked as `ResourceType::Tool(target, name)` with method `tools/list`. A tool that fails is removed before the prefix is added. Prompts, resources and resource templates are filtered the same way. [E] `mcp/handler.rs` `merge_tools`, `merge_prompts`, `merge_resources` and `merge_resource_templates`. The docs agree: "the gateway automatically filters it from the list response". [E] docs, *mcp-authz reference*.
- **On `tools/call`:** the prefixed name is split back into target and tool. `authorize_with_ctx` is then called with `ResourceType::Tool(service_name, tool)` and method `tools/call`, and a request that fails is rejected. [E] `mcp/session.rs` around L600–635; CRD doc comment `BackendMCP.Authorization` ("Get or call operations … reject requests that do not meet the rule").
- CEL variables include `mcp.tool.name`, `mcp.tool.target`, `mcp.prompt.name`, `mcp.resource.name` and `mcp.methodName`, plus the auth context (`jwt.*`, and Koinon's `apiKey.*`). `mcp.tool.arguments` is not set during `mcpAuthorization`. [E] docs, *mcp-authz reference*; `mcp/rbac.rs`.

---

## 2. Discovery on a gateway Koinon does not own

### 2.1 What a client holding an API key can see

- **`tools/list`, `prompts/list` and `resources/list` through the gateway** return exactly what that caller is allowed to use. Target conditions decide which upstreams are contacted at all, and `mcpAuthorization` then filters each item. [E] (§1.2, §1.4)
- So an agent key, or a Koinon "probe" key that has the same rules as the agent, can find out what is available to that identity. [I]
- The result is tools, not targets. A target's name shows only as the `<target>_` prefix (under `conditional` or `always`), and as `[target]` headers in the merged `instructions` for targets that send instructions. [E] `get_info`.
- With `prefixMode: never`, or with a single target under `conditional`, nothing shows which target a tool came from. [E]
- **There is no MCP-level or HTTP endpoint for clients that lists targets.** I found no "list targets" method in the MCP handler. Discovery responses are marked "immediately stale" (`ttlMs: 0`, private) because "the gateway has no way to invalidate clients when backend membership or routing config changes". [E] `mcp/handler.rs` `get_discovery` comment.
- I found no code that sends `notifications/tools/list_changed` to clients when the gateway config reloads. Clients should re-list rather than wait for a push. [I] (absence in `mcp/upstream/mod.rs` and `mcp/session.rs`)

### 2.2 The management resource API (standalone)

- **Routes**, on the admin/UI router (served on the admin address `127.0.0.1:15000` by default): [E] `ui.rs` L90–105; docs *config-resources*.
  - `GET /api/config/resources`
  - `GET` and `PUT /api/config/resources/{kind}` (the PUT body is `{resources:[…]}`)
  - `PUT` and `DELETE /api/config/resources/{kind}/{id}`
  - `GET /api/config` (the raw config file)
  - `GET /api/config/effective` (the file merged with the DB resources)
- **MCP kinds, with their exact names and granularity:** [E] `config_store.rs` `ConfigResourceKind` (L51–80), `resource_id` (L1358–1395), `MCP_SETTINGS_FIELDS` (L332), materialization (L1030–1050).
  - `mcp.target`: **one target**. The id is `value.name` and the value is a `LocalMcpTarget`, so it can be `mcp`, `sse`, `openapi` **or `stdio`**. A unit test stores `{"name":"db-target","stdio":{"cmd":"server"}}` (`config_store.rs` L2181–2184). These targets are appended to the top-level `mcp.targets` list.
  - `mcp.policy`: **one whole policy type**. The id is the policy key under `mcp.policies`, for example `mcpAuthorization`, `cors` or `apiKey` (the test at L2186 uses id `cors`). The resource holds **every rule** in that policy, so per-agent rules cannot be written as separate resources.
  - `mcp.settings`: a single `default` resource with `gateways`, `port`, `statefulMode`, `prefixMode` and `failureMode`.
  - Only the **top-level simplified `mcp` section** is managed this way. MCP backends inside routes are part of a whole `traffic.route` resource. [E] for the kinds; [I] that route-embedded MCP can only be changed by replacing the whole route.
- **Storage modes:** [E] `ui.rs` `config_resource_store` and `list_stored_config_resources`; `lib.rs` `ConfigStoreMode`; docs *config-resources*.
  - The full API needs `config.storage.mode: hybrid` and a database (SQLite or Postgres).
  - In `file` mode, writes go into the config file and a list request returns an **empty array**.
  - In `readOnly` mode, writes return 403.
- **Lists show only DB-backed resources.** Targets and policies that the file owns do not appear in `GET /api/config/resources/mcp.target`. To see everything, read `GET /api/config/effective` (or `GET /api/config` in file mode). [E] `list_stored_config_resources` uses `store.list` only; `get_effective_config` L422–436.
- **Conflicts with the file:** a DB resource whose id collides with a file-owned resource is rejected ("conflicts with file-owned resource"). An item update of a file-owned resource in hybrid mode returns a conflict. [E] `config_store.rs` `append_policy_kind` and `append_list_kind`; `ui.rs` `update_config_resource`.
  - So if the platform keeps `mcp.policies.mcpAuthorization` in the file, Koinon **cannot** add its own `mcp.policy/mcpAuthorization` through the API. [I]
- **Validation:** every write is materialized and fully validated before it is committed. An invalid write returns 422. [E] `validate_materialized_config`.
- **Authorization:** "The admin address … has no authentication. Anyone who can reach it can manage keys." [E] docs *config-resources*. The only per-kind logic on a write stamps an owner on `llm.apiKey`. There is no per-kind or per-tenant write restriction. [E] `ui.rs` `AuthorizationContext::authorize_write`.
  - The UI and API routes can be attached to a gateway with `ui.policies` (OIDC, authorization), which a platform could use to scope a Koinon credential, for example to GET-only. [E] `types/local.rs` `LocalUIConfig`; [I] that it can scope it this way.
- **Admin `/config_dump`** (on the admin port) dumps the runtime state the proxy has received. Secrets are redacted (`ser_redact`). It also works when config comes from xDS (Kubernetes). [E] route list in `management/admin.rs` L204–221; [I] that it includes MCP backends and their targets (`McpBackend` is serializable).

### 2.3 Kubernetes mode

- **CRDs** live in group `agentgateway.dev/v1alpha1`: `AgentgatewayBackend` (short name `agbe`), `AgentgatewayPolicy` (`agpol`), `AgentgatewayModel` and `AgentgatewayParameters`. [E] `controller/api/v1alpha1/agentgateway/doc.go` and the kubebuilder markers.
- **`AgentgatewayBackend.spec.mcp`** has `targets[]` (1–128 entries, list key `name`), `sessionRouting` (Stateful/Stateless), `failureMode` and `prefixMode`. Each target is exactly one of: [E] `agentgateway_backend_types.go` L613–760.
  - `static`: `host` or `backendRef` (a Service in the same namespace), plus `port`, `path` and `protocol` (`StreamableHTTP` or `SSE`), and optional `policies`. There is **no stdio or OpenAPI option** in the CRD target type.
  - `selector`: `namespaces` and/or `services` label selectors.
- **Service discovery through a selector:** [E] `controller/pkg/syncer/backend/mcp.go`; `controller/api/annotations/agentgateway.go`.
  - Every Service that matches and has a port with `appProtocol` set to `agentgateway.dev/mcp` (Streamable HTTP) or `agentgateway.dev/mcp-sse` (or the legacy `kgateway.dev/mcp*` values) becomes **its own target**.
  - The target name comes from the annotation `agentgateway.dev/mcp-target-name`. If that is missing it is `<svc>-<portName>`, or `<svc>-<port>` when the port has no name.
  - The path comes from the annotation `agentgateway.dev/mcp-path`.
  - The `selector` entry's own `name` is therefore not the name that `mcp.tool.target` sees.
  - The docs say "Label selectors support only streamable HTTP", but the v1.6.0 code also accepts `mcp-sse`. [E] docs *dynamic-mcp*; the two disagree.
- **Authorization in Kubernetes:** `AgentgatewayPolicy` with `backend.mcp.authorization` (same semantics: list items are filtered, calls are rejected), attached with `targetRefs[].sectionName` to pick one MCP target by name, or with `targetSelectors` (label match). [E] `agentgateway_policy_types.go` `BackendMCP` (L2419) and `AgentgatewayPolicySpec`; `shared_types.go`.
- **Can Koinon read them?** Yes, if it has a Kubernetes ServiceAccount with get/list/watch on `agentgatewaybackends` and `agentgatewaypolicies` (`agentgateway.dev`), plus Services across the selected namespaces. Selector-discovered target names can only be worked out by repeating the controller's Service selection, or from `/config_dump`. [I]

---

## 3. Registering a new MCP server at runtime

| Path | Restart needed? | Notes |
|---|---|---|
| Edit the config file (standalone) | **No** [E] | The state manager watches the file and reloads after a change (`state_manager.rs` `watch_config_file`, `reload_config_after_change`). A reload that fails is logged and the previous state is kept [E] (L306–316). |
| Resource API, `hybrid` mode | **No** [E] | A DB change raises `subscribe_changes` and the config reloads (`state_manager.rs` L162–203; Postgres uses `PgListener`, `config_store.rs` L7). The docs say writes apply asynchronously and are "not guaranteed to be visible" when the API returns [E]. |
| Resource API, `file` mode | **No** [I] | The write lands in the file, and the file watcher reloads it. |
| Kubernetes CRDs / labelled Services | **No** [I] | The controller translates with krt collections and pushes xDS. Docs: you can "update the backing MCP server without having to update the Backend resource" [E]. |

**Effect on live sessions.** In stateful mode the target set is captured when the session starts (`McpBackendGroup` in `new_for_request`). A target added later probably appears only in new sessions or after a re-initialize. [I]

**`failClosed` risk.** Under the default `failClosed`, one new target that is unhealthy fails every session on that virtual server. [E] for the default; [I] for the impact.

---

## 4. The MCP specification side: discovery for a catalog

- **Inside the protocol** (2026-07-28):
  - `server/discover` is a **mandatory** RPC. It returns `supportedVersions`, `capabilities`, `_meta["io.modelcontextprotocol/serverInfo"]` and `instructions`, and it can be cached (`ttlMs`, `cacheScope`). [E] spec *server/discover*.
  - The revision dropped the `initialize` handshake and sessions, and made `tools/list`, `resources/list` and `prompts/list` independent of the connection. [E] spec *changelog*.
  - `serverInfo` "is self-reported … SHOULD NOT rely on it for security decisions". [E]
  - Tools come with `title`, `annotations` (untrusted unless the server is trusted) and `inputSchema`. `listChanged` updates now arrive over `subscriptions/listen`. [E] spec *tools*.
- **HTTP-level discovery:** the only `.well-known` documents in the core spec are OAuth ones: `/.well-known/oauth-protected-resource` (RFC 9728) and authorization-server metadata (RFC 8414 / OIDC). There is **no `.well-known` catalog or "server card" for MCP servers** in 2026-07-28. [E] spec *authorization-server-discovery*; [I] that none exists, based on the full page index in `llms.txt`. agentgateway serves and passes through the OAuth well-known paths. [E] `mcp/auth.rs` `is_well_known_endpoint`.
- **The MCP Registry API** is a generic REST API that any subregistry can implement, with an official instance at `https://registry.modelcontextprotocol.io` (in preview, with "no uptime or data durability guarantees"). [E] registry `docs/reference/api/generic-registry-api.md`, `official-registry-api.md`, `modelcontextprotocol-io/registry-aggregators.mdx`.
  - Endpoints: `GET /v0.1/servers` (`cursor`, `limit`, `search`, `updated_since`, `version=latest`, `include_deleted`), `GET /v0.1/servers/{name}/versions`, and `GET /v0.1/servers/{name}/versions/{version|latest}`. Optional endpoints: `POST /v0.1/publish`, `PUT` and `DELETE` on a version, and `PATCH …/status`.
  - Reading needs no authentication. Aggregators are expected to "scrape … once per hour" and keep their own copy.
  - The ecosystem plan expects clients to read from curated **subregistries**. [E] `docs/design/ecosystem-vision.md`.
- **`server.json`** (current released schema `https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json`): [E] registry `docs/reference/server-json/draft/server.schema.json` and `CHANGELOG.md`.
  - `ServerDetail` requires `name` (reverse-DNS, e.g. `io.github.user/server`), `description` and `version`. Optional fields: `title`, `icons`, `repository`, `websiteUrl`, `_meta`, `packages[]` and `remotes[]`.
  - `packages[]`: `registryType` (npm, pypi, cargo, **oci**, nuget, mcpb), `identifier`, `version`, `transport` (stdio, streamable-http or sse), `runtimeArguments`, `packageArguments`, `environmentVariables` (with `isSecret`, `isRequired`, `default`, `choices`) and `fileSha256`.
  - `remotes[]`: `streamable-http` or `sse` with `url` (which may be templated, e.g. `{baseUrl}/mcp`) and `headers`.
- **Relevance:** `server.json` already holds what Koinon needs for an "add MCP server" form. [I]
  - Container image: `packages[registryType=oci]`.
  - Required env and secrets: `environmentVariables[].isSecret`.
  - Remote URL and headers: `remotes[]`.

---

## 5. What this means for Koinon

### (a) MCP servers that Koinon deploys

1. **Standalone, Koinon-owned gateway, file mode.** Koinon renders `mcp.targets[]` in the file it already renders, and the file watcher hot-reloads it. [E] for the reload. This is the simplest option, and it keeps `mcpAuthorization` in the same file. For containers, Koinon runs the server as a sidecar or Pod and adds a `mcp` target with `host: http://<svc>:<port>/mcp`.
2. **Standalone, hybrid mode.** `PUT /api/config/resources/mcp.target/{name}` adds or replaces one target. `mcp.policy/mcpAuthorization` holds the whole rule set and must be rewritten on every allowlist change. Writes are validated (422 on error) and applied asynchronously. [E]
3. **Avoid stdio for multi-tenant use.** An stdio target spawns a process inside the gateway, with the gateway's privileges, and can be written through the unauthenticated admin API. [E] that stdio is accepted; [I] that this is a security risk. Prefer one container per server, reached over Streamable HTTP.
4. **Shared or Kubernetes gateway.** Deploy the server as a Deployment and Service with `appProtocol: agentgateway.dev/mcp`, the annotation `agentgateway.dev/mcp-target-name: <org>-<server>`, and an org label. An `AgentgatewayBackend` that the platform or Koinon owns then selects by label. [E] for the mechanism; [I] for the naming scheme. No gateway write is needed for each new server, only a Service with the right labels.
5. **Target names must be unique across orgs and match `[a-z0-9.-]`.** Use a stable slug such as `acme-github`. `_` and `+` are reserved. [E]

### (b) Discovering MCP servers the platform registered on a shared gateway

1. **Through MCP (works everywhere, filtered by authorization):** call `tools/list` and friends with a Koinon discovery key that has broad read rules, or with each agent's own key. You get tools and target prefixes, not a target list. This needs `prefixMode` `always` or `conditional` with at least 2 targets to keep the target attribution. [E]/[I]
2. **Standalone management API (read):** `GET /api/config/effective` (or `/api/config` in file mode) gives the full `mcp.targets` and `mcp.policies`. `GET /api/config/resources/mcp.target` returns only DB-backed targets, and only in hybrid mode. The admin port has no authentication, so the platform has to expose it deliberately, ideally behind `ui.policies`. [E]/[I]
3. **Kubernetes:** list and watch `AgentgatewayBackend`, `AgentgatewayPolicy` and the Services that the backends select. [I]
4. **Admin `/config_dump`:** the runtime view, secrets redacted, available in both modes but only on the admin port. [I]
5. **An optional catalog layer:** Koinon could keep (or read from the platform) a small subregistry that speaks the MCP Registry `GET /v0.1/servers` API, with `server.json` entries pointing at gateway targets. This would give names, descriptions and icons that agentgateway does not store. [I]

### (c) Per-agent allowlists

1. **Keep using `mcpAuthorization` with an `apiKey.agent` + `mcp.tool.target` + `mcp.tool.name` rule per agent.** It filters lists and rejects calls on the same rule. [E] Note that any `allow` rule switches on allowlist behaviour for everyone on that backend: callers not covered by a rule see nothing. [E] `RuleSets::validate`.
2. **Hide whole servers with target `condition`s,** for example `apiKey.agent in ["acme/a1","acme/a2"]`, so that unauthorized targets are never contacted. This needs at least 2 targets. [E] Prefer this for org isolation, with `mcpAuthorization` for allowlists at tool level.
3. **Hybrid API granularity:** the whole `mcpAuthorization` policy is one resource (`mcp.policy/mcpAuthorization`), so Koinon must read, modify and write it, and nothing prevents a lost update between writers. Two other options: give each org or tenant its own route or backend (a `traffic.route` resource each), or attach a policy per target with `AgentgatewayPolicy` `targetRefs.sectionName` in Kubernetes. [E] for the granularity; [I] for the mitigation.
4. **On a shared gateway Koinon does not own,** Koinon cannot add rules unless the platform delegates. Options are a route or backend per org that Koinon may write, `AgentgatewayPolicy` objects in Koinon's namespace, or a platform request in the platform team's format. [I]

### Gaps in agentgateway v1.6.0 (as far as these sources show)

- **No API for clients to list targets** and no catalog metadata (description, owner, icon) on a target. [E]/[I]
- **The management API has no auth or tenancy.** Its granularity is coarse for policies (one resource per policy type), it covers only the top-level `mcp` section, and a list shows only DB-backed resources. [E]
- **No upstream authorization-code / per-user consent flow** for remote SaaS MCP servers. Upstream auth is static, signed, or exchanged from an incoming JWT. [I]
- **No `list_changed` push** to clients when targets change. Discovery and list responses are marked uncacheable. [E]/[I]
- **The Kubernetes CRD has no stdio or OpenAPI targets.** Selector-discovered names come from annotations or the Service name and port. The docs and code disagree on SSE support for selectors. [E]
- **`failClosed` by default:** one broken tenant server can break every agent on a shared virtual server, so use `failOpen` or a backend per org. [E]/[I]
- **`prefixMode: never` resolves names by listing every upstream on each call** (there is an open TODO for caching), which is costly with many targets. [E]

---

## Sources

**agentgateway** (https://github.com/agentgateway/agentgateway, tag `v1.6.0`, commit `ea5608642b9d5c94c5baf5fd9f2f9849b808e963`). Base URL `https://github.com/agentgateway/agentgateway/blob/v1.6.0/`:
- `crates/agentgateway/src/types/agent.rs`: `McpPrefixMode`, `McpBackend`, `McpTarget`, `McpTargetSpec`, `validate_mcp_target_name`
- `crates/agentgateway/src/types/local.rs`: `LocalSimpleMcpConfig`, `LocalMcpBackend`, `LocalMcpTarget`, `LocalMcpTargetSpec`, `McpBackendHost`, `SimpleLocalBackendPolicies`, `LocalUIConfig`
- `crates/agentgateway/src/mcp/README.md`: target conditions, version negotiation, caching
- `crates/agentgateway/src/mcp/handler.rs`: `DELIMITER`, `merge_tools` / prompts / resources, `merge_initialize`, `merge_discover`, `get_info`, `get_discovery`
- `crates/agentgateway/src/mcp/session.rs`: `tools/call` authorization (around L600–635)
- `crates/agentgateway/src/mcp/upstream/mod.rs`: `new_for_request` (default target, conditions)
- `crates/agentgateway/src/mcp/rbac.rs`, `crates/agentgateway/src/http/authorization.rs`: rule semantics
- `crates/agentgateway/src/mcp/mod.rs`: `FailureMode`
- `crates/agentgateway/src/mcp/auth.rs`: well-known OAuth endpoints
- `crates/agentgateway/src/http/auth/mod.rs`, `http/auth/oauth/mod.rs`: `BackendAuthKind`, `OAuthGrantType`
- `crates/agentgateway/src/ui.rs`: resource API routes and handlers, `AuthorizationContext`, `get_effective_config`
- `crates/agentgateway/src/config_store.rs`: `ConfigResourceKind`, `MCP_SETTINGS_FIELDS`, `resource_id`, materialization, tests (L2160–2200)
- `crates/agentgateway/src/lib.rs`: `ConfigStoreMode`
- `crates/agentgateway/src/state_manager.rs`: file and DB watch, reload
- `crates/agentgateway/src/management/admin.rs`: `/config_dump`
- `controller/api/v1alpha1/agentgateway/agentgateway_backend_types.go`: `MCPBackend`, `McpTargetSelector`, `McpSelector`, `McpTarget`
- `controller/api/v1alpha1/agentgateway/agentgateway_policy_types.go`: `BackendMCP`, `AgentgatewayPolicySpec`
- `controller/api/v1alpha1/agentgateway/shared_types.go`, `doc.go`
- `controller/api/annotations/agentgateway.go`; `controller/pkg/syncer/backend/mcp.go`, `backend_plugin.go`

**agentgateway docs** (agentgateway.dev, "latest", fetched 2026-10-23):
- https://agentgateway.dev/docs/standalone/latest/documentation/operations/config-resources/
- https://agentgateway.dev/docs/standalone/latest/documentation/mcp/configuration-modes/
- https://agentgateway.dev/docs/standalone/latest/documentation/configuration/security/mcp-authz/
- https://agentgateway.dev/docs/standalone/latest/documentation/mcp/mcp-authz/
- https://agentgateway.dev/docs/kubernetes/latest/documentation/mcp/dynamic-mcp/

**MCP specification** (modelcontextprotocol.io, revision 2026-07-28, fetched 2026-10-23):
- https://modelcontextprotocol.io/specification/versioning
- https://modelcontextprotocol.io/specification/2026-07-28/changelog
- https://modelcontextprotocol.io/specification/2026-07-28/server/discover
- https://modelcontextprotocol.io/specification/2026-07-28/server/tools
- https://modelcontextprotocol.io/specification/2026-07-28/server/utilities/caching
- https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/authorization-server-discovery
- https://modelcontextprotocol.io/llms.txt (page index, used to check that there is no server-card or `.well-known` catalog page)

**MCP Registry** (https://github.com/modelcontextprotocol/registry, `main` at commit `fa509e09c705f52cd1aa79572da07db00eeda195`):
- `docs/reference/api/generic-registry-api.md`, `docs/reference/api/official-registry-api.md`, `docs/reference/api/openapi.yaml`
- `docs/reference/server-json/draft/server.schema.json`, `docs/reference/server-json/CHANGELOG.md`
- `docs/modelcontextprotocol-io/registry-aggregators.mdx`, `docs/design/ecosystem-vision.md`
