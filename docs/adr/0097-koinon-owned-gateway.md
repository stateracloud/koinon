# 0097. A Koinon-owned gateway, in three steps

- Status: Accepted (2026-11-06)
- Date: 2026-11-06
- Amends: [0040](0040-agentgateway-model-and-mcp-gateway.md) (supersedes in part decisions 1, 2 and 6 and
  the "Our own Go proxy" rejection: agentgateway becomes the first driver, not the destination),
  [0013](0013-model-access.md) (the "Our own Go proxy" alternative), [0041](0041-koinon-spend-ledger-and-budget-stops.md)
  (decisions 4 to 6 in step 3: the budget stop moves in front of the call),
  [0050](0050-spend-ledger-ingestion-and-net-limits.md) (the ledger reads a usage record, not access-log
  fields), [0056](0056-model-providers-pricing-and-budgets-per-org.md) (decision 5 in step 3: the
  gateway's own cost and catalog fall away), [0057](0057-mcp-servers.md) (items 4, 5 and 7 in step 2:
  MCP targets, credential injection and the ask target move into Koinon's MCP path),
  [0058](0058-gateway-modes.md) (supersedes in part: shared mode is not built, Kubernetes mode is frozen,
  not ported, and deleted in step 3), [0049](0049-v01-scope-decisions.md) (Kubernetes-mode support ends
  with step 3), [0006](0006-identity-layers.md) (its open trust-domain question, answered in decision 11),
  [0073](0073-agent-passport.md) (decision 6 delivery and the decision 12 delivery spike, in step 2),
  [0084](0084-leak-guard.md) (decision 1, in step 2: the guard is called from Koinon's MCP path, not
  through ExtMcp), [0091](0091-leak-guard-by-destination.md) (decision 4, in step 1: a reply the guard
  never saw sets both legs)
- Relates to: [0068](0068-prompt-caching-always-on.md) (caching stays on through every driver),
  [0069](0069-native-model-formats.md) (native formats, so no format translation),
  [0074](0074-run-traces-trace-router.md) (gateway spans), [0085](0085-upgrade-compatibility-and-recovery-policy.md)
  (patch-line rules for each step), [0045](0045-openshell-hardening-defaults.md) and [0075](0075-isolation-tiers.md)
  (untouched), [0023](0023-observability-and-audit.md) (fixed metric labels); ADR 0096, MCP sign-in
  ([PR #248](https://github.com/stateracloud/koinon/pull/248), not yet merged; its section 3 delivery and
  its gateway spike are affected in step 2)
- Issue: [#258](https://github.com/stateracloud/koinon/issues/258)
- Decision by: the maintainer, 2026-11-06 (the three steps, and at the 2026-11-06 review: gateway
  placement, unpriced models, MCP server-initiated requests, MCP replicas, runaway calls; at the second
  2026-11-06 review: the new step order, platform gateways, dropping Kubernetes mode, SPIRE); the other
  details below are drafted for review

## Context

### How agentgateway got every job

Koinon's first design (ADR 0013) put LiteLLM behind OpenShell's `inference.local`. On 2026-10-21 both
fell away in one day of research: OpenShell 0.1.0 removed `inference.local`, and LiteLLM's tenancy
features turned out to be Enterprise-only (ADR 0040, Context). agentgateway was the only open-source
build with per-key USD budgets, an MCP gateway and an in-path MCP hook, with no enterprise tier. ADR
0040 made it the model gateway and the MCP gateway.

ADR 0040 also priced "our own Go proxy": viable, about 3-5k lines and 4-8 engineer-weeks without
format translation, rejected because it rebuilds what agentgateway provides and adds per-provider
streaming usage parsing. That estimate covered the **model path only**. Nobody priced a Koinon-owned
**MCP path**, and no later decision looked at the question again.

Since then, later ADRs put more on agentgateway because it was already in the path:

| Job | Decision | agentgateway's part | Koinon's part |
| --- | --- | --- | --- |
| Model routing, provider keys, retries, failover | 0040 decisions 1, 3, 4 | routes per org header, attaches provider keys, retries | renders the config (`internal/gateway`) |
| Agent key check | 0040 decision 3 | checks the `kgw_` key hash | mints and hashes keys; the **OpenShell supervisor** swaps the sandbox's placeholder for the key |
| USD budgets | 0041, 0050, 0056 | in-path stop on pushed per-key limits; charges after the response | ledger, budget levels, cost truth from its own price list |
| Usage for the ledger | 0041, 0050 | parses streaming usage, logs `koinon.*` attributes over OTLP | decodes the log (`internal/ledger/otlp.go`) |
| MCP proxying and tool rules | 0014, 0040 decision 2, 0057 | MCP targets, CEL tool allowlist, `<org>-<server>` prefixing, credential files | renders targets and rules; serves the ask target (0057 item 7) |
| Leak guard | 0084 decision 1, 0091 decision 4 | calls the guard through ExtMcp `mcpGuardrails`, failClosed; its response hook sets the session legs | every decision (`internal/mcpguard`) |
| Header allowlist, `kgw_` refusal | 0058 amendment 2026-10-30 | CEL `filterKeys` in standalone; Kubernetes mode cannot, interim 403 guard | the allowlist (`internal/gateway/headers.go`) |
| Model-call spans | 0074 | sends spans to the trace router | the trace router |

So Koinon already owns the decisions. agentgateway is the data plane: it is on every request, and
Koinon depends on how it behaves there, not only on its config format. The `ModelGateway` interface
(ADR 0040 decision 6) fences the config only.

### What that dependency costs

From the dependency review of 2026-11-06 (Links):

- **Security behaviour Koinon cannot patch.** agentgateway v1.7.0 sends only successful MCP replies to
  the guard's response hook; error replies, notifications and server-initiated requests skip it
  (`handler.rs:2183-2208`, upstream #4245), and ADR 0091 sets an MCP tool's session legs only from that
  hook ([#254](https://github.com/stateracloud/koinon/issues/254)). Kubernetes mode cannot strip `kgw_`
  keys from other headers ([#54](https://github.com/stateracloud/koinon/issues/54)). Key-stage refusals
  carry no do-not-retry marker ([#112](https://github.com/stateracloud/koinon/issues/112)).
- **Usage Koinon cannot see.** v1.7.0 drops the Anthropic thinking-token split
  ([#78](https://github.com/stateracloud/koinon/issues/78)), and the log rule that no expression reads a
  response body means Koinon cannot read it itself.
- **MCP sign-in and the passport sit on open upstream bugs.** Per-target outbound auth (#3583) and the
  500-instead-of-401 on token exchange (#3907) are open upstream, and both ADR 0073 and ADR 0096 need a
  spike before anything is built on agentgateway.
- **Speed.** A minor every 4-7 weeks, each moving behaviour that Koinon pins only in code comments.
- **Not agentgateway's fault, but on its path.** Config and key pickup waits on the kubelet tick and a
  fixed 90 s key settle ([#250](https://github.com/stateracloud/koinon/issues/250),
  [#249](https://github.com/stateracloud/koinon/issues/249)); tests pull the image from an upstream
  registry that went down ([#238](https://github.com/stateracloud/koinon/issues/238)).

### What comparable products do

The hosted products (Claude Code on the web, Codex cloud, Amp, Devin) run a proxy they wrote between
the VM and the outside world. Of the self-hosted peers, Coder wrote its own Go gateway (AI Bridge);
Agent Substrate writes its own control plane and uses Envoy or agentgateway only for egress and
credential injection, with no budgets and no tool-call guard. No product researched puts USD budgets
and an MCP tool-call guard on a third-party data plane.

## Decision

### 1. Koinon owns its gateway

The **gateway** (GLOSSARY) becomes Koinon code: a Go data plane for the MCP path and the model path,
behind one Koinon seam. agentgateway is the first **gateway driver** behind that seam, not the
destination. The move happens in three steps, one per minor release. Each step ends only when its exit
criteria hold.

**Order** (maintainer decision 2026-11-06, second review; removal research, Links):

- **v0.1**, unchanged: the fence and the gateway behaviour suite (decision 4).
- **v0.2**: one `koinon-gateway` Deployment serves the MCP path **and** a model path at parity with what
  agentgateway does today (reverse proxy, streaming usage parsing, header allowlist, charge after the
  call, the config stream with revision confirm). Both paths are Koinon's by default. agentgateway stays
  selectable as a fallback for that one minor (decision 5).
- **v0.3**: the improvements (call reservations, the budget totals table, the circuit breaker,
  Koinon-only prices), then agentgateway is deleted (decision 6).

Removal needs parity, not the reservation: agentgateway also charges after the call, so a Koinon model
path that does the same removes it. The model path does not depend on ADR 0073 or ADR 0096, so it can
be built beside the MCP path in v0.2, with the suite as the shared specification.

Who owns each job:

| Job | Today | Step 1 (v0.1) | Step 2 (v0.2) | Step 3 (v0.3) |
| --- | --- | --- | --- | --- |
| Placeholder swap in the sandbox | OpenShell supervisor | unchanged | unchanged | unchanged |
| Agent key check, MCP path | agentgateway | agentgateway | **Koinon** | Koinon |
| Agent key check, model path | agentgateway | agentgateway | **Koinon** | Koinon |
| MCP targets, tool allowlist, prefixing | agentgateway (rendered by Koinon) | agentgateway | **Koinon** | Koinon |
| MCP credentials: static, sign-in (0096), passport (0073) | static only, agentgateway | static only, agentgateway | **Koinon**, all three | Koinon |
| Leak guard: decisions | Koinon | Koinon | Koinon | Koinon |
| Leak guard: which messages it sees | agentgateway's hook (successful replies) | hook, and Koinon fails closed on a reply it never saw | **every message**, in Koinon's MCP path | Koinon |
| Header allowlist, `kgw_` refusal | agentgateway (standalone), interim guard (Kubernetes) | unchanged, in the suite | **Koinon** (both paths) | Koinon |
| Agent header for Koinon's own tool servers | agentgateway | agentgateway | **Koinon** | Koinon |
| Model routing, provider keys, retries | agentgateway | agentgateway | **Koinon**, at parity | Koinon |
| Streaming usage parsing | agentgateway | agentgateway, mapped to the usage record | **Koinon** | Koinon |
| Config delivery | files or CRDs, kubelet tick, 90 s settle | unchanged | **Koinon** State stream with revision confirm | Koinon |
| Cost truth and price list | Koinon (0056 decision 5), agentgateway's catalog as cross-check and fallback | unchanged | Koinon, catalog as today | **Koinon only** |
| Budget stop in the path | agentgateway, charge after the response | unchanged | **Koinon, charge after the response** | **Koinon, call reservation before the call** |
| Model-call spans | agentgateway | agentgateway | **Koinon** | Koinon |
| Egress of the gateway pod | chart NetworkPolicy | unchanged | Koinon's gateway: plain NetworkPolicy | unchanged |
| agentgateway itself | the gateway | the gateway | fallback driver, one minor | **deleted** |

Nine more jobs, each with its gap and how it is filled (own-gateway design research, Links):

| Job | Gap, and how it is filled | Today | Step 1 | Step 2 | Step 3 |
| --- | --- | --- | --- | --- | --- |
| Runaway calls | No per-key request rate limit; a circuit breaker per provider plus the run caps bound a runaway loop (step 3, maintainer decision 2026-11-06) | none found | none | none | **Koinon**: circuit breaker |
| Config delivery | Files, kubelet tick, 90 s settle, probe key; a revisioned State stream the gateway acks (decision 2.1) | files or CRDs | unchanged | **Koinon** (both halves) | Koinon |
| Unknown MCP message kinds | A new method passes unguarded; refuse every method not on an allowlist | agentgateway forwards | unchanged | **Koinon refuses** | Koinon |
| Guard argument order (#256) | Arguments are joined in call order, so a secret split across reordered keys passes; scan a canonical form | Koinon | **Koinon fix** | Koinon | Koinon |
| MCP-call spans | Only implied today; a span per tool call (target, tool, verdict, duration), no arguments or results | agentgateway | agentgateway | **Koinon** | Koinon |
| Audit of tool calls and credential use | The new path must write it; one row per call and per credential use, no token values (ADR 0096) | approvals only | unchanged | **Koinon** | Koinon |
| Availability, timeouts, stream drain, outbound proxy | Koinon owns a hot path; 2+ replicas, a PodDisruptionBudget, drain of open streams, per-stage timeouts, `HTTPS_PROXY` honoured upstream | agentgateway | agentgateway | **Koinon** (both paths) | Koinon |
| The behaviour suite | `gatewaytest` checks rendering only; request-path cases (decision 4) | none | **Koinon** | both drivers | Koinon's gateway (and any third-party driver, decision 10) |
| Gateway pod egress scope | Plain NetworkPolicy cannot filter by name, so egress is broad; the step 2 design states the hosts and ports, no CNI features | chart | unchanged | **stated** | unchanged |

### 2. The seam

The seam is the set of Koinon interfaces every gateway driver sits behind. It is defined by
behaviour, not by config: the gateway behaviour suite (decision 4) is its specification.

1. **Desired state.** `gateway.State` stays the one description of what a gateway serves (keys, limits,
   providers, models, MCP targets, tool grants). In step 2 it splits into a model half and an MCP half,
   and the deferred `MCPGateway` interface of ADR 0040's 2026-10-26 amendment is created, so the two
   paths are built and tested apart and a third-party driver may serve the model half only
   (decision 10). Drivers converge to the state; how is the driver's
   business. agentgateway's drivers keep rendering files or CRDs. Koinon's gateway subscribes to a
   revisioned State stream from koinon-api over Koinon RPC and acks each revision it serves, so for it
   there are no files, no kubelet tick, no key settle and no probe key.
2. **Usage.** Every driver yields **usage records** (decision 3). Nothing outside a driver reads
   gateway log attributes.
3. **Koinon hooks.** The calls a gateway makes into Koinon are Koinon interfaces, with the transport a
   driver detail: the leak guard (`internal/mcpguard`; ExtMcp gRPC for agentgateway, a Go library call
   inside Koinon's gateway, step 2), the token service (ADR 0073, ADR 0096), and in step 3 the call
   reservation.
4. **The sandbox does not know which driver serves it.** The in-cluster host, ports and paths the
   sandbox calls stay fixed across drivers, so OpenShell provider records and policies do not change
   when the driver does. _(inferred)_
5. **No agentgateway names outside the drivers.** The knowledge that leaks today into the reconciler
   (the `koinon-agentgateway-` profile prefix, key-file reload rules), agentd, the ledger, preflight,
   the chart, `web/src/lib/providers.ts` and the `agentgateway.db` path moves behind the driver or to
   neutral names in step 1.

### 3. The usage record

The spend ledger reads a Koinon-defined **usage record**, one per gateway request, and never a
gateway's own fields. It extends today's `gateway.UsageRecord` with what `ledger.Entry` already
decodes from the access log:

- request ID (idempotency key), key ID, agent, run (once ADR 0073's per-run keys land), time;
- model, API format (ADR 0069), streamed or not, HTTP status, whether the upstream answered, the
  gateway's error reason (a cut stream);
- input, output, cache read, cache write and reasoning tokens, each with a "known" flag, in disjoint
  buckets (input excludes cache reads and writes, output excludes reasoning). The driver maps its
  gateway's totals to them (agentgateway's `koinon.*` fields are totals); the ledger prices the
  buckets as they stand and stores the provider's totals in `spend_entries` (#302);
- request `max_tokens` and request bytes (for estimates and reserves, ADR 0056), response ID;
- the gateway's own cost, optional, only while a driver has one.

The agentgateway driver maps its OTLP access log to usage records; that mapping (today
`internal/ledger/otlp.go` and the `LogField*` constants) moves into the driver package in step 1.
Koinon's gateway writes usage records directly. The ledger's pricing, estimates and idempotency do not
change.

### 4. Step 1 (v0.1): keep agentgateway, fence it by contract

Scope:

- **The gateway behaviour suite** (#272). Extend `internal/gateway/gatewaytest` from "what the driver renders"
  to "what the running gateway does", run against the pinned agentgateway image with the fake model
  server and fake MCP servers (no paid model, `testenv.Require` for Docker). Cases:
  - key refusals: missing, unknown, revoked and over-budget keys, each with its status and the marker
    agentd reads (#112);
  - header handling: the allowlist forwards only allowed headers, and a `kgw_` key in any other header
    is refused or stripped (#54);
  - usage per API format and provider dialect Koinon routes (OpenAI chat completions and Anthropic
    Messages; streaming and not; cache read and write; reasoning; a stream cut mid-way): the usage
    record the driver yields matches the fixture (#78, #47);
  - guard coverage per MCP message kind: `tools/call` request and result, a JSON-RPC error reply,
    notifications, server-initiated requests (sampling, elicitation), `tools/list`, `resources/read`,
    `prompts/get`; for each, whether the guard saw it (#254);
  - budget stop: a zero limit refuses; concurrent calls near the limit, with the overshoot measured and
    recorded, not asserted away;
  - no body capture: the rendered log and trace config reads no `response.body` or `bodyPrefix`;
  - the revision probe: a delivered config is reported as served.
  Known upstream gaps are written as expected failures that name their issue, so an upgrade that fixes
  one flips it and an upgrade that breaks a passing case fails.
- **A gateway bump is "suite green".** An agentgateway version enters Koinon only when the suite passes
  against it; a changed expected-failure list is reviewed like code.
- **Close #254 on Koinon's side, fail closed.** An MCP call whose reply the guard never saw counts as
  untrusted: it sets both session legs (untrusted input and private data). The same holds for any MCP
  message kind the guard cannot see. The upstream fix is drafted for a human to post, in
  [`docs/dev/upstream-agentgateway-4245.md`](../dev/upstream-agentgateway-4245.md) (AGENTS.md, OpenShell
  and upstream rules apply to agentgateway too).
  Built (#254): the open-call ledger in `internal/mcpguard` covers a `tools/call` that ends in an error
  reply or is cut short (both legs, a person decides); a reply of any other method the gateway might
  hand the guard (`CheckResponse`, `Guard.CheckUnseen`) takes both legs on the session. What the guard
  is never handed at all (tools/list descriptions, log notifications and server-initiated requests on a
  call's stream) is not visible to any Koinon-side rule on this gateway; it stays the expected failure
  `#254` in `AgentgatewayV170Gaps` until the step 2 gateway, which sees every message.
  *Prior art* (read 2026-11-09; the Docker MCP Gateway's interceptor docs and source were not
  retrievable here, only its README, which describes interceptors as per-call hooks; so the comparison
  below is from the go-sdk source and the MCP specification as vendored in the go-sdk docs):
  the go-sdk `LoggingTransport` wraps a `Connection` and sees every `Read` and `Write` of every message
  kind, errors and notifications included, because it sits below the method dispatch; Koinon borrows
  that placement for step 2 (the guard belongs on the connection, not on a per-method hook) and cannot
  use it now, because agentgateway's hook is per method and per successful result. The spec makes
  sampling, elicitation (form and URL mode) and roots server-to-client requests that a client may
  decline by not declaring the capability; Koinon relies on that for the interim pin, not on a filter.
  Scanners such as Invariant's mcp-scan inspect tool descriptions and results after the fact; Koinon
  differs in deciding before the agent reads and failing closed (both legs) when it cannot.
- **The interim fix has its issue: #269** (exit condition: "the step 2 gateway refuses these itself").
- **Interim pin for server-initiated requests** (maintainer decision 2026-11-06). A #254 spec runs a
  fake MCP server that sends sampling, elicitation (form and URL) and `roots/list` during a
  `tools/call`, and checks that sampling is refused, elicitation never reaches the model, and roots
  shows only the sandbox folder. This holds only because of OpenCode's defaults (it declares no
  sampling, and in ACP mode it cancels MCP elicitations), not because of Koinon, so it is an interim
  fix with its own issue (issue to file), exit condition: "the step 2 gateway refuses these itself"
  (decision 5).
- **The usage record** (decision 3; #273) and the leakage cleanup (decision 2.5).
- **The guard scans arguments in a canonical form** (#256): keys sorted, each value scanned on its own.
- **Fix the `gateway.Limit` comment** (`internal/gateway/gateway.go:300-302`): a limit can be overshot
  by one in-flight response per concurrent call, not by one response.
- **Supply chain (#238).** One mirrored, digest-pinned agentgateway image for the chart, the Go tests
  and kind; no test pulls from `cr.agentgateway.dev`.
- **Kubernetes mode is frozen, then dropped, not ported** (#274; maintainer decision 2026-11-06,
  second review). It keeps passing the suite in v0.1 and gets no new features; Koinon's gateway never gets a
  Kubernetes mode, and it is deleted with agentgateway in step 3. A platform that runs its own
  Kubernetes gateway puts it behind Koinon's (decision 10). Shared mode (ADR 0058) is not built: an
  existing gateway an org already runs is reached as a provider upstream, as ADR 0058 decision 2
  already allows.

Exit criteria:

1. The suite runs in the required Go tests against the mirrored image, every case passes or is an
   expected failure with an open issue.
2. The #254 fail-closed rule is merged with a test against the real image (an error reply carrying a
   canary sets both legs).
3. The ledger reads only usage records; `internal/ledger` imports no agentgateway attribute names.
4. No Go test or CI job pulls agentgateway from an upstream registry.

### 5. Step 2 (v0.2): Koinon's gateway, MCP path and model path at parity

Scope: one `koinon-gateway` Deployment serves both paths and is the default. The MCP path is written in
Go on the official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`), designed together with MCP
sign-in (#237, ADR 0096) and the agent passport (#40, ADR 0073). The model path does what agentgateway
does today, no more (model path at parity, below).

**The MCP path** (#278).

- The MCP path is an aggregating go-sdk server with one go-sdk client per upstream target. The guard
  **wraps each upstream client's `Connection`**, the pattern of the SDK's `LoggingTransport`, so every
  raw JSON-RPC message (result, error, notification, server-initiated request) passes the guard before
  the SDK dispatches it. Method middleware handles grants, the ask tier and request-side checks. The
  guard runs inside the gateway process (placement, below).
- Only methods on an allowlist are forwarded; any other method is refused.
- **Placement** (maintainer decision 2026-11-06). The gateway is a separate `koinon-gateway`
  Deployment from the same image, with its own ServiceAccount, 2+ replicas, a PodDisruptionBudget and
  drain of open streams on shutdown.
  - Only the gateway can decrypt stored credentials (provider keys, MCP credentials, sign-in tokens).
    koinon-api may encrypt (seal) new ones, for example after an OAuth callback lands on the public
    API, but never decrypts them. The gateway's decrypt key comes from OpenBao, the secret store.
  - The leak guard and the passport check run inside the gateway as a library from the same image, so
    there is no network hop per call. koinon-api keeps writing policy to Postgres.
  - Today's facts this changes: koinon-api mounts the KEK (`charts/koinon/templates/deployments.yaml:123`)
    and can `get`, `list` and `watch` every Secret in the namespace (`charts/koinon/templates/rbac.yaml:4-11`).
    A Kubernetes Secret in the same namespace therefore cannot hold the gateway-only key.
  - Unverified: whether OpenBao can hold a gateway-only key in all three DB modes without new
    infrastructure (open question 1, #279).
- **Server-initiated requests** (maintainer decision 2026-11-06). Koinon's gateway owns the client
  side of every upstream session. At initialize it advertises neither `sampling` nor `elicitation` to
  upstream servers, and it answers `roots/list` itself with one fixed folder. A sampling or elicitation
  request that arrives anyway is refused with a JSON-RPC error, logged as a run event, and taints the
  run (a server that ignores the capabilities it was given is not trusted). An `input_required` reply
  (MCP 2026-07-28 multi round-trip requests) is refused the same way. If a connector ever needs human
  input, the question goes to the Koinon inbox for a person to answer, never to the model. Until step
  2 ships, the interim #254 spec of step 1 pins today's behaviour.
- **MCP replicas: stateless** (maintainer decision 2026-11-06). Koinon's MCP path runs the go-sdk
  v1.8.0 in Stateless mode (`StreamableHTTPOptions.Stateless`, `mcp/streamable.go:129-146`). There is
  no session affinity, and a replica restart drops nothing. It gives up server-initiated requests
  (refused anyway) and the standing notification stream, so a changed tool list is seen on the next
  call.
- Each tool call writes one span and one audit row (target, tool, guard verdict, credential used), with
  no arguments, results or token values.
- Per-target credentials are attached by Koinon's MCP path: the static credential (ADR 0057 item 5),
  the MCP sign-in token (ADR 0096) and the passport (ADR 0073). Each exists only between the gateway and
  the MCP server. This replaces ADR 0073's delivery through ext_authz and `backendAuth`, its delivery
  spike, and ADR 0096's gateway spike; the token service in koinon-api stays.
- **The agent header for Koinon's own tool servers** (#281). The GitHub tools MCP server trusts agentgateway's
  agent header today (`charts/koinon/templates/deployments.yaml:265`; removal research A14). Koinon's
  MCP path sets an equivalent, Koinon-named header after the key check, and NetworkPolicy keeps the tool
  servers reachable only from the gateway. Without it the tools break when the default driver changes.
- The ask tier can park a call in the path, so the second `<org>-<server>-ask` target of ADR 0057 item
  7 may fold into the main target (decided in the step 2 design). _(inferred)_
- The same code runs in every cluster as a plain Deployment and Service, so the standalone/Kubernetes
  split ends.

**The model path at parity** (#279; maintainer decision 2026-11-06, second review). Each model family speaks
its native format (ADR 0069), so the gateway forwards bodies and does no format translation.

- **A thin pass-through proxy Koinon writes**: `httputil.ReverseProxy` with `Rewrite`, building each
  outbound request from the header allowlist, and an SSE usage tap per routed format that reads events
  as they pass, keeps no body and writes usage records. It retries or fails over only before the first
  response byte, for what Koinon configures in agentgateway today; do-not-retry markers are Koinon's
  (#112).
- **Budget stop: charge after the call**, against the ledger, as agentgateway does today. The call
  reservation comes in step 3.
- **Prices and catalog as today** (ADR 0056 decision 5) until step 3.
- Model-call spans go from the gateway to the trace router (ADR 0074).

**Config delivery.** Both halves come from the revisioned State stream (decision 2.1); the gateway
confirms each revision it serves. The kubelet tick, the 90 s key settle and agentd's reload waits do
not apply to Koinon's gateway.

**The fallback.** agentgateway stays selectable for v0.2 only, for an install that hits a Koinon
gateway bug. It is the v0.1 setup (agentgateway on both paths), so MCP sign-in and the passport need
Koinon's gateway. _(inferred: the maintainer named the fallback, not its scope)_
- **Design notes for the step 2 design** (per-replica limits, with Envoy's timeout, stream-limit and
  drain defaults as the checklist):
  - a cap on open streams per replica that answers 503 with `Retry-After` before anything is reserved;
  - scaling on an open-streams gauge (fixed labels), not on CPU, since idle streams use no CPU;
  - the upstream connection pool sized up from Go's default of 2 idle connections per host;
  - per-stage timeouts;
  - a shutdown grace period as long as the longest allowed stream, and a PodDisruptionBudget.

Exit criteria:

1. The MCP half of the suite passes against Koinon's gateway with **no** expected failures: the guard
   sees every server message kind, key refusals carry their markers, headers are handled.
2. A different credential per target on a fan-out `tools/list` and `tools/call`, with the static
   server receiving none (the question ADR 0073 and ADR 0096 wanted a spike for).
3. ADR 0096's stage A e2e (fake AS and fake OAuth MCP server on kind) is green.
4. The escape suite finds no sign-in token, passport or MCP credential in the sandbox (ADR 0025).
5. Streamable HTTP and the MCP spec version Koinon pins work with the go-sdk.
6. The model half of the suite passes against Koinon's gateway, including usage for every routed
   format and dialect against recorded fixtures; the budget cases pass as charge-after.
7. The GitHub tools server works behind Koinon's MCP path with the Koinon-named agent header.
8. Every catalog model with a `cache_read` price runs with caching on through Koinon's gateway (ADR
   0068's test).
9. The quickstart e2e is green with Koinon's gateway as the default driver.
10. The build stays within twice its estimate (section Risks); past that, it goes back to the
    maintainer before it continues.

Then Koinon's gateway is the default, and agentgateway is the fallback for v0.2 only.

### 6. Step 3 (v0.3): the improvements, then agentgateway is deleted

Scope: on Koinon's gateway, the call reservation, the budget totals table, the circuit breaker and
Koinon-only prices. Then agentgateway, its drivers and Kubernetes mode are deleted (decision 7).

- **Call reservation.** Before forwarding a model call, the gateway reserves its worst-case cost
  (input estimated from the request, output at the request's `max_tokens`, with a gateway default
  ceiling when the request sets none) against every budget level the ledger keeps (ADR 0041 decision 5,
  ADR 0081). The ledger is the only writer: `Reserve`, `Settle` and `Release` (on timeout) are ledger
  calls, and `Reserve` is one conditional update per budget level in one Postgres transaction. The
  gateway holds no counters, in memory or in Redis. A call whose reservation does not fit is refused
  with the budget marker. When the call ends, the reservation settles to the usage record's cost and
  the rest is released. While the ledger is reachable, concurrent calls can no longer overshoot
  together, which agentgateway's read-then-charge check allows; ADR 0041's "at most one response per
  agent" bound and the pushed per-key limits fall away. The input estimator and the default ceiling
  are set in the step 3 design.
- **Budgets at scale: Postgres only, batched** (decided by the maintainer, 2026-11-06).
  - The ledger keeps at most one reservation write in flight per org. Reservations that arrive
    meanwhile are folded into the next batch: one conditional `UPDATE` that adds their sum when it
    fits, split and retried per call when it does not. Settles and releases only free budget, so they
    may be delayed and batched freely.
  - Step 3 adds a **budget totals table** (spent and reserved per scope and window). Today every check
    sums a month of spend entries (`internal/db/queries/spend.sql:121, 263-274`). The migration is
    expand-only (ADR 0085), and the summing path keeps working beside it until step 3 retires it.
  - Step 3 adds an **expiring reservations table** and a cleanup job that releases money held by a
    gateway that died. A settle that arrives after expiry still records its spend.
  - No Redis or Valkey (Alternatives). No leases now: they are a later option, only if load tests show
    the org row above about 30% of its measured limit with batching. They would live inside the ledger
    client, so the gateway's `Reserve` interface does not change, and they follow the expiry rules of
    Envoy's RLQS and Google's Doorman.
- **Runaway calls: no per-key request rate limit** (maintainer decision 2026-11-06). A circuit breaker
  per provider pauses calls to that provider, for every agent, when its errors pile up. That breaker
  and the existing run caps (requests, USD, wall clock; decided 2026-10-31, ADR 0081) bound a runaway
  loop. Errors are logged and counted with fixed metric labels (ADR 0023).
- **Ledger unreachable at reserve time: allow the call, charge it afterwards** (decided by the
  maintainer, 2026-11-06). The gateway forwards the call without a reservation and charges it from its
  usage record once the ledger is back, for every call it allowed, idempotent on the request ID. The
  consequence, stated plainly: **during a ledger outage, budgets can be overspent by whatever runs in
  that window.** The run caps (requests, USD, wall clock; ADR 0081) still apply. The gateway counts
  calls allowed without a reservation and alerts on them, with fixed metric labels (ADR 0023).
- **Entry work for step 3: three measurements**, recorded in this ADR before step 3 commits to numbers.
  They run at the start of v0.2 (maintainer decision 2026-11-06; #280), since memory per open stream
  matters as soon as the model path exists.
  Docker-gated tests, no cluster:
  1. single-row writes per second on Postgres 18, plain per-call versus batched;
  2. gateway memory per open stream at 1k, 5k and 10k streams per replica, against the fake model
     server;
  3. the MCP `Connection` wrapper on Streamable HTTP.
- Cut streams keep following ADR 0056 decision 6 (the usage tap itself is step 2).
- Cost comes only from Koinon's prices; agentgateway's catalog, the `gateway_cost_usd` cross-check and
  the "served when the gateway can price it" fallback of ADR 0056 decision 5 end.
- **Unpriced models: refuse by default** (maintainer decision 2026-11-06). A call for a model with no
  Koinon price is refused with an error that names the model and says how to price it (ADR 0098), not
  the bare 404 of ADR 0056 decision 2. A platform admin may set an optional fallback price (ADR 0041
  decision 7). It is never zero and must include `max_output`, so a call reservation is possible; a
  zero or missing rate is rejected when saved. Every call charged at the fallback is flagged on its
  ledger row and shown on the spend page. Charging $0, as LiteLLM and Portkey do, is rejected: a $0
  call does not count against any budget.
- To fill the default price list, a reviewed import tool may propose entries from LiteLLM's MIT price
  JSON for a person to approve; nothing is downloaded at runtime (ADR 0098,
  [0098-price-catalogue-updates-from-public-sources.md](0098-price-catalogue-updates-from-public-sources.md)).

Exit criteria:

1. The whole suite passes against Koinon's gateway with no expected failures.
2. A concurrency test holds the budget: with the ledger reachable, N parallel calls near a limit
   never record spend above it. A ledger-outage test shows every allowed call charged once afterwards.
3. A call for an unpriced model is refused with the pricing error.
4. The quickstart e2e stays green.

**Then agentgateway is deleted** (maintainer decision 2026-11-06, second review): its drivers, the
vendored proto, the chart pieces, the spikes and Kubernetes mode (decision 7). No separate removal ADR
is needed.

### 7. What retires

Unused from step 2 on by default (Koinon's gateway), kept only for the v0.2 fallback, deleted in step 3:

| What | Where today | Retires |
| --- | --- | --- |
| The fixed 90 s key settle | `internal/cmd/koinonreconciler/commands.go:132` (`--provider-key-settle`) | step 3; not used by Koinon's gateway from step 2; #250 is closed, not planned (decision 8) |
| ExtMcp hook definitions | `third_party/agentgateway` (vendored proto), `buf.gen.agentgateway.yaml`, `Makefile` target, `internal/mcpguard/grpc.go`, `internal/gateway/guard.go` | step 3 |
| MCP config rendering | MCP targets, `ToolRules` CEL, prefixing, MCP credential files and their settle (`gatewayplan/plan.go`) | step 3 |
| Kubernetes mode | `internal/gateway/kubernetes`, the upstream controller and its CRDs, the #54 interim guard, `.github/workflows/g2-kind.yml` | step 3, not ported |
| Model config rendering | `internal/gateway/standalone` (render, reload, reload log, revision probe) | step 3 |
| The access-log contract | `LogField*` constants and the OTLP decode, as a ledger input | step 1 (moves into the driver); step 3 |
| agentgateway's agent header, trusted by the GitHub tools server | `charts/koinon/templates/deployments.yaml:265` | step 2 (Koinon's MCP path sets a Koinon-named header); step 3 |
| The agentgateway image, chart values, RBAC for its CRDs, metrics port, spikes | chart, `hack/spike/agentgateway*`, `hack/chart-check.d` | step 3 |

### 8. Effect on open issues

- **#254** (guard misses MCP errors and server messages): step 1, closed on Koinon's side by the
  fail-closed rule; fully closed in step 2, where the guard sees every message.
- **#249** (wake on credential arrival): lands in v0.1.
- **#250** (config pickup ADR): closed as not planned (the maintainer, 2026-11-06). Neither the single
  projected volume nor the pod-annotation touch with its `patch pods` grant is built. The gateway's
  config and key pickup keeps waiting on the kubelet tick, and the 90 s key settle stays, until step 2
  makes Koinon's State stream the default; both are deleted with agentgateway in step 3. The pickup
  research (Links) stays as the evidence for that choice.
- **#54** (Kubernetes-mode header allowlist): stays on the interim 403 guard in v0.1; Koinon's gateway
  has no Kubernetes mode (step 2), and the guard is deleted with Kubernetes mode in step 3. Moves to
  v0.2.
- **#112** (do-not-retry markers): the suite records today's markers in step 1; both paths' markers are
  Koinon's in step 2. Moves to v0.2.
- **#78** (thinking-token split): an expected failure in the suite from step 1; fixed by an upstream
  release that passes the case, or in step 2 by Koinon's parser. Moves to v0.2 unless upstream fixes it
  first.
- **#238** (upstream registry outage): step 1, as above; unchanged by the later steps while the
  agentgateway driver lives.
- **#40** (passport delivery spike on agentgateway): re-scoped in step 2 to passport delivery in
  Koinon's MCP path; the agentgateway spike is not needed.
- **#237** (MCP sign-in): built in step 2, as ADR 0096 plans; its delivery is Koinon's MCP path.

### 9. Rules that hold in every step

- **No secrets in sandboxes.** Provider keys, MCP credentials, sign-in tokens and passports exist only
  in the gateway and koinon-api (from step 2, koinon-api only seals them and the gateway alone decrypts
  them); the sandbox holds only its placeholder, swapped by the OpenShell supervisor. ADR 0044 stays the only, gated exception.
- **Never Kyverno.** Any admission fence a step needs is a native ValidatingAdmissionPolicy.
- **No CNI-specific features.** The gateway is isolated with standard NetworkPolicy, as today.
- **Fixed metric labels.** Gateway metrics carry no org, agent, user, ticket or run ID (ADR 0023).
- **Prompt caching always on.** Every driver forwards the harness's cache markers unchanged, and
  usage records carry cache reads and writes (ADR 0068).
- **OpenShell defaults untouched.** `enforcement: enforce` in every tier (ADR 0045) and the isolation
  tiers (ADR 0075) do not change; the sandbox-facing gateway endpoint stays the same (decision 2.4).
- **No response bodies recorded.** agentgateway's config never reads `response.body`; Koinon's gateway
  parses usage from the stream as it passes and keeps no body.
- **Never delegated** (maintainer decision 2026-11-06, second review). The MCP path, the leak guard, the
  passport, the run caps and the ledger are always Koinon's, whatever gateway a platform runs
  (decision 10).

### 10. Gateways a platform already runs

Maintainer decision 2026-11-06, second review (removal research, section B, Links).

Some platforms already run a gateway: Gateway API with the Inference Extension, a platform-run kgateway
or agentgateway, Agent Router (formerly Envoy AI Gateway), WSO2, Kong, Azure API Management or Apigee.

- **Koinon's gateway is always in front.** Theirs sits behind it, as a provider base URL (its key is a
  Koinon provider credential, and Koinon's prices still apply) or as an MCP target with its own
  credential or sign-in. No product-specific code. This also replaces Kubernetes mode for platforms
  that run agentgateway or kgateway themselves.
- **Why not theirs in front.** None of these products documents a budget reservation before the call:
  each limits spend from the response after it, and Azure documents that concurrent calls overspend.
  None offers a hook that shows every MCP message, errors and server requests included, to an outside
  checker. So the leak guard and the budget stop cannot move into any of them.
- **A third-party gateway may only replace the model path**, as a driver that passes the behaviour
  suite. The driver contract:
  1. it checks the Koinon agent key (hash from State) and reports the key ID;
  2. it builds outbound headers from Koinon's allowlist and refuses `kgw_` keys in other headers;
  3. it yields one usage record per call (decision 3);
  4. it asks the ledger before the call (`Reserve`, then `Settle` or `Release`), or runs in a flagged
     charge-after mode that the ledger marks and the spend page shows as weaker;
  5. it passes prompt-cache markers unchanged (ADR 0068) and records no response bodies;
  6. it follows Koinon's config revisions and confirms each one;
  7. its metrics carry fixed labels (ADR 0023).
- **No vendor driver is built until someone asks.** The contract and the suite are published; a
  driver is written against them when a platform needs its gateway to be the model data plane.

### 11. SPIRE, when a platform runs it

Maintainer decision 2026-11-06, second review (removal research, section C, Links).

1. **Koinon's own processes.** When the SPIRE Workload API socket is present
   (`SPIFFE_ENDPOINT_SOCKET`), koinon-api, koinon-gateway, agentd and the reconciler take X.509-SVIDs
   through go-spiffe (`github.com/spiffe/go-spiffe/v2`) and use mTLS between them, authorizing the peer's
   SPIFFE ID on each listener. The gateway authenticates to the token service (ADR 0073) with its
   JWT-SVID as the OAuth client assertion (IETF draft-ietf-oauth-spiffe-client-auth). The token
   exchange subject stays the agent; the gateway is the actor. Koinon installs no SPIRE.
2. **Never in a sandbox.** The SPIRE socket is never mounted into a sandbox: an agent with the socket
   could fetch a usable identity, which breaks "no secrets in sandboxes". A ValidatingAdmissionPolicy
   refuses SPIFFE CSI volumes and the socket's hostPath in sandbox pods (#282), and an
   escape-suite test checks that no SPIFFE socket is reachable from a sandbox (#282).
3. **Agents are not SPIRE workloads.** One agent spans many sandboxes over its life, and SPIRE attests
   pods and processes. Minting agent IDs in the platform's SPIRE would need an admin identity that can
   mint any ID in the platform's trust domain. Instead, Koinon's own issuer (ADR 0043) runs an
   **agents trust domain** (for example `spiffe://agents.<install-domain>/org/<org>/agent/<agent>`)
   and publishes a SPIFFE bundle that the platform's SPIRE can federate with. Koinon needs no SPIRE
   admin identity. This answers ADR 0006's open trust-domain question.
4. **Without SPIRE nothing changes.** Audience-bound ServiceAccount tokens and the chart's TLS stay the
   default; SPIRE is an optional input, never required.

Unverified: whether the Kata `vm` tier (ADR 0075) can see a host socket at all; no source was found.
The rule in item 2 holds either way.

## Alternatives considered

| Option | Why not |
| --- | --- |
| **Stay on agentgateway for good and fence it** (the dependency review's option A alone) | Cheapest now, and step 1 is exactly this. But each minor can move behaviour the guard and the ledger depend on, security fixes wait for a Rust upstream (#4245 open since 2026-10-30, #3907 since 2026-10-10, neither with a PR), Kubernetes mode keeps a known bypass (#54), and sign-in and the passport get built on open MCP auth bugs. |
| **Own the MCP path only**, agentgateway for models for good | Takes the part where Koinon's security lives, and is step 2. Stopping there keeps the budget stop after the call (concurrent overshoot), usage parsing Koinon cannot fix (#78), the price catalog cross-check, the config rendering and Kubernetes mode for the model path. |
| **Swap to another gateway** (Agent Router, Bifrost, LiteLLM, Portkey, Kong, ContextForge) | The same coupling with a new vendor, and the kubelet and registry costs come along unchanged. None has an open-source fail-closed in-path MCP hook as complete as ExtMcp (review F9). Bifrost's open-source governance plugin does have virtual-key, team and customer budgets and MCP governance, but its check is read-then-charge in memory per replica (`store.go:2248-2283`); user-level limits and cross-replica sync are Enterprise (per Obot's ADR). Agent Router stays the named fallback for the model path (#98). |
| **The earlier order: MCP path in v0.2, model path in v0.3, removal later** | A smaller v0.2, but agentgateway, the kubelet config path, the 90 s key settle and two drivers would stay into v0.4, and every agentgateway minor in that window could still move ledger and guard behaviour. Removal needs parity, not the reservation (decision 1). |
| Model path with the call reservation in v0.2 | Removes the overshoot a minor sooner, but puts a new money path and a migration into the same minor as the MCP rewrite and sign-in; slip risk on all three. |
| Port Kubernetes mode to Koinon's gateway | Keeps writing another project's CRDs; a platform's own Kubernetes gateway works behind Koinon's as an upstream (decision 10). |
| A platform gateway in front, or replacing the MCP path | No product researched reserves budget before the call or shows every MCP message to an outside checker (decision 10). |
| "Bring your own gateway" that only reads its logs for spend | Loses the budget stop and the leak guard entirely. |
| Agent identities as SPIRE workloads, or minted by an admin SVID in the platform's SPIRE | Puts a usable identity in the sandbox, or asks the platform for a privilege that can mint any ID; agent identity would then depend on the platform's SPIRE and its naming, and backing out means re-issuing every agent identity (decision 11). |
| **Build the whole gateway in v0.1** | Delays v0.1 by ADR 0040's 4-8 weeks plus the MCP path, and builds OAuth plumbing before ADR 0096 settles. The suite would not exist yet to say when the new gateway is good enough. |
| Embed a gateway engine (Bifrost) for the model path, as Obot (MIT) did | Less parsing code, but: a large dependency tree whose internal API is no stable contract (Obot's ADR), format translation Koinon does not want (ADR 0069), budgets that overshoot across replicas, a pricing datasheet whose terms forbid redistribution, and frequent releases against the 3-day rule (ADR 0079). Obot still had to write budgets itself, and Koinon would still write budgets, the guard and usage records. At most a source of ideas for step 3. |
| MCP path with the guard in method middleware only (as Docker's MCP gateway does) | Errors reach middleware as Go `error` values and transport-level messages are out of reach, so the guard would again miss replies: the #254 class in Koinon's own code. |
| Budget counters in the gateway's memory | Overshoot by up to N times with N replicas. |
| Redis or Valkey for budget counters | Budgets are money. Redis replication is asynchronous and can lose acknowledged writes on failover, so Postgres would still be the truth and Koinon would run two stores and a reconcile between them, the class of bug LiteLLM keeps fixing. It adds a stateful service to size, persist, fail over and back up in all three DB modes, and a fail-open or fail-closed choice when it is down (and Redis 8's licence means Valkey, one more supply-chain line). The headroom is not needed: one batched Postgres row per org covers the expected load. |
| Envoy AI Gateway (Agent Router) as the data plane | Charges after the response and accepts overshoot, needs Redis for its limits, and brings several components plus the vendor coupling this ADR removes. |
| Envoy in front, with a Koinon `ext_proc` server | Buys connection handling only. Koinon would still write the usage tap, reservation, guard and MCP aggregator, every stream chunk would cross gRPC, and operators would run one more component. Kept as an escape hatch if Go's connection handling ever proves to be the limit. Koinon borrows Envoy's designs instead: RLQS and Doorman lease expiry, and its timeout, stream-limit and drain defaults as a checklist. |
| Move only the proxy out of koinon-api, keep the KEK there | Buys availability and scaling now, but koinon-api could still decrypt every key, so "keys stay out of the internet-facing process" would be false. Fixing it later means re-encrypting every stored credential and changing RBAC on running installs (ADR 0085). |
| Run the gateway inside koinon-api | Every API deploy cuts open model streams, the API needs 2+ replicas for the hot path, and one public-API bug exposes every key. |
| Guard and passport check over a Koinon RPC per call | A network hop and a koinon-api dependency on every message; a library in the gateway process does the same work. |
| Charge unpriced models $0 (LiteLLM, Portkey) | A $0 call counts against no budget: a silent hole in a hard stop for unattended agents. |
| Allow MCP sampling and elicitation and taint the run | Taint covers what flows into the model, but the sampled completion and the elicitation answer flow back to the server, and the guard never checks that as an outgoing write. |
| MCP session affinity (`sessionAffinity: ClientIP`) | Ties sessions to replicas, so a restart drops them; the go-sdk's Stateless mode needs no affinity. |
| Per-key request rate limits | The run caps already bound one runaway agent, and a per-provider circuit breaker covers a failing provider for every agent at once; a third limit adds settings without a case it alone covers. |
| Download a price list (LiteLLM's) at runtime | A supply-chain path into cost truth; LiteLLM had two compromised releases in 2026. A reviewed import is used instead. |

## Consequences

- Positive: security behaviour (guard coverage, header refusal, credential attachment) is Koinon Go
  that Koinon tests and patches.
- Positive: the suite is useful at once (every agentgateway bump becomes a test run) and is the
  acceptance test for Koinon's gateway, so step 1's work is not thrown away.
- Positive: budgets hold under concurrency from step 3, with Postgres as the only store for money; one
  code path runs in every cluster.
- Negative: during a ledger outage, budgets can be overspent by what runs in that window; only the run
  caps bound it, and the calls are charged afterwards.
- Positive: about 2.9k lines of config rendering for another project's schema, a vendored proto, the
  Kubernetes-mode driver and two upstream registries go away over the three steps. _(inferred)_
- Negative: Koinon owns per-provider streaming usage parsing, MCP spec churn and a high-traffic data
  path, including its availability and performance.
- Negative: two drivers run side by side for v0.2, and the suite must run against both in that minor.
- Negative: v0.2 is larger: both paths, sign-in and the passport land in one minor. The model path has
  no dependency on ADR 0073 or ADR 0096, so it runs as a parallel lane.
- Positive: agentgateway, Kubernetes mode, the kubelet config path and the 90 s key settle are gone in
  v0.3, not v0.4.
- Negative: the MCP sign-in and passport work (ADR 0096, ADR 0073) changes delivery mechanism; their
  agentgateway spikes are dropped. _(inferred)_
- Follow-ups: issues for the step 1 items (suite, usage record, leakage cleanup, mirror, #254 fail
  closed, Kubernetes-mode freeze); a step 2 design issue joined to #237 and #40, with a model-path lane;
  a step 3 issue; the SPIRE work of decision 11 (identity source package, the sandbox admission policy
  and its escape-suite test, the agents trust domain with ADR 0043's issuer); on
  acceptance, "Amended by 0097" lines in the ADRs listed under Amends, the GLOSSARY's **Gateway** entry
  (no longer "agentgateway"), and a `docs/design.md` row; ADR 0096 updated to name Koinon's MCP path as
  its delivery.

### Risks

- **Per-provider streaming usage parsing.** This is ADR 0040's original reason to say no, and the main
  ongoing cost. Koinon routes two API formats (ADR 0069), but each provider has its own dialect
  (OpenRouter, Ollama, OpenAI-compatible servers; `include_usage` chunks, Anthropic's
  `message_start`/`message_delta` counts). Mitigation: fixtures recorded from each provider in the
  suite, cut-stream estimates as today (ADR 0056), and the provider usage cross-check (#47,
  `internal/providerusage`).
- **The price catalog.** Koinon already prices every request itself (ADR 0056 decision 5), so what is
  lost is agentgateway's catalog as a fallback for models without a Koinon price and the 1% cross-check.
  The default price list (14 entries, hand-reviewed) must cover every catalog model before step 3.
- **Unverified size estimates.** About 800-1,500 lines for the MCP path and 3-5.5k lines in all, and
  ADR 0040's 4-8 weeks for the model path, are estimates from reading, not measured. Each step checks
  its size against the estimate (exit criterion "within twice its estimate") and returns to the
  maintainer past it. Net Go "close to flat" after retirement is a guess.

## Open questions

Tracked in #279 (the model path design).

1. **OpenBao and the gateway-only key** (unverified): whether OpenBao can hold a key only the gateway
   can use, in all three DB modes, without new infrastructure. Checked in the step 2 design; the
   placement decision (step 2) does not change, only how the key is provisioned.

Decided and moved into the decision sections: gateway placement, MCP server-initiated requests and
MCP replicas (step 2), unpriced models and runaway calls (step 3), all at the maintainer's 2026-11-06
review; the call reservation details (step 3). On 2026-11-06 (acceptance): the step 3 entry
measurements run at the start of v0.2 (decision 6, #280). At the second 2026-11-06 review: the new order (decision
1), when the agentgateway driver is removed (in step 3, decision 6), Kubernetes mode (dropped, not
ported, decision 4), platform gateways (decision 10) and SPIRE (decision 11).

## Upstream verification

Checked 2026-11-06 in the research reports (Links), from source and docs; nothing downloaded was run:

- agentgateway v1.7.0 (`26f5aebe`): `mcp/handler.rs:2183-2208` sends only `Response` messages to the
  guardrail response hook; `http/budget/mod.rs` checks an in-memory counter before the call
  (`:554-559`), pushes settlement after it (`:561-564`) and settles in `settle()` (`:493-519`);
  `messages.rs` never sets `reasoning_tokens`. Upstream issues #4245, #3907, #3583 open.
- `github.com/modelcontextprotocol/go-sdk` v1.8.0 (already in `go.mod`): Streamable HTTP, SSE and
  stdio, client and server, Apache-2.0/MIT. It speaks the 2026-07-28 spec (`latestProtocolVersion`,
  `mcp/shared.go:50-51`), and the Go proxy serves it as a release, not a pre-release. Its transport
  `Connection` sees every raw JSON-RPC message (`LoggingTransport`, `transport.go:356`). It has a
  Stateless mode (`StreamableHTTPOptions.Stateless`, `mcp/streamable.go:129-146`).
- OpenCode 2.2.0 declares no sampling capability and in ACP mode cancels MCP elicitations; agentgateway
  v1.7.0 relays server-initiated requests to legacy clients (MCP server-initiated requests research,
  Links).
- Bifrost: the open-source governance plugin has budgets and MCP governance
  (`plugins/governance/main.go:39-45`); its check is read-then-charge (`store.go:2248-2283`). Obot's
  licence is MIT (GitHub metadata).
- Go 1.27.3 `net/http/httputil.ReverseProxy` flushes `text/event-stream` at once
  (`reverseproxy.go:681-696`).
- SPIRE (removal research, Links): the server's `MintJWTSVID` and `MintX509SVID` mint one-off SVIDs
  only for a local or admin caller; federation works through a bundle endpoint per trust domain.
  Agent Router's MCP docs show no hook for an outside MCP message checker; Azure API Management's
  `llm-token-limit` states that concurrent calls can overshoot.
- Not verified: Kata `vm` tier access to a host socket; the SPIFFE CSI driver's maturity; Coder AI
  Bridge's budget enforcement; hosted products' internal routing (inferred from
  their docs); Agent Substrate's default data plane.

## Links

- Research (outside the repository, 2026-11-06): "Why is Koinon so dependent on agentgateway?", "agentgateway
  dependency review" and "Gateway config and credential pickup (#250)", in the research store. The last is kept as the
  evidence for closing #250 without building it.
- Research: "Koinon's own gateway: gaps, prior art, libraries, architecture, user stories"
  (`2026-11-06-own-gateway-design.md`, research store), and its page:
  https://claude.ai/artifact/XvE2bhuyVvbnjPrSjpT2BH
- Research: "Koinon gateway: what is open, why not Redis, scaling, Envoy prior art"
  (`2026-11-06-gateway-scale-redis-envoy.md`, research store)
- Research (research store, 2026-11-06): "Gateway placement and unpriced models"
  (`2026-11-06-gateway-placement-unpriced.md`) and "MCP server-initiated requests: refuse, or allow and
  taint?" (`2026-11-06-mcp-server-initiated-requests.md`)
- Research (research store, 2026-11-06): "Removing agentgateway, integrating platform gateways, and
  using a platform's SPIRE" (`2026-11-06-gateway-removal-integrations-spire.md`); the removal
  research cited above. Sources there: Agent Router MCP docs, Azure API Management `llm-token-limit`,
  SPIRE server and Delegated Identity APIs, go-spiffe v2, draft-ietf-oauth-spiffe-client-auth.
- [0040](0040-agentgateway-model-and-mcp-gateway.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0049](0049-v01-scope-decisions.md), [0057](0057-mcp-servers.md), [0058](0058-gateway-modes.md),
  [0073](0073-agent-passport.md), [0084](0084-leak-guard.md), [0091](0091-leak-guard-by-destination.md)
- ADR 0096 (MCP sign-in): [PR #248](https://github.com/stateracloud/koinon/pull/248)
- Issues: [#258](https://github.com/stateracloud/koinon/issues/258), #254, #250, #249, #238, #237, #112,
  #98, #78, #57, #54, #47, #40
- https://github.com/agentgateway/agentgateway/issues/4245
- https://github.com/modelcontextprotocol/go-sdk
- https://github.com/coder/aibridge
