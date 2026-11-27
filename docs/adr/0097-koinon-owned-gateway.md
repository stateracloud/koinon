---
status: accepted
date: 2026-11-06
area: models-spend
tags: [agentgateway, mcp, models, spend-ledger, leak-guard]
amends: ["0006", "0013", "0040", "0041", "0049", "0050", "0056", "0057", "0058", "0073", "0084", "0091"]
relates: ["0023", "0045", "0068", "0069", "0074", "0075", "0085", "0096"]
---
# 0097. A Koinon-owned gateway, in three steps

- Status: Accepted (2026-11-06)
- Amended by: [0119](0119-agent-substrate-execution-layer.md) (Agent Substrate execution layer); [0120](0120-execution-hardening-defaults.md) (execution hardening defaults)
- Date: 2026-11-06
- Area: models-spend
- Tags: agentgateway, mcp, models, spend-ledger, leak-guard
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
  [0073](0073-agent-passport.md) (decision 6 delivery and the decision 12 delivery spike, in step 2;
  0073 decision 1 subject and 0073 decision 13, #1235),
  [0084](0084-leak-guard.md) (decision 1, in step 2: the guard is called from Koinon's MCP path, not
  through ExtMcp), [0091](0091-leak-guard-by-destination.md) (decision 4, in step 1: a reply the guard
  never saw sets both legs, for a call whose declared result would set a leg)
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

_(Changed by [0119](0119-agent-substrate-execution-layer.md): actor token identity; forge and registry L7 routes join the gateway (D1, D11); phase 1.)_

_(Changed by [0120](0120-execution-hardening-defaults.md): the gateway is the only L7 enforcement point, and it has no audit-only mode.)_

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
- **Close #254 on Koinon's side, fail closed.** A call opens in the open-call ledger only when the
  server's declared result would set a leg (ADR 0091 trusts the declaration). When such a call's reply
  never reaches the guard (an error reply, or a cut stream), the call counts as untrusted and sets both
  session legs (untrusted input and private data). A call to a server declared with neither leg opens
  nothing, so an error reply from it sets nothing: this is weaker than "every unseen reply sets both
  legs". A reply of a method other than `tools/call` that the gateway hands the guard does set both legs. The upstream fix (agentgateway#4245) is drafted in the #254 pull
  request for a human to post in their own words (the AGENTS.md upstream rules apply to agentgateway too).
  Built (#254): the open-call ledger in `internal/mcpguard` covers a `tools/call` that ends in an error
  reply or is cut short, when the server's declared result would set a leg (both legs, a person decides); a reply of any other method the gateway might
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
  fix with its own issue (#269), exit condition: "the step 2 gateway refuses these itself"
  (decision 5). Exit met (#1239): koinon-gateway, the default MCP driver, refuses sampling and
  elicitation (form and URL mode) itself and answers `roots/list` with its one fixed folder. The #254
  spec (`test/harnesses/servermessages_test.go`) now runs against it, and `internal/mcpgateway` has a
  spec for each refusal. Behind the agentgateway fallback, the client still decides.
- **The usage record** (decision 3; #273) and the leakage cleanup (decision 2.5).
- **The guard scans arguments in a canonical form** (#256): keys sorted, each value scanned on its own.
- **Fix the `gateway.Limit` comment** (`internal/gateway/gateway.go:300-302`): a limit can be overshot
  by one in-flight response per concurrent call, not by one response.
- **Supply chain (#238).** One mirrored, digest-pinned agentgateway image for the chart, the Go tests
  and kind; no test pulls from `cr.agentgateway.dev`.
- **Kubernetes mode is frozen, then dropped, not ported** (#274; maintainer decision 2026-11-06,
  second review). It keeps passing the suite in v0.1 (except with the Substrate driver: section 16) and gets no new features; Koinon's gateway never gets a
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
- The step 1 drift rule ("a non-`tools/call` reply sets both legs") is not carried over as is. Once
  Koinon's gateway sees every message, it taints only what it cannot read, and it scans the kinds it
  reads (`initialize` instructions, `resources/read`) by content and declared legs. An unknown kind
  still sets both legs. A `tools/list` is not session data: it is reviewed configuration, pinned to the
  hash an admin approved (decision 13, amendment 2026-11-23, #1132). Tool text is never
  fingerprinted. Approved text sets no leg. Unreviewed text sets only the untrusted-input leg, and only on a server that
  declares it. A list the guard cannot read keeps both legs.
- Only methods on an allowlist are forwarded; any other method is refused.
- **Placement** (maintainer decision 2026-11-06). The gateway is a separate `koinon-gateway`
  Deployment from the same image, with its own ServiceAccount, 2+ replicas, a PodDisruptionBudget and
  drain of open streams on shutdown.
  - Only the gateway can decrypt stored credentials (provider keys, MCP credentials, sign-in tokens).
    koinon-api may encrypt (seal) new ones, for example after an OAuth callback lands on the public
    API, but never decrypts them. How the gateway gets its decrypt key, and what that means for each
    KEK backend, is decision 12 (it replaces the earlier text "comes from OpenBao").
  - The leak guard and the passport check run inside the gateway as a library from the same image, so
    there is no network hop per call. koinon-api keeps writing policy to Postgres.
  - Today's facts this changes: koinon-api mounts the KEK (`charts/koinon/templates/deployments.yaml:123`)
    and can `get`, `list` and `watch` every Secret in the namespace (`charts/koinon/templates/rbac.yaml:4-11`).
    A Kubernetes Secret in the same namespace therefore cannot hold the gateway-only key.
  - Open question 1 is answered in decision 12: a key wrapped by a separate KMS key, not a
    Kubernetes Secret and not a second store.
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
- **Retention of `mcp_call_log`** (#278). The table follows the rule of the audit tables (ADR 0100):
  Koinon deletes no row in v1, and audit retention is its own policy (#29). A row has a fixed small
  size: bounded text columns and no argument, result or token value. One policy for all audit tables is
  better than a rule for this table alone. The gateway role has INSERT on the table and nothing else,
  so it cannot delete rows.
- **Custody in the chart** (#278, step 2). `koinon-gateway` has its own database login
  (`koinon_mcpgateway`, in the group role `koinon_gateway` only, never in `koinon_app`) and mounts its
  own credentials Secret (`gateway.mcpKeysSecret`), not the provider keys. The reconciler writes the
  `mcp-*` and `mcpca-*` files to that Secret. koinon-api checks an agent's live MCP tools through
  `gateway-mcp-check-url`, which stays on the agentgateway URL until agents move to this path.
- Per-target credentials are attached by Koinon's MCP path: the static credential (ADR 0057 item 5),
  the MCP sign-in token (ADR 0096) and the passport (ADR 0073). Each exists only between the gateway and
  the MCP server. This replaces ADR 0073's delivery through ext_authz and `backendAuth`, its delivery
  spike, and ADR 0096's gateway spike; the token service in koinon-api stays.
- **The agent header for Koinon's own tool servers** (#281). The GitHub tools MCP server trusts agentgateway's
  agent header today (`charts/koinon/templates/deployments.yaml:265`; removal research A14). Koinon's
  MCP path sets an equivalent, Koinon-named header after the key check, and NetworkPolicy keeps the tool
  servers reachable only from the gateway. Without it the tools break when the default driver changes.
- **An org's MCP server gets no agent header** (maintainer decision 2026-11-24, #1235). Koinon's MCP path
  sets the agent header only on Koinon's own services: a platform target (Koinon's own tool servers, `Org ""`)
  and an ask target (Koinon's approval proxy for the ask-tier tools of an org's server, `internal/mcpask`;
  #1315). The gateway plan alone marks an ask target (`MCPTarget.Ask`); it keeps the org, for org scoping and
  tool pins, and carries no credential (`gateway.MCPTarget.TrustsAgentHeader`,
  `internal/mcpgateway/credential.go`). It never sends it to a server an org registered, and there is no
  opt-in for it. An org's server trusts nothing of Koinon's, and the agent ID is internal data. A plain
  header can be forged: server owners would build access checks on it, and a signed credential later
  would then need two identity paths or break those servers. An org's server learns the caller only from
  a signed credential made for that one server: the passport (ADR 0073 decision 13) or the server's own
  sign-in token (ADR 0096, #237), when those are built. When the gateway speaks an MCP revision with
  client information on each request, it sends its own client information, never the agent's, and trace
  baggage that goes to an org's server carries no agent ID.
  - **The fallback gateway still sends the agent header to an org's server** (maintainer decision
    2026-11-24, #1235). agentgateway sends the agent header to every MCP target that it calls, until step 3
    deletes that path. The render does not change. The driver selects which calls go through
    agentgateway. With driver `agentgateway` (the default until #1239, and the fallback after it), all MCP
    calls go through it. With driver `koinon`, every call through koinon-gateway gets no agent header on an
    org's server. Since #1239, an agent's own MCP calls, koinon-api's live check of an agent's tools and
    the reconciler's probe go through koinon-gateway with driver `koinon`, so an org's server sees no agent
    header with the default driver. Before #1239, those calls went through agentgateway with both drivers,
    and driver `koinon` removed the header only from the calls that a human approved.
  - The reason that the render does not change: in Kubernetes mode, a `transformation` on the one MCP
    route sets the header before agentgateway selects the target, so it cannot leave out one target. A
    `transformation` on the Backend runs after agentgateway injects the target's credential; the one that
    Koinon tried stripped the credential ([research](../research/agentgateway-k8s-crds.md)). A Backend for
    each org would change the Kubernetes-mode shape that section 4 of this ADR freezes. In standalone mode,
    a policy on each target (`requestHeaderModifier` or `transformations` of `LocalMcpTarget`;
    [research](../research/mcp-discovery-agentgateway.md)) can probably remove the header from an org's
    target. Koinon does not do this: step 3 deletes the path, and no test proves that such a policy keeps
    the target's credential on agentgateway v1.7.0. The operator text of `gateway.mcp.driver` in
    `charts/koinon/values.yaml` tells an operator about this disclosure.
  - The e2e assertions (`test/e2e/mcp_test.go`, `test/e2e/quickstart_test.go`) follow the path that each
    call uses: an agent's call, a call a human approved, and a call from a client outside the cluster. An
    org's fake server sees the agent (or Koinon's executor) on the agentgateway path, and no agent header
    on Koinon's MCP path. The e2e install runs the Koinon case when koinon-gateway becomes its default
    (#1239).
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

Record (2026-11-12, #279 slice 2; the choices below are decided): the stream is backed by two
install-wide tables (migration 00082), `gateway_state_revisions` (one revision sequence for the whole
State, a full secret-free snapshot per revision with a digest) and `gateway_state_acks` (gateway copy,
revision, half). The reconciler applies the state, then stores a revision only when the snapshot
differs from the latest one, so the stored revision records what was applied. Each new revision sends
`pg_notify` on `koinon_gateway_state`. Retention keeps the newest 100 revisions and drops any older
than 30 days, never the newest; snapshots hold org names and URLs, so the age bound also limits how
long a deleted org's data stays. An ack of a revision covers every earlier one for that half. The
`copy_id` of an ack is unique per process incarnation (for example pod UID plus a boot nonce) and a
copy never reuses it after a restart. S3 note: LISTEN does not work behind transaction-mode
PgBouncer, so the stream client also polls the table as a fallback.

Record (2026-11-25, #1239; the choices below are decided): with koinon-gateway as the default MCP driver,
the stored revision is the State that koinon-gateway serves, not a record of the agentgateway Apply. The
reconciler stores it on its own path, before the Apply and whatever the Apply does, so an agentgateway
outage or a refused config does not freeze key revokes and grants on koinon-gateway. A failed store is
retried in the pass and is then a pass error. The sandbox work of an agent with a key waits until every
live koinon-gateway copy acked the first revision that held that key (the bounded wait of #1260); after
the wait the agent is held with a message, and the next pass looks again. An agent that is not active (a draft
keeps its key), an agent with no key (archived) and an agent whose key the copies already acked never
wait, so stops and teardowns go on and a change for other agents holds nobody. When the list of live
copies cannot be read, the gate fails closed. With no live copy (the agentgateway driver) nothing waits. After
a restart the reconciler does not know which revision first held an old key, so only the keys it issued
since the start wait; the keys of the first stored revision count as served.

Record (2026-11-12, #279 slice 3; the choices below are decided): the stream is served and consumed,
with no cut-over. `internal/gateway/stateserver` serves `GatewayStateService` on the internal listener
that already carries the gateway's bearer token (`internal/otlpauth`, the token the OTLP receiver
checks): no new port, Secret or flag, and agentd's trace token never admits it. The server refuses to
start without an authentication wrapper. One loop reads the newest revision every 5 s and on each
`pg_notify`, so a missed notification (transaction-mode PgBouncer) costs latency and nothing else; every
stream reads that one cached snapshot, and a new stream reads the table once at connect. A stream sends a
revision whenever its number differs from the last one sent, so a restored database that holds a lower
number is followed. `internal/gateway/stateclient` is the copy's side: it applies the model half
(`ModelPathGateway.ApplyModel`) and the MCP half (`MCPGateway.ApplyMCP`) of each revision, acks each half
after that half applied, never acks a half that failed, and reconnects with jittered backoff. A lost ack
(three tries) ends the stream, so the reconnect replays the newest revision and acks it again without
applying it twice. Acks are recorded and gate nothing; the reconciler still pushes state with Apply. Slice
4 moves the gate and must first settle the stale-leader term fence (issue 635, still open for it; the heartbeat bound and prune fence are done). Prior art: Envoy's xDS (versioned
full snapshots, ACK per resource type); here one revision number covers both halves, there is no NACK
(a failed half is simply not acked) and the copy id stands in for the nonce.

Record (2026-11-19, #1086 slice 1; the choices below are decided): the forge and registry routes and the
actor identity token are two small packages with no cut-over yet. `internal/gateway/actortoken` mints and
checks the token: a JWS signed with Ed25519, audience `koinon-gateway`, lifetime 300 to 3600 seconds
(ADR 0120 section 1.2), claims `iss`, `sub`, `aud`, `iat`, `nbf`, `exp`, `jti`, `org`, `agent` and `run`. The
minter holds the private key outside the guest and outside the gateway; the gateway holds public keys only
and accepts no other algorithm. `internal/gateway/route` is the route model: a route names class (forge or
registry), org, one exact host and port, methods, path prefixes, query keys and a credential name, never a
value. The gate refuses by default, has no audit-only mode, refuses the whole set when one route fails to
load, refuses non-canonical paths instead of rewriting them, and refuses one credential on two route
classes, and one credential on two orgs. It serves `https` only, and a platform route (no org) carries no credential. Among matching routes an org route wins, then the longest path prefix. The guest sends the token in the `Koinon-Actor-Token` header, and the gateway removes it before
it forwards. Slice 2 puts the route set into the State stream and serves the routes in the gateway
binary; slice 3 reads the credential from the secret store and drops the OpenShell transition profile.

Record (2026-11-24, #1086 part A; the choices below are decided): the transport of the forge and registry
routes is a path-addressed reverse proxy, `<gateway>/egress/<host>[:port]/<path>`, in the package
`internal/gateway/egress`. There is no CONNECT, no TLS interception and no gateway CA in the guest. The guest
reaches the gateway by name (ADR 0119 P-B), and git `insteadOf` and the npm, pip and Go registry settings
point at that prefix. The upstream leg is `https` only. The upstream is the matched route's host and port,
not the guest's text. It dials through `catalog.URLPolicy.GatewayDialContext`: the dialer resolves each host
once as an absolute name, so the gateway pod's search domains never expand it. It refuses a single label,
localhost and a name in Koinon's namespace by name, and loopback, link-local, metadata and private
addresses by address unless the platform allows them. The namespace rule holds for every cluster DNS
domain. An allowed CIDR (`--egress-allowed-hosts`) admits every route host that resolves into it, an org's
route host included: part B must state this where org admins write routes. The handler runs on a listener of its own (`--egress-addr`, off by default) and never under a
ServeMux, so the gate sees the path the guest sent. The gateway removes the actor token, `Authorization`,
`Cookie` and the proxy and forwarding headers before it forwards, and rewrites an `https` redirect to the
egress prefix; it drops any other redirect target. Logs and the metric
`koinon_gateway_egress_requests_total{class,reason}` carry no token, path, query or ID, and each request
counts one result. The guest sees `no_route` when another org owns the route (`org_mismatch` stays in the
log and the metric), so a guest cannot learn the hosts of other orgs. The gateway removes a refusal
header that the upstream sends. A route that names
a credential is refused (`credential_unavailable`) until part C attaches it. Platform routes (no org, no
credential) load from a JSON file; one bad route refuses the file. The parts: part A serves the routes in
the binary; part B puts the org and agent routes into the State stream, from the forge grants and the
registry levels of ADR 0113; part C attaches the credential from the secret store through the scrubbed
source type, mints the actor token outside the guest for both drivers and drops the OpenShell transition
profile. Prior art: Fly.io's Tokenizer (a proxy that adds the credential, the client holds none), git
`url.<base>.insteadOf`, and npm and pip registry mirrors that take a path prefix.

Record (2026-11-24, #1086 part B; the choices below are decided): the org routes come from the sources
that exist today, the code-host grant of each active agent and its npm plugin-install rule. The registry
levels of ADR 0113 come later, in an issue of their own. The reconciler translates the same enforced
endpoints that it renders into the OpenShell policy, and the OpenShell rules stay in force as a second
layer. The State update gets an add-only egress half (`EgressState`, field 5), which the gateway applies on
each revision with no ack of its own: nothing waits for it. The gateway merges the org routes with the
platform routes file (`egress.Merge`). An org route names its org and has a `/` in its name, and a platform
route has no `/`, so the two sets never share a name. One bad org route refuses all org routes, and the
gateway then serves the platform routes alone. The agent deny routes go with them, so a platform file
must not rely on them: today no platform file names a forge host. The reconciler compiles each agent's routes first, so one
agent's fault takes away only that agent's routes. To translate the connector rules without loss, the route
model gets: an agent scope (empty means every agent of the org; an agent route wins over an org route, and
an org route over a platform route), segment patterns (`*` is one non-empty segment, `**` is zero or more,
other segments match the decoded segment exactly and with case), exact query values, an explicit
any-query flag, and deny routes. A deny route is checked before every allow, without case, and has no
credential. It takes path patterns only, never a prefix, and ignores a trailing slash. A rule that the model cannot express exactly (a query glob, an access preset, an endpoint path,
a protocol other than REST) gives the agent no route. The gateway agent name is the agent ID, as in
`gateway.AgentRef`: the actor token that part C mints must carry the same value in its `agent` claim. A
forge route names the credential `org/<org>/agent/<agent>/codehost`, a name and never a value, so it
returns 503 `credential_unavailable` until part C attaches it. The host of a forge route comes from the
connection's base URL, which an org admin sets: an allowed CIDR (`--egress-allowed-hosts`) admits that host
too when it resolves into the CIDR. Known limit: the route model refuses an encoded slash in every path,
also the npm CLI's spelling of a scoped package (`/@scope%2fname`); that needs a change of its own before
npm traffic goes through the gateway.

Record (2026-11-24, #1086 part C1): the gateway attaches the forge credential. The reconciler mints the
agent's code-host token (read-only, as for the OpenShell provider record) and seals it at once to the
gateway's recipient key (envelope v2, decision 12). The binding is the org ID, the table `agents`, the agent
ID and the column `forge_token`; no column stores the token. The file is `forgev2-<org ID>-<agent ID>-<8 hex
of the scope>-<8 hex of the envelope>` (`internal/forgecredfile`) in the MCP key Secret, which only
koinon-gateway mounts, never in the provider keys. The scope is the connection, the repositories, the
permissions and the forge hosts of the routes that send the token. The reconciler keeps the envelope in
memory while the scope, the token revision and the key ID stay the same, so an unchanged token keeps its
file. A new file waits for the key settle time; in that time the credential keeps its last applied file,
else (after a restart) a file that the Secret still holds, in both cases only a file of the same agent and
scope, else it is left out. So a token is never sent to a host or a connection it was not minted for. The State gets an add-only list `EgressState.credentials`: name,
org, agent, file, scheme and user, never a value. The gateway refuses the whole org half when a credential is
named twice or when a route names a credential of another org or agent. It opens the file on each request
of a credentialed route and checks that the agent ID in the file name is the route's agent. It sends the
value only to the matched route's upstream, as `Authorization: Basic` with the user `x-access-token` (GitHub
and Gitea take a token this way). It asks for an identity body (`Accept-Encoding` removed) and refuses a
response with another encoding (502). It masks the value and its Basic pair in the response headers,
trailers, body (also across reads) and upstream error text, and it adds both to the log scrubber. Every
failure fails closed with 503 `credential_unavailable`, with a fixed reason in the log and the
`koinon_gateway_credential_open_failures_total` metric. The reconciler seals only when koinon-gateway
serves the MCP path (`gateway.mcp.driver=koinon`): a gateway that does not read the list refuses the route
with 503, so no format gate is needed. Part C2 (the actor token outside the guest for both drivers, the git
prefix in the guest, the egress listener in the chart and a kind e2e) and part C3 (drop the OpenShell
transition profile) follow. Prior art: Fly.io's Tokenizer, the MCP credential path of decision 12
(`mcpgateway.StaticCredentials`, the credential scrubber).

Record (2026-11-25, #1086 part C2): the actor token and the egress listener. `gateway.mcp.koinon.egress`
turns on koinon-gateway's egress listener (its own port, default 8081, never the MCP port; it needs
`gateway.mcp.driver=koinon`). The ensure-secrets Job makes one Ed25519 key; koinon-agentd mounts only its
seed and koinon-gateway only its public key, as `<key ID>.pub`. koinon-agentd mints the actor token outside
the guest for both drivers: a compact JWS with the org slug, the agent ID and the run ID, 15 minutes
long, written again every 5 minutes. It writes the token and the git prefix
(`url.<gateway>/egress/<host>/.insteadOf`, with the token in `http.extraHeader` as `Koinon-Actor-Token`)
into the sandbox through the driver's exec, to `/tmp/.koinon-egress`: outside the home, so no checkpoint
holds it, and it removes the file when the run ends. The token is never logged. No forge credential is in
the guest. The reconciler adds an enforced L7 sandbox rule (`koinon_egress`) for the listener: it mirrors
each allow and deny rule of the forge read grant under `/egress/<host[:port]>`, so the guest reaches the
same paths through the gateway as it did directly. `--egress-ca-file` (`gateway.mcp.koinon.egress.caBundle`) adds private
roots for the upstreams of the listener only. The network policies let sandboxes reach the egress port
only. A run of an agent whose grant gives no read access gets 503 `credential_unavailable` from the
gateway: it fails closed. The kind e2e clones from a fake Gitea over HTTPS through the listener.
Prior art: SPIFFE JWT-SVIDs (a short-lived identity, not a credential), git's
`insteadOf` and `http.extraHeader`.

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
7. The GitHub tools server works behind Koinon's MCP path with the Koinon-named agent header, and a
   server an org registered gets no agent header on that path (#1235).
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

Note (maintainer decision 2026-11-08, #374): the agentgateway config helpers duplicated in `internal/gateway/standalone/render.go` and `internal/gateway/kubernetes/render.go`, and the Kubernetes driver importing the standalone package's types, are accepted until step 3 deletes that code. Do not propose refactoring them in a review.

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
  first. Built (#78): Koinon's model path records the split in the ledger; the agentgateway driver keeps
  the gap.
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

### 12. MCP credentials sealed to a gateway key (amendment 2026-11-19, #1151)

Maintainer decision 2026-11-19. Part of step 2 (decision 5, placement). This decision is design only:
the code comes in the slices below. ADR 0091 needs no change: the leak guard does not touch stored
credentials.

**Problem.** Two processes can open a stored MCP credential today, and the gateway is not one of them.
The reconciler holds the KEK and calls `mcpservers.OpenCredential` (`internal/reconciler/mcp.go`). It writes the plaintext into the Secret that also holds the provider keys, so the plaintext is also in etcd. koinon-api mounts
the KEK, and its Role can `get`, `list` and `watch` every Secret in the namespace
(`charts/koinon/templates/rbac.yaml`). Decision 5 says only the gateway may decrypt. This decision makes that true for the static MCP credentials only. Provider keys stay plaintext in that Secret until slice 6.

**Scope.** The static credential of an MCP server (`mcp_servers.credential`). Provider keys and MCP
sign-in tokens (decision 5 names them) use the same mechanism later, in their own issue (slice 6). Their
envelopes stay at v1 until then.

#### 12.1 The scheme: HPKE, from the Go standard library

- **Choice.** HPKE (RFC 9180), base mode, from `crypto/hpke`. The module's Go is 1.27.4 (`go.mod`), and
  the toolchain's standard library has `crypto/hpke` (checked in its source: `NewSender`,
  `NewRecipient`, `Sender.Seal(aad, plaintext)`, `NewKEM`). No new dependency, so ADR 0079 has nothing
  to check.
- **Why not a sealed box.** A NaCl sealed box is not in the standard library. It would use
  `golang.org/x/crypto/nacl/box` (`golang.org/x/crypto` is already in `go.mod`, v0.59.0). It has no
  `info` or `aad` input, so the binding to the row (org, table, row, column) could not be authenticated
  by the box and would need a second layer. It names no algorithm, so changing the KEM later would be
  a new format. HPKE has both: `info` and `aad`, and a KEM ID.
- **Suite.** KEM DHKEM(X25519, HKDF-SHA256) (`0x0020`), KDF HKDF-SHA256, AEAD AES-256-GCM. The
  envelope names the suite, so a later key can use DHKEM(P-256) (FIPS) or a hybrid post-quantum KEM
  (`MLKEM768-X25519`, `0x647a`, in the same package) with no format change. Checked in slice 1 (go1.27.4):
  Go's FIPS 140-only mode refuses X25519 ("use of X25519 is not allowed in FIPS 140-only mode"). The code
  has a second accepted suite, DHKEM(P-256, HKDF-SHA256) (`hpke-0010-0001-0002`), and
  `DefaultRecipientSuite` returns it in that mode. The suite of a key does not change.
- **One DEK per value, as today.** The payload is sealed with AES-256-GCM under a fresh random 32-byte
  DEK, exactly as in v1. The DEK is the HPKE plaintext. The payload code does not change.

#### 12.2 Envelope v2

The stored form is JSON in the same column. `fmt` tells the formats apart. A v1 reader refuses `fmt: 2`
(`ErrUnsupportedFormat`), so an old binary fails closed on a new value. Fail closed is not enough for a rollout: 12.5 gives the order that keeps every component able to read what is written.

```json
{
  "fmt": 2,
  "alg": "AES-256-GCM",
  "recipient": "hpke-0020-0001-0002",
  "recipient_key_id": "gk-1f3a9c2e5b7d4e60",
  "enc": "<base64: HPKE encapsulated key>",
  "sealed_dek": "<base64: HPKE ciphertext of the DEK>",
  "nonce": "<base64: payload nonce>",
  "ciphertext": "<base64: payload under the DEK>"
}
```

- `recipient` is the suite as KEM, KDF and AEAD IDs (hex, RFC 9180 registry). The code has a fixed table
  of suites it accepts, and refuses any other.
- `recipient_key_id` names the recipient's key pair. It is `gk-` and 16 hex characters of the SHA-256 of
  the public key. It matches the pattern of `ValidKEKID`, and a loader refuses a key whose ID is not the
  hash of its public key.
- **Binding.** The payload AAD is the length-prefixed list of purpose, `recipient_key_id`, org, table,
  row and column, with the tag `koinon/secretstore/v2` (the shape of `Binding.aad` in v1). The HPKE
  `info` and the HPKE `aad` are the same list with purpose `dek`. A copy of an envelope into another row,
  column or org, or under another key ID, fails to open.
- **Seal side.** `Seal(recipientPublicKey, binding, plaintext)` needs the public key only. koinon-api
  holds nothing else.
- **No KEK ID.** A v2 envelope names no KEK, so `keys rewrap`, `keys retire` and the "values still use
  KEK" count skip v2 values (slice 3 updates `keyscli.Columns` handling and its test).

#### 12.3 Where the gateway key lives

**What RBAC can do.** A narrower Secret Role for koinon-api is not possible in the release namespace.
RBAC has no deny rule and no label selector. koinon-api must `list` and `watch` Secrets whose names it
cannot know (the labelled declarative Secrets), so it can read any Secret in its namespace. The reconciler has `get` on Secrets by name, so it can read a Secret with a fixed name. A Secret there cannot hide a key from it (the same finding as decision 5, now with the
RBAC text).

**Decision.** The custody boundary is the KMS key that wraps the gateway private key.

- The gateway's private key (32 bytes for X25519) is wrapped by the existing `secretstore.Wrapper`
  (`local`, `openbao`, `awskms`). It is stored in the same form as a wrapped KEK (`StoredKEK`: backend,
  key ID, wrapped bytes, install ID). Install binding and `Probe` are reused. Slice 1 adds a separate AAD with its own purpose tag (`recipientKeyAAD`). `kekAAD` and `kekInstallAAD` do not change (wrapped KEKs are stored). So a wrapped gateway key can never be read as a KEK. The gateway builds its own `Wrappers` value with its own wrapping key, because one `Wrappers` has one primary key. The install binding needs `install_identity` and `kek_bound_only`: the gateway role and the re-seal role get SELECT on both (slice 2).
- The wrapping key is a **separate** KMS key: an OpenBao transit key (default `koinon-gateway`) or an AWS
  KMS key, not the KEK's key. Its policy lets only the gateway's identity decrypt: the gateway's
  Kubernetes auth role in OpenBao, the gateway's IAM role in the AWS key policy. koinon-api and the
  reconciler get no right on it. This is the existing wrapper with its own key ID and its own policy.
  It is not a change to the KEK's key.
- Only a new, separate "gateway key-admin" identity may encrypt with it (create the key pair, rotate).
  This follows the key-admin Job of ADR 0085 decision 12: one identity may encrypt, the users may only
  decrypt.
- The wrapped private key sits in a Secret of the release namespace (`<release>-gateway-recipient-key`),
  mounted into the gateway pods only. koinon-api can read the Secret, and it is ciphertext under a key koinon-api cannot use (on a KMS backend; see the table). The chart's `kek-guard` policy gets a third rule: only the gateway key-admin identity may CREATE, UPDATE or DELETE this Secret. CREATE is named, because koinon-api and the reconciler have `create` on Secrets. If the Secret exists when the Job runs and no `gateway_recipient_keys` row names it, the Job stops and reports it. It never adopts a key it did not make.
- **The public keys** are not secret. They are rows of a new table `gateway_recipient_keys` (key ID,
  KEM, public key, state `active`, `retiring` or `retired`, times). koinon-api has SELECT only. The
  gateway key-admin role has INSERT and the state update. Triggers refuse any other UPDATE and any
  DELETE (the pattern of `kek_bound_only`, migration 00049). The migration must revoke INSERT, UPDATE and DELETE on the table from the shared application role (`koinon_app` gets table rights by default privileges in cnpg mode). Triggers stop mistakes, not the table owner. With separate roles, an attacker with koinon-api's database login cannot make koinon-api seal to a key the attacker knows. With external mode and one shared role (`values.yaml` allows it), this protection does not hold. The chart documents that limit, and the docs recommend separate roles. At load, the gateway derives the public
  key from its private key and refuses a key whose row differs (fail closed, a run event, a metric).
- **Pods and identity.** The gateway keeps no Kubernetes API access (rbac.yaml says so today) and reads the Secret as a mounted file. To sign in to the KMS it needs an identity token: a projected ServiceAccount token with its own audience, which the OpenBao role binds, or the IRSA webhook mount for AWS. It is mounted as a projected volume, not as an API token. Slice 2 adds it. The gateway key-admin Job needs the Kubernetes API to write the Secret, as the key-admin Job does today.

**What each backend gives.**

| KEK backend | koinon-api can open a stored MCP credential? | Why |
| --- | --- | --- |
| `openbao`, `awskms` | No | The wrapped key is useless without the gateway's decrypt right on the separate KMS key. |
| `local` | Yes, and so can the reconciler, if the Secret is in the release namespace | `Local` wraps with the identity function: the "wrapped" key is the key. No KMS holds the boundary. |

On `local`, the Secret has a fixed name, so koinon-api (list) and the reconciler (get) can read the raw key. For `local`, v2 still removes the plaintext from the reconciler, from the MCP credentials Secret and
from etcd. It does not give custody. The chart and the install status say so: the NOTES text and the metric
`koinon_gateway_recipient_key_custody{custody="kms|none"}` (fixed labels, ADR 0023) show it, and the
docs say "use a KMS backend for the gateway-only property". This is one code path in every backend; only
the strength of the boundary differs, and the chart reports it.

**Decided (maintainer, 2026-11-19, #1151).** There is one code path on every backend. The `local` backend
still accepts MCP credentials, with no second namespace. Koinon reports the custody level of each install
instead of hiding it: on `local` the report says "koinon-api can read the wrapping key"; on AWS KMS and
OpenBao it says "gateway-only unwrap". The report is in the install status and in the docs. No text
claims "koinon-api cannot decrypt" on the `local` backend.

#### 12.4 Key ID, rotation and re-seal

- A key pair is made by the gateway key-admin identity. It is a Job, like the key-admin Job of ADR 0085 decision 12. That Job renders only with a KMS, so the gateway key Job is a new template that renders on every backend, `local` included. On `local` it writes the key unwrapped (`Local` wraps with the identity function): it generates the pair, wraps the private key, stores the Secret, inserts the public row as
  `active`. It refuses to replace a key that rows still use. It runs once at install, before any v2
  value exists.
- koinon-api reads the newest `active` public key at start and on a poll loop (the KEK loop is `Keyring.WatchDir`; this one reads the table). With no `active` row it seals v1, as today. A database check backs the loop: a trigger refuses a v2 credential whose `recipient_key_id` is not `active` or `retiring`, so a stale koinon-api pod can never write a value nobody can open. The rolling-upgrade order is in 12.5.
- **Rotation.** (1) The key-admin identity adds a new pair as `pending`. The gateway pods load the key (the kubelet needs up to about a minute to update the mounted Secret) and report it loaded. Only then the key becomes `active` and the old one `retiring`. This is the settle step the reconciler already has for credential files, and it stops a 503 window. (2) koinon-api seals new values to the new key. (3) A re-seal Job runs as the gateway
  identity (it needs the private key), with its own database login whose role has UPDATE on the
  `credential` column only (a column grant). The gateway's normal role has SELECT and no UPDATE. For each v2 value under a `retiring` key, it opens
  the value, seals it to the active key, and writes it with a compare-and-swap on the old bytes, so a
  write by koinon-api in between is not lost. (4) When no row names the old key (`keys verify`), the old key becomes `retired`, after a wait longer than the reload interval of koinon-api, and the key-admin identity deletes its wrapped private key. The trigger above is the last guard. Rotation
  re-seals; it never rewraps a DEK, because a DEK can be read only with the private key.
- The gateway holds every `active` and `retiring` private key, and picks the key by `recipient_key_id`.
  An unknown key ID fails closed (12.6).
- **KEK rotation and gateway key rotation are independent.** A KEK rotation does not touch v2 values.
  A gateway key rotation does not touch v1 values.

#### 12.5 The one-time move of stored credentials to v2

The gateway cannot open v1: it has no KEK, by design. A job that holds the KEK does the
move, once.

0. **Gate on the driver.** The MCP data plane is chosen by `gateway.mcp.driver`. Since #1239 the default is `koinon`; before it, the default was agentgateway. agentgateway stays the fallback, and it reads the credential file as plaintext (standalone `backendKey`, and the Secret reference in the frozen Kubernetes mode). So koinon-api seals v2, and the move runs, only when every MCP path of the install is `koinon-gateway`. A chart value derives the gate, and `keys seal-mcp` refuses to run otherwise. Until then v1 and the reconciler's plaintext files serve agentgateway, and the old open path stays. Slice 5 waits for the gate too.
1. **Expand (slice 2 and 3).** Release N adds `gateway_recipient_keys`, the key-admin Job, the writers
   that seal v2 (only when an `active` row exists), the reconciler's copy path for v2, and the gateway's v2 reader. **Release N reads v2 and never writes it.** Writing starts in release N+1, or when an explicit gate says every component is at N or later. The key-admin Job runs while old pods still run, so the active row alone is not that gate. The v1 `keyscli` usage query scans `kek_id` of every envelope into a string, and a v2 row gives NULL. Slice 3 fixes that query, so `keys status` and the key-admin add path work in release N before any v2 value exists. After the first v2 write, a rollback to N-1 leaves v2 credentials unservable. The rollback rule needs the reverse move `keys seal-mcp --to-v1`, which needs the private gateway key. Slice 4 builds the engine only: no command runs it yet (see 12.8 item 2), so a rollback to N-1 after the first v2 write is not supported until a gateway-side command exists. Track it in a follow-up issue. The reconciler still opens v1 and still writes plaintext files for v1 values, and the gateway
   still reads those files (marked by the file name: `mcp-…` plaintext, `mcpv2-…` ciphertext). Nothing
   stored changes yet.

   Known limits of slice 3, recorded so a later slice can close them:
   - The write gate is generic and lives in `internal/formatgate`. Each component reports the formats it reads
     to `component_reports` (migration 00102; koinon-api, the reconciler and koinon-gateway, each pod once
     per interval). koinon-api seals a v2 credential only when `Gate.Ready` finds a fresh report from every
     pod the chart expects (`KOINON_COMPAT_EXPECT`, built from the replica counts) and no fresh report
     that lacks the format. It fails closed: a missing report, a pod at N-1 and an unknown component all
     give `ErrNotReady`, and the credential is then written as v1. A later format change registers by one
     `Format` value in `formatgate.Formats` with its reader components, and its writer calls `Gate.Ready`.
     `KOINON_GATEWAY_MCP_SEAL_V2` stays as an operator override. It can only close the gate. The gate does not
     move sealing to N+1. A pod from before the reporter writes no row, so only the replica count shows it:
     the gate stays closed until the expected number of pods report. In a surge rollout the new pods can
     report before the last old pod has stopped, so koinon-api also requires the gate to stay open for two
     minutes without a break (`formatgate.DefaultHold`, longer than a pod's drain and a report's TTL) before
     it seals. A rollback leaves fresh N rows for up to the TTL; the same hold covers it.
   - Before #1239, agentgateway still served the agents' MCP path while the driver was koinon, and it cannot open
     a sealed credential. The standalone renderer leaves such a target out of agentgateway's config, so no
     envelope goes upstream. #1239 moves the agents' MCP calls (koinon-agentd's `KOINON_AGENT_MCP_URL`, and the
     sandbox profile's `/mcp` rules on a second endpoint, koinon-gateway's Service) and koinon-api's live tool
     check (`KOINON_GATEWAY_MCP_CHECK_URL`) to koinon-gateway. It also moves the reconciler's MCP prober
     (`internal/reconciler/mcpprobe.go`, the `gateway.mcpUrl` setting): through agentgateway, which has no target
     for a server with a sealed credential, the probe would find no tools, mark the server unhealthy, and the
     plan would then give no agent a grant to it. With the agentgateway driver the prober stays on agentgateway.
   - The reconciler serves a sealed credential only when its `gateway.mcpSealed` setting is on (the chart sets
     it with the koinon driver). Without it the server is left out and the log says why, so a move back to
     agentgateway never sends an envelope upstream. `keys seal-mcp --to-v1` (slice 4) is the way back.
   - The trace router no longer scrubs v2 MCP credential values: it cannot read v2 and the reconciler holds no key.
     koinon-gateway holds the plaintext, so it scrubs the values it opened (#1174). `mcpgateway.CredentialScrub`
     records each value when the gateway opens it, and removes it by exact value (and its encoded forms, and the
     token after the auth scheme) from every log line, span and audit row the gateway writes and from every tool
     list and tool result it gives to an agent. Built-in targets open their credentials through the same scrub.
     - **Scope.** An org chooses its own credential bytes. So each text that belongs to one org uses only that org's
       values: the tool lists and tool results an agent gets, and the log lines, spans and audit rows of an agent's
       request (the org travels in the request context). One org's credential cannot change or hide the text of
       another org. A log line or span with no org is process telemetry: it uses the values of all orgs, in its
       values and free text only, never in its keys or group names, which are code constants.
     - **Where.** A tool list and a tool result are scrubbed as they enter the gateway, so the tool-list check, the
       pin hash and the agent see one text, and again as they leave. The guard's inspection of the wire still sees
       the raw message: it must, to judge it.
     - **Limits.** A value stays 24 hours after its last open. One org holds at most 256 values and the process
       8192; over a cap the oldest go first. A value that is already known costs no lock. A value shorter than 8
       bytes is not matched (it would remove ordinary words). So koinon-api refuses to save a credential value, or
       the token after an auth scheme word in it, shorter than 8 bytes (#1221): Connect gives the ErrorInfo reason
       `CREDENTIAL_TOO_SHORT`, REST gives 400 `invalid_request`. A value saved before that check can still be
       opened: the gateway counts it (`koinon_gateway_credential_too_short_total`) and logs the target once. A
       declared Secret that holds such a value is refused on each sync, and its server keeps the old value; its
       `disabled: true` still applies when the short value is its only fault (#1221). During a restore hold, the
       plan reports such a Secret as refused only; the disable applies at the release.
       `koinon_gateway_credential_redactions_total` counts the texts it changed, by path (`agent` or `telemetry`). The open that goes over a cap never loses its
       own values to that cap; an open at the same time can (see Batching).
       A value that a cap removes before 24 hours is no longer scrubbed: `koinon_gateway_credential_evictions_total`
       counts these, so an operator sees a cap that is too low for the credentials in use.
     - **Threat scope.** The scrub is a backstop for a target that echoes its credential by accident. It does not
       stop a target that hides the value on purpose (hex, a hash, a split over two fields): that target already
       holds the credential. One residual stays in one org: an agent can save a credential equal to a guessed text and see if
       its org's audit row of a call changes. Only a reader of that org's audit can see this. Each call of a built-in
       target gets the gateway's recording source as an argument of `Builtin.Call`. Its type is `ScrubbedSource`,
       which only `CredentialScrub.Source` can make (its field is not exported). The outbound clients
       (`NewTargetHTTPClient` and the A2A client, also for the card read in koinon-api) take this type and no
       other, so code that sends a credential the scrub did not record through them does not compile; a test builds such code and
       expects each error. The zero value has no source: a read fails, and the outbound clients refuse it. The type
       proves that a scrub recorded the value, not which scrub. Two gaps stay, so each built-in kind keeps a test
       that the gateway records the value it opens, and a built-in that uses another source fails it (#1221). A
       built-in can make a scrub of its own (`NewCredentialScrub`) and send through it, and it can read a source of
       its own and quote the value or send it with an HTTP client of its own. The card read in koinon-api has no gateway scrub: it uses a scrub
       for that read only. That value is the one the admin saves, and the read does not log it.
     - **Kinds** (maintainer decision 2026-11-25, #1221). The scrub covers every credential kind the gateway sends
       upstream, the per-agent kinds too. `internal/gateway/credkind` is the one list: each kind is a value of
       type `credkind.Kind` with one rule. `Kind` is a struct with an unexported field, so only that package can
       make a kind. Each opener gives its values to `CredentialScrub.Record` with its kind:
       the MCP path through `ScrubbedSource` (`Credential.Kind`), the egress path (ADR 0120) and the model path
       (provider keys). The rules:
       - **By value**: MCP static, MCP sign-in (ADR 0096), egress bearer and Basic, provider key. The value goes to
         the value set of its org, as above. A sign-in token keeps the fixed 24 hours after its last open, not its
         expiry: a longer life only over-scrubs, and one value per server and refresh stays well under the caps.
       - **By shape**: the passport (ADR 0073). One passport for each run, audience and scope, with a life of
         minutes, would fill the caps and push out the values of the org. So the scrub does not store it. From the
         first passport it opens, the gateway removes every JWT with the typ `koinon-passport+jwt` from every
         text, in every org, also a passport that a tool returns on purpose. A JWT of another typ stays. A passport
         in a form the shape does not match (base64 of the whole token, a split) is not removed: the threat scope
         above applies.
       - **No value**: `none` and `error` carry no value.
       A kind that is not in the list, or no kind, is scrubbed by value, and a value given with a by-shape kind
       that does not have the shape, or with a no-value kind, goes to the value set too: a new kind is covered by
       default. A spec reads the kinds from the source of the package and fails when one has no rule.
       The scrub covers the texts it sees: MCP results, errors, log lines and spans, and the egress responses (ADR
       0120). The model path records provider keys for its logs and spans, but it does not scrub the response body
       that goes back to the agent. An upstream that echoes the key in an error body reaches the agent unchanged.
       Section 15 makes this an accepted limit of v0.2 and adds a detection and an alert.
     - **Batching.** A removal (cap or age) does not rebuild the matcher of all orgs at once. A removed value stays
       in it until the next rebuild. That rebuild runs at the next change at least one second later, and with traffic
       at most one minute later. Until then the value can only over-scrub a process record. A removal marks a value
       as removed with a compare-and-swap on the last-open time that it read. An open that moves that time forward
       before the swap keeps the value. An open whose time is not later than the stored time (the same clock tick,
       or a clock that is late) does not change the time, so a cap that read that time can still remove the value.
       This is the normal race at a cap: `koinon_gateway_credential_evictions_total` counts it, and the next open of
       the value records it again (#1221).
     The router path logs and stores no opened v2 value (a spec checks it). The router still scrubs v1 MCP values,
     which it can open.
   - `koinon_gateway_credential_open_failures_total` counts failed opens. One refused call can open a
     credential more than once, so the counter is not a count of refused calls.
2. **Move (slice 4, built in #1151).** `koinon-api keys seal-mcp` opens each v1 value with the KEK and seals it
   to the `active` public key. Plaintext exists only in that process; it is never written, logged or sent.
   The write is a compare-and-swap on the stored value: a row that changed meanwhile is skipped, and the
   next run takes it. The command is idempotent (it skips v2 values), resumable, and takes `--org` and
   `--dry-run`. A dry run opens each value and writes nothing. The command follows the write gate of
   12.5 step 1: it refuses, and changes nothing, while any expected pod cannot read v2. One code path serves
   every backend, because koinon-api holds only the public key. It writes one audit row per run with
   counts and key IDs, and no value. The report names an org and a row ID for a failure, never a value.
   `keys verify` and `keys status` print the number of v1 and v2 MCP credentials (v2 per key ID). The
   upgrade preflight (the migration Job, `--require-mcp-v2`) refuses while a v1 credential remains, and
   says which command to run. The release that has no v1 reader switches it on by default (slice 5).
   - **`--to-v1`.** The reverse move needs the private gateway key, so only a process that holds the
     keyring can run it. The `koinon-api` CLI has none, so there `--to-v1` fails closed and says so.
     This is a change from the first text of this decision, which put it under the KEK identity: the KEK
     cannot open v2. The engine takes the keyring as an injected hook. A gateway-side command can use it.
   - **Custody.** On the `local` backend koinon-api can read the wrapping key, so nothing may claim it
     cannot decrypt. Only on OpenBao or AWS KMS can only the gateway unwrap.
   - **Rotation and re-seal.** A new key is `pending`, then `active`. A credential sealed to the old key
     stays readable. `koinon-gateway keys reseal` (the chart's re-seal Job, spec 6) opens each v2 value that
     names an older key with that key and seals it to the active key, with the same compare-and-swap
     write. It runs as the gateway identity, with the `koinon_gateway_reseal` login, and makes no load
     report. The old key retires only when no v2 credential names it.
3. **Contract (slice 5).** Delete `OpenCredential` from the reconciler's path, the plaintext files and
   the gateway's legacy reader, and the reconciler's code that opens MCP credentials. The reconciler
   keeps its KEK for the other stored secrets (not in this scope).

**Patch-line rules** (ADR 0085 decisions 5 to 8, ADR 0099; `hack/migrate-patch-line`). The marker is
`none` until v0.3.0 ships, so no minor line is promised yet. Slices 2 to 5 must ship before the marker
names v0.3. If slice 5 slips past that, then within v0.3 the release stays expand-only: the v1 reader, the
plaintext files and the legacy gateway reader stay until v0.4, and the preflight rule above (v1 count
must be zero to leave a release that has the reader) stays too. Old backups hold v1 values that the KEK still opens. After the move, operators should rotate the upstream MCP credentials to get the full property. The new table and `mcp_call_log.recipient_key_id` are add-only migrations,
and `credential` keeps its column and type (both formats fit one `jsonb`), so no migration renames or
drops anything.

#### 12.6 Audit and failure modes

**Audit** (ADR 0100; no value, no plaintext, no ciphertext in any row or log):

- `gateway_key.created`, `gateway_key.rotated`, `gateway_key.retired`: key ID, KEM, backend, the actor.
- `mcp_credential.sealed_v2` batch rows from the move and the re-seal: counts, old and new key ID.
- Each `mcp_call_log` row already records the credential kind (`credential_kind`, migration 00096). Slice 3 adds a `recipient_key_id` column (add-only) for the key of the envelope it opened.
- Metrics with fixed labels (ADR 0023): `koinon_gateway_recipient_key_loaded{state}`,
  `koinon_gateway_credential_open_failures_total{reason}` (`unknown_key`, `open`, `format`, `kms`), and the custody gauge above.
  `open` covers a wrong key, a changed byte and another row's binding: the AEAD cannot tell them apart, and a
  split would suggest it can (slice 3 chose these names).

**Failure modes.** The rule everywhere is fail closed.

| Case | Result |
| --- | --- |
| Envelope names a key ID the gateway does not hold | The target is not served: the call fails with a tool error (`credential_kind` `error`, outcome `upstream_error`, and the key ID in `recipient_key_id`), no request is sent upstream, never a call without its credential. Other targets keep working. |
| Wrong key, changed bytes, or another row's binding | The same (`ErrOpen`). No retry with another key. |
| Unknown `fmt`, suite or KEM | The same (`ErrUnsupportedFormat`). |
| KMS unreachable at start | The pod is not ready (MCP path on), so a rollout stops. The wrapper errors wrap `ErrUnreachable`, so an outage is not shown as a wrong key. |
| KMS unreachable later | Loaded keys stay in memory. `Probe` runs on each reload, as for the KEK (`internal/kekhealth`). |
| Public row differs from the key the gateway holds | The key is not loaded as `active`. A run event and a metric. |
| koinon-api seals to a stale `retiring` key | Opens, because the gateway holds `retiring` keys until the re-seal is done. |
| Old reconciler or api (v1 only) reads a v2 value | `ErrUnsupportedFormat`: refused, never mis-read. |

**What this does not change.** A compromised gateway reads every MCP credential: it must use them. A
compromised koinon-api can still replace a credential (write) but cannot read one, on a KMS backend.
The gateway still has no KEK.

#### 12.7 Specs the code must have

1. koinon-api cannot decrypt a stored credential: with the keyring and the wrapper koinon-api has (no
   gateway key right), `Open` of a v2 value fails, on the OpenBao and AWS fakes of `secretstoretest`. A `local` test with a split key set is artificial and does not prove the production property: on `local` koinon-api can read the key (12.3). A chart test checks koinon-api and the reconciler
   have no right on the gateway wrapping key (OpenBao policy text, AWS key policy example).
2. The reconciler handles only ciphertext: a spec gives it a keyring with no gateway key and checks that
   the bytes in the MCP credentials Secret equal the database bytes and contain no plaintext (also in the
   Secret's `data` and in its logs). An import test fails if the reconciler package reaches the opener.
3. The gateway decrypts: seal with the public key only, open with the gateway wrapper, and the header it
   builds has the credential.
4. A wrong key fails closed: unknown key ID, another key's pair, a changed `enc`, `sealed_dek`, `nonce` or
   `ciphertext`, a binding of another row, org, table or column, an unknown suite, `fmt: 1` where `fmt: 2`
   is required. Each gives 503 for the target and no upstream request.
5. Golden files: `testdata/envelope_v2.json` opens (the RFC 9180 vectors in the Go source tree (`crypto/hpke/testdata`), reused for the HPKE part); v1 golden files
   still open.
6. Rotation: a value sealed to the old key opens during the re-seal, and after it is under the new key; a
   concurrent write by koinon-api is not lost.
7. The move: v1 becomes v2 once, a second run does nothing; `keys verify` reports zero; the preflight
   refuses an upgrade while the count is above zero.
8. Column grants: the gateway database role cannot read a column it does not need (the column grants of #278 are merged, and this spec keeps them), and cannot UPDATE any column; the re-seal role has SELECT on `org_id`, `id` and `credential` and UPDATE on `credential`, nothing else, which is what the compare-and-swap needs; the
   koinon-api role cannot INSERT or UPDATE `gateway_recipient_keys`.

#### 12.8 Slices

Each slice is one lane and one pull request, in this order. Sizes are guesses (decision 5's rule: past
twice the guess, go back to the maintainer).

1. **`internal/secretstore` v2.** Envelope v2, HPKE seal and open, key ID, the purpose tag for wrapped
   gateway keys, golden file and fuzz. No caller. About 400 lines with tests.
2. **Key lifecycle.** Migration `gateway_recipient_keys` with grants (revoke on the shared role), triggers and the new database logins (gateway key-admin, re-seal) in the chart, cnpg roles and external-mode docs, the `koinon-gateway`
   key commands (init, rotate, retire), the gateway key-admin identity and Job, chart values, the
   Secret, the `kek-guard` rule, OpenBao and AWS policy docs, the KMS identity token mount, the load and probe in the gateway, the `pending` to `active` report, the
   public-key load in koinon-api, metrics. Specs 1 (chart part) and 8.
3. **Seal v2, copy ciphertext, open in the gateway** (done in #1151 slice 3; a v2 file is named `mcpv2-<org ID>-<server ID>-<8 hex>`, because the binding names the org and the gateway has only its slug; the trace router skips v2 values; the chart sets `KOINON_GATEWAY_MCP_SEAL_V2` only with `gateway.mcp.driver=koinon`). `mcpservers` seals v2 when a key is `active`,
   the reconciler copies `mcpv2-` files, the gateway opens them and fails closed (its `keyPath` and the planner accept `mcpv2-` names), the `keyscli` usage query and `AnyEnvelope` skip v2, `mcp_call_log.recipient_key_id`, and the driver gate of 12.5 step 0.
   Specs 1 to 5.
4. **The move** (code done in #1151 slice 4; the kind e2e with `local` runs from the main session). `keys seal-mcp` (the `--to-v1` engine, with no runnable command yet), `keys verify` count, the upgrade preflight, the re-seal Job.
   Spec 7. Rotation and re-seal Job (spec 6) land here too, because they share the compare-and-swap code.
5. **Contract.** Remove the reconciler's open path, the plaintext files, the legacy gateway reader; docs
   and the GLOSSARY; mark this decision "done". Before the marker names v0.3.
6. **Follow-up issue, not in #1151:** provider keys and MCP sign-in tokens on the same scheme.

### 13. Tool lists are pinned to the approved hash (amendment 2026-11-19, #1132)

The text of a tool (its name, title, description, annotations and schemas) reaches the model as
instructions. An upstream server can change this text at any time. The gateway therefore serves a tool
list only when it matches what an admin approved.

- `internal/mcptoolpin` defines the text and its hash. `Normalise` cleans each member with the
  sanitiser of ADR 0057, and the hash covers the canonical JSON of the result. The sync and the gateway
  both use it, so they cannot disagree about what a change is.
- The sync (the prober) stores the observed text and its hash on the `mcp_tools` row. An admin decision
  (a tier change or an acknowledgement) stores them again as the approved text and hash. A tool that was
  reviewed before the pin existed is adopted at the next sync.
- A tool whose observed hash differs from the approved hash is a change. The sync sets the review flag
  and demotes an `auto` tool, as for a changed description.
- At each `tools/list` (and each `notifications/tools/list_changed`, which runs the same check) the
  gateway asks the guard (`ToolListChecker`). The guard returns the tools to serve and the names to
  leave out. A tool whose hash equals the approved hash is served from the approved text. A changed tool
  is left out of the list, and a call to it is refused without a call to the target. A tool that no admin
  has approved yet is served as sanitised text, and a server that declares the untrusted-input leg sets
  that leg. A list the guard cannot read is withheld, and both legs are set.
- The gateway cuts a list over 256 tools. The tools past the limit are left out, not passed unchecked.
  No path serves an unchecked list: a guard that is not loaded, that panics or that refuses withholds
  the list.
- The left-out set is kept in memory in each gateway replica. The database review flag that the sync
  sets backs the call refusal. The metric `koinon_gateway_mcp_tools_left_out_total` has the label
  `reason` only (ADR 0023).
- When a list carries text for a tool that is neither the approved text nor the text the sync recorded
  last, the gateway wakes the prober (#1173). It sends a Postgres `NOTIFY` on `koinon_mcp_probe` with
  the org and server IDs only, at most once per server in 10 seconds per replica. The leading reconciler
  listens on that channel and syncs that one server at once, at most once per server in 5 seconds. A
  wake inside that time is held to its end, not dropped. The review flag and the new text therefore reach
  the console in seconds, not at the next probe interval. The wake carries no text: the prober reads the
  text through the gateway itself.
- An admin decision is a compare-and-set (#1173). The console shows every field the hash covers (name,
  title, description, annotations, input and output schema) and the hash, and the tier or
  acknowledgement request carries that hash. The service locks the row and refuses the decision with
  `Aborted` (HTTP 409) when the observed hash changed since the admin read it, and with
  `InvalidArgument` when a tool waits for an approval and the request carries no hash. After a refusal
  the console reads the tools again and shows the new text. So an admin never approves text they did
  not see.

This completes decision 4 of ADR 0091 for tool lists. It does not change the tiers: an approved text is
not an `auto` grant.

### 14. Availability and limits (amendment 2026-11-25, #1347)

A review of koinon-gateway before v0.1.0 found gaps that cut or stop agent tool calls in production. Each rule
below has a test. Slice 1 holds these rules; the other gaps of #1347 follow in later slices of the same issue.

- **A database error does not make the pod unready.** After one good pass, the key loader stays ready on its
  last good keyring when the list of key rows or the load report fails (a Postgres restart, a CNPG
  switchover). The pass still returns the error, and the loader logs it. Before the first good pass, and for a
  missing key or a missing migration, the loader stays not ready. A cause that comes from the keys themselves
  is never transient.
- **`/readyz` needs the guard's database.** The pod is ready only after the guard reached its database once
  (the ping and the grant check of `GatewayGuard.Load`). The guard opens a lazy pool and retries every 2 s, so a
  broken login showed as a ready pod with every org target closed, and a rollout did not stop. A later outage
  is not checked in `/readyz`: the guard refuses on its own then (fail closed, section 2 of ADR 0120), and a
  database blip must not remove every endpoint.
- **The drain covers a call in flight.** A tool call is never retried. One request has two stages, each bounded
  by the call timeout (`mcpgateway.DefaultCallTimeout`, 2 minutes): the guard check, which may wait for a
  person's approval, and the upstream call. The rule is one number: `--drain-timeout` is at least twice the
  call timeout (`mcpgateway.MinDrain`, 4 minutes) when the MCP path is on. The process refuses a smaller value
  at start, and the chart refuses `gateway.mcp.koinon.drainTimeoutSeconds` below 240. The default is 270 s,
  so the pod grace period is 305 s with the default shutdown delay. The grace period is only a ceiling: the
  pod exits as soon as its calls end. A later change may end the approval waits at drain (a refusal that the
  agent retries; nothing went upstream), and then the rule could fall to one call timeout.
- **One bound on each upstream reply.** The Go SDK client reads a JSON reply with no limit, and it bounds one
  SSE event only (16 MiB). The pod limit is shared by all orgs, so one org's server could make both copies run
  out of memory. The gateway wraps each upstream client: a reply of type `application/json` may carry at most
  `Options.MaxResponseBytes` (default 4 MiB, `DefaultMaxResponseBytes`) in total, and one SSE event is bounded
  by the same value. A reply over the bound fails its call and logs one warning with the target name and the
  limit. The log line has no argument and no result. The bound is per reply, not per pod: the gateway has no
  cap on concurrent calls or on bytes in flight yet, so many large replies at once can still exceed the pod
  limit. A per-org concurrency cap or a budget of bytes in flight is open work.
- **v0.1 sends no progress notifications, so a call has one time limit.** The gateway answers each request with
  one JSON reply (`JSONResponse`). It sends nothing while the upstream call runs, and a tool call that runs
  longer than the call timeout (2 minutes) fails. A client that resets its timeout on progress gets no help
  from the gateway. The harness config therefore sets the OpenCode request timeout of the `koinon` MCP server
  (`timeout`, in ms, in `harnesses/opencode/etc/koinon/opencode/opencode.jsonc`) to 270 s, at or above
  `MinDrain` (4 minutes), because one call has two stages (approval wait and upstream call). A client that
  gives up earlier would leave the write running while the agent sees an error. The OpenCode default is 60 s,
  so without this value the client gives up first. A spec in `internal/mcpgateway` reads the config and checks
  the value against `MinDrain`, so the two cannot drift. The later fix
  is to forward upstream progress notifications on an SSE reply. Build it when an org MCP server needs calls
  longer than the call timeout. It changes the reply type and the response bound, so it needs its own slice.
- **A copy that cannot apply a State revision turns unready, and the prober keeps its interval.** A copy whose
  `ApplyMCP` keeps failing stays live on its old revision, and it still accepts keys revoked since. The State
  client (`stateclient.Client.Stale`) reports a half that has failed to apply for `DefaultStaleAfter` (2
  minutes), and `/readyz` returns 503 for that time. The copy keeps serving what it has; the rollout and the
  Service stop trusting it. The cost: a revision that every copy refuses makes every copy unready after 2
  minutes, so a bad config can remove all endpoints. This is accepted, because the other choice is to serve
  revoked keys for as long as the config stays bad, and the failure shows in the log and in
  `koinon_gateway_state_revisions_applied_total{half="mcp",result="failed"}`. On the reconciler side, the first probe of a new server
  waits for the acks in its own goroutine, not in the prober loop, so a copy that never acks (bounded by
  `MCPAckWait`, 1 minute) no longer stops the interval probes and the other wakes. An interval probe held back by
  a missing ack logs a warning and counts in `koinon_reconciler_mcp_probes_skipped_total` (no labels).
- **Accepted for v0.1: no `list_changed` and a tool list that can be partial.** The gateway is stateless
  (`JSONResponse`, no session): it holds no stream to send `notifications/tools/list_changed` on. A tool that an
  admin approves during an agent session shows in the next session. This fits v0.1: an agent session starts,
  lists its tools once, and ends; no harness of v0.1 re-lists tools during a session. `tools/list` skips a target
  that fails or that the guard holds, and logs one warning with the target name (`tools/list: target skipped`).
  The agent sees a shorter list, not an error, which is the fail-open choice for a list (a call to a missing
  tool still fails closed). The later fix needs a stream: a stateful mode with SSE, together with the progress
  forwarding above. Build both when an org needs tools that change within one session.

### 15. A provider key echoed in a model response (amendment 2026-11-26, #1332)

An upstream provider can echo its provider key in a response body, most often in an error body ("invalid key:
..."). On the model path, that body goes to the agent. Section 12 lists this limit. The maintainer decided on
2026-11-26: accept the risk for v0.2, detect the echo and alert on it. There is no scrubber in v0.2.

- **Where.** koinon-gateway checks the response on the model path (`gateway.model.driver=koinon`). It checks
  the header values of each upstream answer of status 200 or more (2xx and error) and wraps its body before the
  usage tap. A 1xx answer is not wrapped. The agentgateway driver does not
  check it: its log and tracer fields never read `response.body` or `bodyPrefix`, because that makes the gateway
  buffer every response (section 4). Step 3 deletes that driver.
- **How.** The check streams with the body. It looks for the exact bytes of the provider key that the gateway
  sent upstream for that request. It keeps only the last `len(key) - 1` bytes of the body, so it finds a key that
  is split across two reads, and its memory is bounded by the key length. It never holds back, changes or
  reorders a byte: the agent gets the same bytes, at the same time. A key shorter than 16 bytes is not checked,
  because it would match ordinary text.
- **Signal.** At the first match in a response (a header value or the body), the gateway counts
  `koinon_gateway_model_key_echo_total{answer="success"|"error"}` once (a fixed label set, ADR 0023; `success`
  is a 2xx answer) and logs one warning, `provider key echoed in a model response`, with the provider name, the
  status and where (`header` or `body`). The log line never has the key, the body or a part of them. The chart alert
  `KoinonModelKeyEcho` (severity critical, group `koinon-gateway`, rendered with the koinon model driver) fires
  when the counter rises in one hour, so it stays on for an hour after one echo. The chart's ServiceMonitor
  scrapes the `stats` port of the model-gateway Service for this counter. The operator rotates the provider key.
- **Known limits.** The check finds the exact bytes only. It does not find a key that the upstream encodes
  (JSON `\uXXXX` escapes, base64, URL encoding), cuts, or changes. On an SSE answer, a model that streams the key
  in several text deltas puts JSON between its parts, so the bytes are not contiguous and the check does not find
  it; an error body, the usual case, comes in one piece. The Go transport decodes a gzip body before the check;
  an upstream that sends a `Content-Encoding` without a request for it is not decoded and not checked. An echo
  before the first scrape of a new pod is not seen by `increase`, because the first sample is already 1; the
  log line still shows it. A detection comes after the bytes went to the agent: the alert starts a rotation, it
  does not stop the leak.
- **Later.** A scrubber that removes the key from the stream can reuse the same matcher: it holds back the
  `len(key) - 1` tail bytes in place of only reading them. It needs its own decision, because it changes the
  bytes that a client sees (lengths, SSE event bounds) and the time at which it sees them.

### 16. Kubernetes mode is not supported with the Substrate driver in v0.1.0 (amendment 2026-11-27, #1489)

On Agent Substrate, atenet-egress adds an actor JWT to every model and MCP call from the guest. Only
koinon-gateway checks that JWT. agentgateway (`gateway.mode=kubernetes`) does not check it. A gateway key in the
guest would replace the JWT, but a credential in a sandbox is forbidden (ADR 0120, no secrets in sandboxes). The
maintainer decided on 2026-11-27:

- **Not supported.** `gateway.mode=kubernetes` (agentgateway as the model gateway) is not supported with
  `execution.driver=substrate` in v0.1.0. The line in section 4, "keeps passing the suite in v0.1", does not hold
  for this pair.
- **In the chart.** With `execution.driver=substrate` and no `gateway.model.driver`, the chart uses `koinon`. An
  explicit `gateway.model.driver=agentgateway`, an agentgateway MCP driver or `gateway.mode=kubernetes` with
  Substrate does not render, and the error names the value to set.
- **Later.** The kubernetes mode does not return with Substrate: section 4 freezes it and step 3 deletes it with
  agentgateway. An agentgateway model or MCP driver with Substrate can return only if agentgateway checks the actor
  token. That needs its own decision, and step 3 removes agentgateway.

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
| Seal MCP credentials to a transit key, encrypt-only for koinon-api and decrypt-only for the gateway (decision 12) | Decided against by the maintainer on 2026-11-19. The gateway-key envelope has one code path in every backend, `local` included. |
| Narrower Secret RBAC for koinon-api in the release namespace (decision 12.3) | RBAC has no deny and no label selector, and koinon-api must list Secrets it cannot name. A key in a Secret there stays readable. |
| A second namespace for the gateway key on every backend (decision 12.3) | Gives custody on `local` too, but the gateway needs Kubernetes API access (none today) and the chart gets a second namespace and Role. Not chosen on 2026-11-19: one code path and a custody report are enough (decision 12.3). |
| A NaCl sealed box instead of HPKE (decision 12.1) | Not in the standard library, no `info` or `aad`, no algorithm name in the format. |
| Let the gateway read ciphertext from Postgres and skip the reconciler's copy | The maintainer decided that the reconciler copies the ciphertext. Reading the column would need column grants on the sealed credential column (#278); revisit with slice 3. |
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

1. **OpenBao and the gateway-only key**: answered in decision 12.3. A separate transit key (or KMS
   key) wraps the gateway key, and only the gateway identity may decrypt with it. It works in all three
   DB modes, because the key lives in the KMS and not in the database. Closed.
2. **`local` backend without custody**: answered by the maintainer on 2026-11-19 (#1151), in decision
   12.3. One code path on every backend, and Koinon reports the custody level per install. Closed.

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
