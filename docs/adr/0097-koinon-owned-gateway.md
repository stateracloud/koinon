# 0097. A Koinon-owned gateway, in three steps

- Status: Proposed
- Date: 2026-11-06
- Amends: [0040](0040-agentgateway-model-and-mcp-gateway.md) (supersedes in part decisions 1, 2 and 6 and
  the "Our own Go proxy" rejection: agentgateway becomes the first driver, not the destination),
  [0013](0013-model-access.md) (the "Our own Go proxy" alternative), [0041](0041-koinon-spend-ledger-and-budget-stops.md)
  (decisions 4 to 6 in step 3: the budget stop moves in front of the call),
  [0050](0050-spend-ledger-ingestion-and-net-limits.md) (the ledger reads a usage record, not access-log
  fields), [0056](0056-model-providers-pricing-and-budgets-per-org.md) (decision 5 in step 3: the
  gateway's own cost and catalog fall away), [0057](0057-mcp-servers.md) (items 4, 5 and 7 in step 2:
  MCP targets, credential injection and the ask target move into Koinon's MCP path),
  [0058](0058-gateway-modes.md) (supersedes in part: shared mode is not built, Kubernetes mode is frozen
  and ends with step 3), [0049](0049-v01-scope-decisions.md) (Kubernetes-mode support ends with step 3),
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
- Decision by: the maintainer, 2026-11-06 (the three steps); the details below are drafted for review

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
criteria hold, and agentgateway stays a supported driver until Koinon's gateway passes the gateway
behaviour suite.

Who owns each job:

| Job | Today | Step 1 (v0.1) | Step 2 (v0.2) | Step 3 (v0.3) |
| --- | --- | --- | --- | --- |
| Placeholder swap in the sandbox | OpenShell supervisor | unchanged | unchanged | unchanged |
| Agent key check, MCP path | agentgateway | agentgateway | **Koinon** | Koinon |
| Agent key check, model path | agentgateway | agentgateway | agentgateway | **Koinon** |
| MCP targets, tool allowlist, prefixing | agentgateway (rendered by Koinon) | agentgateway | **Koinon** | Koinon |
| MCP credentials: static, sign-in (0096), passport (0073) | static only, agentgateway | static only, agentgateway | **Koinon**, all three | Koinon |
| Leak guard: decisions | Koinon | Koinon | Koinon | Koinon |
| Leak guard: which messages it sees | agentgateway's hook (successful replies) | hook, and Koinon fails closed on a reply it never saw | **every message**, in Koinon's MCP path | Koinon |
| Header allowlist, `kgw_` refusal | agentgateway (standalone), interim guard (Kubernetes) | unchanged, in the suite | **Koinon** (MCP) | **Koinon** (model) |
| Model routing, provider keys, retries, failover | agentgateway | agentgateway | agentgateway | **Koinon** |
| Streaming usage parsing | agentgateway | agentgateway, mapped to the usage record | agentgateway | **Koinon** |
| Cost truth and price list | Koinon (0056 decision 5), agentgateway's catalog as cross-check and fallback | unchanged | unchanged | **Koinon only** |
| Budget stop in the path | agentgateway, charge after the response | unchanged | unchanged | **Koinon, call reservation before the call** |
| Model-call spans | agentgateway | agentgateway | agentgateway | **Koinon** |
| Egress of the gateway pod | chart NetworkPolicy | unchanged | Koinon's gateway: plain NetworkPolicy | unchanged |

Nine more jobs, each with its gap and how it is filled (own-gateway design research, Links):

| Job | Gap, and how it is filled | Today | Step 1 | Step 2 | Step 3 |
| --- | --- | --- | --- | --- | --- |
| Request rate limit per key | Only USD and run caps bound a runaway loop; a token bucket per key, limits from State (open question 4) | none found | none | none | **Koinon**, if decided |
| Config delivery | Files, kubelet tick, 90 s settle, probe key; a revisioned State stream the gateway acks (decision 2.1) | files or CRDs | unchanged | **Koinon** (MCP half) | **Koinon** (model half) |
| Unknown MCP message kinds | A new method passes unguarded; refuse every method not on an allowlist | agentgateway forwards | unchanged | **Koinon refuses** | Koinon |
| Guard argument order (#256) | Arguments are joined in call order, so a secret split across reordered keys passes; scan a canonical form | Koinon | **Koinon fix** | Koinon | Koinon |
| MCP-call spans | Only implied today; a span per tool call (target, tool, verdict, duration), no arguments or results | agentgateway | agentgateway | **Koinon** | Koinon |
| Audit of tool calls and credential use | The new path must write it; one row per call and per credential use, no token values (ADR 0096) | approvals only | unchanged | **Koinon** | Koinon |
| Availability, timeouts, stream drain, outbound proxy | Koinon owns a hot path; 2+ replicas, a PodDisruptionBudget, drain of open streams, per-stage timeouts, `HTTPS_PROXY` honoured upstream | agentgateway | agentgateway | **Koinon** (MCP) | **Koinon** (model) |
| The behaviour suite | `gatewaytest` checks rendering only; request-path cases (decision 4) | none | **Koinon** | both drivers | both drivers |
| Gateway pod egress scope | Plain NetworkPolicy cannot filter by name, so egress is broad; the step 2 design states the hosts and ports, no CNI features | chart | unchanged | **stated** | unchanged |

### 2. The seam

The seam is the set of Koinon interfaces every gateway driver sits behind. It is defined by
behaviour, not by config: the gateway behaviour suite (decision 4) is its specification.

1. **Desired state.** `gateway.State` stays the one description of what a gateway serves (keys, limits,
   providers, models, MCP targets, tool grants). In step 2 it splits into a model half and an MCP half,
   and the deferred `MCPGateway` interface of ADR 0040's 2026-10-26 amendment is created, because from
   then on the two halves have different drivers. Drivers converge to the state; how is the driver's
   business. agentgateway's drivers keep rendering files or CRDs. Koinon's gateway subscribes to a
   revisioned State stream from koinon-api over Koinon RPC and acks each revision it serves, so for it
   there are no files, no kubelet tick, no key settle and no probe key.
2. **Usage.** Every driver yields **usage records** (decision 3). Nothing outside a driver reads
   gateway log attributes.
3. **Koinon hooks.** The calls a gateway makes into Koinon are Koinon interfaces, with the transport a
   driver detail: the leak guard (`internal/mcpguard`; ExtMcp gRPC for agentgateway, a Go call or
   Koinon RPC for Koinon's gateway), the token service (ADR 0073, ADR 0096), and in step 3 the call
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
  buckets (input excludes cache, output includes reasoning, as today's `koinon.*` fields);
- request `max_tokens` and request bytes (for estimates and reserves, ADR 0056), response ID;
- the gateway's own cost, optional, only while a driver has one.

The agentgateway driver maps its OTLP access log to usage records; that mapping (today
`internal/ledger/otlp.go` and the `LogField*` constants) moves into the driver package in step 1.
Koinon's gateway writes usage records directly. The ledger's pricing, estimates and idempotency do not
change.

### 4. Step 1 (v0.1): keep agentgateway, fence it by contract

Scope:

- **The gateway behaviour suite.** Extend `internal/gateway/gatewaytest` from "what the driver renders"
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
  message kind the guard cannot see. The upstream fix is drafted for a human to post (AGENTS.md,
  OpenShell and upstream rules apply to agentgateway too).
- **The usage record** (decision 3) and the leakage cleanup (decision 2.5).
- **The guard scans arguments in a canonical form** (#256): keys sorted, each value scanned on its own.
- **Fix the `gateway.Limit` comment** (`internal/gateway/gateway.go:300-302`): a limit can be overshot
  by one in-flight response per concurrent call, not by one response.
- **Supply chain (#238).** One mirrored, digest-pinned agentgateway image for the chart, the Go tests
  and kind; no test pulls from `cr.agentgateway.dev`.
- **Kubernetes mode is frozen.** It keeps passing the suite and gets no new features. Shared mode (ADR
  0058) is not built: an existing gateway an org already runs is reached as a provider upstream, as
  ADR 0058 decision 2 already allows.

Exit criteria:

1. The suite runs in the required Go tests against the mirrored image, every case passes or is an
   expected failure with an open issue.
2. The #254 fail-closed rule is merged with a test against the real image (an error reply carrying a
   canary sets both legs).
3. The ledger reads only usage records; `internal/ledger` imports no agentgateway attribute names.
4. No Go test or CI job pulls agentgateway from an upstream registry.

### 5. Step 2 (v0.2): a Koinon MCP path

Scope: Koinon's gateway serves the MCP path, written in Go on the official MCP Go SDK
(`github.com/modelcontextprotocol/go-sdk`), designed together with MCP sign-in (#237, ADR 0096) and the
agent passport (#40, ADR 0073). agentgateway keeps the model path.

- The MCP path is an aggregating go-sdk server with one go-sdk client per upstream target. The guard
  **wraps each upstream client's `Connection`**, the pattern of the SDK's `LoggingTransport`, so every
  raw JSON-RPC message (result, error, notification, server-initiated request) passes the guard before
  the SDK dispatches it. Method middleware handles grants, the ask tier and request-side checks. The
  guard runs in the same process or over a Koinon RPC (open question 1).
- Only methods on an allowlist are forwarded; any other method is refused. Whether servers may send
  sampling and elicitation requests is open question 3.
- Each tool call writes one span and one audit row (target, tool, guard verdict, credential used), with
  no arguments, results or token values.
- Per-target credentials are attached by Koinon's MCP path: the static credential (ADR 0057 item 5),
  the MCP sign-in token (ADR 0096) and the passport (ADR 0073). Each exists only between the gateway and
  the MCP server. This replaces ADR 0073's delivery through ext_authz and `backendAuth`, its delivery
  spike, and ADR 0096's gateway spike; the token service in koinon-api stays.
- The ask tier can park a call in the path, so the second `<org>-<server>-ask` target of ADR 0057 item
  7 may fold into the main target (decided in the step 2 design). _(inferred)_
- The same code runs in every cluster as a plain Deployment and Service, so the MCP half of the
  standalone/Kubernetes split ends.
- **Design notes for the step 2 design** (per-replica limits, with Envoy's timeout, stream-limit and
  drain defaults as the checklist):
  - a cap on open streams per replica that answers 503 with `Retry-After` before anything is reserved;
  - scaling on an open-streams gauge (fixed labels), not on CPU, since idle streams use no CPU;
  - the upstream connection pool sized up from Go's default of 2 idle connections per host;
  - per-stage timeouts;
  - a shutdown grace period as long as the longest allowed stream, and a PodDisruptionBudget;
  - MCP session affinity across replicas (open question 5).

Exit criteria:

1. The MCP half of the suite passes against Koinon's gateway with **no** expected failures: the guard
   sees every server message kind, key refusals carry their markers, headers are handled.
2. A different credential per target on a fan-out `tools/list` and `tools/call`, with the static
   server receiving none (the question ADR 0073 and ADR 0096 wanted a spike for).
3. ADR 0096's stage A e2e (fake AS and fake OAuth MCP server on kind) is green.
4. The escape suite finds no sign-in token, passport or MCP credential in the sandbox (ADR 0025).
5. Streamable HTTP and the MCP spec version Koinon pins work with the go-sdk.
6. The build stays within twice its estimate (section Risks); past that, it goes back to the
   maintainer before it continues.

Then agentgateway's MCP configuration retires (decision 7).

### 6. Step 3 (v0.3): a Koinon model path

Scope: Koinon's gateway serves the model path behind the same seam, starting with the API formats and
provider dialects Koinon routes. Each model family speaks its native format (ADR 0069), so the gateway
forwards bodies and does no format translation.

- **A thin pass-through proxy Koinon writes**: `httputil.ReverseProxy` with `Rewrite`, building each
  outbound request from the header allowlist, and an SSE usage tap per format that reads events as
  they pass and keeps no body. It retries or fails over only before the first response byte.
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
  - Request-rate limits, if open question 4 adds them, use a token bucket inside each replica; they are
    a safety limit, not money.
- **Ledger unreachable at reserve time: allow the call, charge it afterwards** (decided by the
  maintainer, 2026-11-06). The gateway forwards the call without a reservation and charges it from its
  usage record once the ledger is back, for every call it allowed, idempotent on the request ID. The
  consequence, stated plainly: **during a ledger outage, budgets can be overspent by whatever runs in
  that window.** The run caps (requests, USD, wall clock; ADR 0081) still apply. The gateway counts
  calls allowed without a reservation and alerts on them, with fixed metric labels (ADR 0023).
- **Entry work for step 3: three measurements**, recorded in this ADR before step 3 commits to numbers.
  Docker-gated tests, no cluster:
  1. single-row writes per second on Postgres 18, plain per-call versus batched;
  2. gateway memory per open stream at 1k, 5k and 10k streams per replica, against the fake model
     server;
  3. the MCP `Connection` wrapper on Streamable HTTP.
- Streaming usage parsing for each routed format, writing usage records; cut streams follow ADR 0056
  decision 6.
- Cost comes only from Koinon's prices; agentgateway's catalog, the `gateway_cost_usd` cross-check and
  the "served when the gateway can price it" fallback of ADR 0056 decision 5 end (open question 2).
  To fill the default price list, a reviewed import tool may propose entries from LiteLLM's MIT price
  JSON for a person to approve; nothing is downloaded at runtime.
- Retries, failover and do-not-retry markers are Koinon's (#112).
- Model-call spans go from the gateway to the trace router (ADR 0074).

Exit criteria:

1. The whole suite passes against Koinon's gateway with no expected failures, including usage for
   every routed format and dialect against recorded fixtures.
2. A concurrency test holds the budget: with the ledger reachable, N parallel calls near a limit
   never record spend above it. A ledger-outage test shows every allowed call charged once afterwards.
3. The quickstart e2e is green with Koinon's gateway as the default driver.
4. Every catalog model with a `cache_read` price runs with caching on through Koinon's gateway (ADR
   0068's test).

When these hold, Koinon's gateway becomes the default. The agentgateway standalone driver stays
selectable and keeps running the suite in CI; removing it is a separate, later decision.

### 7. What retires

| What | Where today | Retires |
| --- | --- | --- |
| The fixed 90 s key settle | `internal/cmd/koinonreconciler/commands.go:132` (`--provider-key-settle`) | step 3, when Koinon's model path is the default; #250 is closed, not planned (decision 8) |
| ExtMcp hook definitions | `third_party/agentgateway` (vendored proto), `buf.gen.agentgateway.yaml`, `Makefile` target, `internal/mcpguard/grpc.go`, `internal/gateway/guard.go` | step 2 |
| MCP config rendering | MCP targets, `ToolRules` CEL, prefixing, MCP credential files and their settle (`gatewayplan/plan.go`) | step 2 |
| Kubernetes mode | `internal/gateway/kubernetes`, the upstream controller and its CRDs, the #54 interim guard, `.github/workflows/g2-kind.yml` | step 3 |
| Model config rendering | `internal/gateway/standalone` (render, reload, reload log, revision probe) | when the agentgateway driver is removed (after step 3) |
| The access-log contract | `LogField*` constants and the OTLP decode, as a ledger input | step 1 (moves into the driver); gone with the driver |

### 8. Effect on open issues

- **#254** (guard misses MCP errors and server messages): step 1, closed on Koinon's side by the
  fail-closed rule; fully closed in step 2, where the guard sees every message.
- **#249** (wake on credential arrival): lands in v0.1.
- **#250** (config pickup ADR): closed as not planned (the maintainer, 2026-11-06). Neither the single
  projected volume nor the pod-annotation touch with its `patch pods` grant is built. The gateway's
  config and key pickup keeps waiting on the kubelet tick, and the 90 s key settle stays, until the
  step 3 model path retires the agentgateway driver's config delivery. The pickup research (Links)
  stays as the evidence for that choice.
- **#54** (Kubernetes-mode header allowlist): stays on the interim 403 guard in v0.1; its MCP half ends
  in step 2 and the rest with Kubernetes mode in step 3. Moves to v0.3.
- **#112** (do-not-retry markers): the suite records today's markers in step 1; the MCP markers are
  Koinon's in step 2 and the model markers in step 3. Moves to v0.3, MCP part done in v0.2.
- **#78** (thinking-token split): an expected failure in the suite from step 1; fixed by an upstream
  release that passes the case, or in step 3 by Koinon's parser. Moves to v0.3 unless upstream fixes it
  first.
- **#238** (upstream registry outage): step 1, as above; unchanged by the later steps while the
  agentgateway driver lives.
- **#40** (passport delivery spike on agentgateway): re-scoped in step 2 to passport delivery in
  Koinon's MCP path; the agentgateway spike is not needed.
- **#237** (MCP sign-in): built in step 2, as ADR 0096 plans; its delivery is Koinon's MCP path.

### 9. Rules that hold in every step

- **No secrets in sandboxes.** Provider keys, MCP credentials, sign-in tokens and passports exist only
  in the gateway and koinon-api; the sandbox holds only its placeholder, swapped by the OpenShell
  supervisor. ADR 0044 stays the only, gated exception.
- **Never Kyverno.** Any admission fence a step needs is a native ValidatingAdmissionPolicy.
- **No CNI-specific features.** The gateway is isolated with standard NetworkPolicy, as today.
- **Fixed metric labels.** Gateway metrics carry no org, agent, user, ticket or run ID (ADR 0023).
- **Prompt caching always on.** Every driver forwards the harness's cache markers unchanged, and
  usage records carry cache reads and writes (ADR 0068).
- **OpenShell defaults untouched.** `enforcement: enforce` in every tier (ADR 0045) and the isolation
  tiers (ADR 0075) do not change; the sandbox-facing gateway endpoint stays the same (decision 2.4).
- **No response bodies recorded.** agentgateway's config never reads `response.body`; Koinon's gateway
  parses usage from the stream as it passes and keeps no body.

## Alternatives considered

| Option | Why not |
| --- | --- |
| **Stay on agentgateway for good and fence it** (the dependency review's option A alone) | Cheapest now, and step 1 is exactly this. But each minor can move behaviour the guard and the ledger depend on, security fixes wait for a Rust upstream (#4245 open since 2026-10-30, #3907 since 2026-10-10, neither with a PR), Kubernetes mode keeps a known bypass (#54), and sign-in and the passport get built on open MCP auth bugs. |
| **Own the MCP path only**, agentgateway for models for good | Takes the part where Koinon's security lives, and is step 2. Stopping there keeps the budget stop after the call (concurrent overshoot), usage parsing Koinon cannot fix (#78), the price catalog cross-check, the config rendering and Kubernetes mode for the model path. |
| **Swap to another gateway** (Agent Router, Bifrost, LiteLLM, Portkey, Kong, ContextForge) | The same coupling with a new vendor, and the kubelet and registry costs come along unchanged. None has an open-source fail-closed in-path MCP hook as complete as ExtMcp (review F9). Bifrost's open-source governance plugin does have virtual-key, team and customer budgets and MCP governance, but its check is read-then-charge in memory per replica (`store.go:2248-2283`); user-level limits and cross-replica sync are Enterprise (per Obot's ADR). Agent Router stays the named fallback for the model path (#98). |
| **Build the whole gateway in v0.1** | Delays v0.1 by ADR 0040's 4-8 weeks plus the MCP path, and builds OAuth plumbing before ADR 0096 settles. The suite would not exist yet to say when the new gateway is good enough. |
| Embed a gateway engine (Bifrost) for the model path, as Obot (MIT) did | Less parsing code, but: a large dependency tree whose internal API is no stable contract (Obot's ADR), format translation Koinon does not want (ADR 0069), budgets that overshoot across replicas, a pricing datasheet whose terms forbid redistribution, and frequent releases against the 3-day rule (ADR 0079). Obot still had to write budgets itself, and Koinon would still write budgets, the guard and usage records. At most a source of ideas for step 3. |
| MCP path with the guard in method middleware only (as Docker's MCP gateway does) | Errors reach middleware as Go `error` values and transport-level messages are out of reach, so the guard would again miss replies: the #254 class in Koinon's own code. |
| Budget counters in the gateway's memory | Overshoot by up to N times with N replicas. |
| Redis or Valkey for budget counters | Budgets are money. Redis replication is asynchronous and can lose acknowledged writes on failover, so Postgres would still be the truth and Koinon would run two stores and a reconcile between them, the class of bug LiteLLM keeps fixing. It adds a stateful service to size, persist, fail over and back up in all three DB modes, and a fail-open or fail-closed choice when it is down (and Redis 8's licence means Valkey, one more supply-chain line). The headroom is not needed: one batched Postgres row per org covers the expected load. |
| Envoy AI Gateway (Agent Router) as the data plane | Charges after the response and accepts overshoot, needs Redis for its limits, and brings several components plus the vendor coupling this ADR removes. |
| Envoy in front, with a Koinon `ext_proc` server | Buys connection handling only. Koinon would still write the usage tap, reservation, guard and MCP aggregator, every stream chunk would cross gRPC, and operators would run one more component. Kept as an escape hatch if Go's connection handling ever proves to be the limit. Koinon borrows Envoy's designs instead: RLQS and Doorman lease expiry, and its timeout, stream-limit and drain defaults as a checklist. |
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
- Negative: two drivers run side by side from step 2 to at least step 3, and the suite must run against
  both.
- Negative: the MCP sign-in and passport work (ADR 0096, ADR 0073) changes delivery mechanism; their
  agentgateway spikes are dropped. _(inferred)_
- Follow-ups: issues for the step 1 items (suite, usage record, leakage cleanup, mirror, #254 fail
  closed, Kubernetes-mode freeze); a step 2 design issue joined to #237 and #40; a step 3 issue; on
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

Each gets a tracking issue when this ADR is accepted.

1. **Where the gateway runs**: in koinon-api, or a separate `koinon-gateway` Deployment from the same
   image. Answered by the step 2 design with #237 and #40. Default: separate, so provider keys and MCP
   credentials stay out of the internet-facing process, with the guard and token service over Koinon
   RPC.
2. **Unpriced models in step 3**: refuse, or charge a platform fallback price (ADR 0041 decision 7).
   The maintainer decides in v0.3 planning. Default: refuse, unless a fallback price is set.
3. **MCP sampling and elicitation**: whether servers may send these requests, or Koinon refuses them.
   Answered by the #254 test in step 1 and fixed in the step 2 design.
4. **Per-key request rate limits**: whether step 3 adds them, or USD and run caps stay the only
   bounds. If added, they are a token bucket inside each replica (decided). Answered in the step 3
   design.
5. **MCP session affinity across replicas**: Service `sessionAffinity: ClientIP` on the sandbox's
   address (plain Kubernetes), forwarding to the replica that owns the session, or the go-sdk's
   stateless mode, whose existence is unverified. Answered in the step 2 design; default `ClientIP`
   unless stateless mode is confirmed.

(The call reservation details, formerly an open question, are decided in step 3: Postgres only,
batched, with an allow-and-charge-afterwards rule for a ledger outage.)
6. **When the agentgateway driver is removed**: a short later ADR, once Koinon's gateway has been the
   default for one minor and #98's fallback check is done.
7. **Whether anyone needs Kubernetes mode**: asked on the release checklist before step 3; the answer
   only changes the migration notes, not the plan.

## Upstream verification

Checked 2026-11-06 in the research reports (Links), from source and docs; nothing downloaded was run:

- agentgateway v1.7.0 (`26f5aebe`): `mcp/handler.rs:2183-2208` sends only `Response` messages to the
  guardrail response hook; `http/budget/mod.rs` checks an in-memory counter before the call
  (`:554-559`), pushes settlement after it (`:561-564`) and settles in `settle()` (`:493-519`);
  `messages.rs` never sets `reasoning_tokens`. Upstream issues #4245, #3907, #3583 open.
- `github.com/modelcontextprotocol/go-sdk` v1.8.0 (already in `go.mod`): Streamable HTTP, SSE and
  stdio, client and server, Apache-2.0/MIT. It speaks the 2026-07-28 spec (`latestProtocolVersion`,
  `mcp/shared.go:50-51`), and the Go proxy serves it as a release, not a pre-release. Its transport
  `Connection` sees every raw JSON-RPC message (`LoggingTransport`, `transport.go:356`).
- Bifrost: the open-source governance plugin has budgets and MCP governance
  (`plugins/governance/main.go:39-45`); its check is read-then-charge (`store.go:2248-2283`). Obot's
  licence is MIT (GitHub metadata).
- Go 1.27.3 `net/http/httputil.ReverseProxy` flushes `text/event-stream` at once
  (`reverseproxy.go:681-696`).
- Not verified: Coder AI Bridge's budget enforcement; hosted products' internal routing (inferred from
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
- [0040](0040-agentgateway-model-and-mcp-gateway.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0049](0049-v01-scope-decisions.md), [0057](0057-mcp-servers.md), [0058](0058-gateway-modes.md),
  [0073](0073-agent-passport.md), [0084](0084-leak-guard.md), [0091](0091-leak-guard-by-destination.md)
- ADR 0096 (MCP sign-in): [PR #248](https://github.com/stateracloud/koinon/pull/248)
- Issues: [#258](https://github.com/stateracloud/koinon/issues/258), #254, #250, #249, #238, #237, #112,
  #98, #78, #57, #54, #47, #40
- https://github.com/agentgateway/agentgateway/issues/4245
- https://github.com/modelcontextprotocol/go-sdk
- https://github.com/coder/aibridge
