---
status: accepted
date: 2026-11-23
area: tenancy-identity
tags: [api, auth, audit, backup-restore]
amends: ["0107", "0110"]
relates: ["0004", "0022", "0023", "0100"]
---
# 0116. RPC facts and the request entry

- Status: Accepted (decided by the maintainer, 2026-11-23, in the decision comment on #771)
- Date: 2026-11-23
- Area: tenancy-identity
- Tags: api, auth, audit, backup-restore
- Amends: [0107](0107-restore-hold-mode.md) (which layer holds a call that reaches outward) and
  [0110](0110-held-copy-isolation.md) (decision 5: the outward switch becomes two RPCs)
- Relates to: [0004](0004-human-identity.md) (roles), [0022](0022-frontend-architecture.md) (connect-rpc),
  [0023](0023-observability-and-audit.md) and [0100](0100-install-audit-chain.md) (the audit chain and the request ID)
- Issues: closes #771; part of the request entry (#695); tests in #672; children #772 to #788

## Context

Every Connect RPC and every admin REST route answers the same four questions before it does its work. Who may call
it (the minimum Role)? Is it refused while the install is held? What does a refusal look like? Which request ID does
the call carry? Today each handler answers these questions in its own code:

- **Role.** Each handler calls `authorize(ctx, orgID, minRole)` (`internal/services/services.go`). Some rules are finer
  than one Role. The content level `full` of a run-trace destination needs an owner. `AuditService.List` with no org ID
  is a platform admin call. These rules live in the domain code, and nothing lists them.
- **Hold.** ADR 0107 put the hold check in the service layer, so that "a new caller of a guarded service is held
  without any check of its own". That choice is right for callers that are not requests: `approvals.Execute` runs from
  `internal/apiassembly/domain.go`, and the importer, the sweepers, the A2A edge (`internal/services/a2aedge.go`) and
  remote agents (`internal/remoteagents/remoteagents.go`) call services too. For callers that are requests, the check
  is spread over 8 handler calls of `refuseWhileHeld` (`internal/services/hold.go`): 5 in `connections.go`, one each in
  `bindings.go`, `tasks.go` and `directmessage.go`. The A2A edge (`a2aedge.go`) makes a ninth call; it is not a request
  handler. The guards in the services add to them. A new RPC can reach outward and nothing tests that it is held.
- **Errors.** `TraceDestinationService` attaches `google.rpc.ErrorInfo` with 8 reason strings
  (`internal/services/tracedestinations.go`). Three more `withReason` calls exist: `PLATFORM_ADMIN_REQUIRED` in
  `platformprices.go` and `outward.go`, and `PRICE_REFRESH_OFF` in `platformprices.go`. Other services map errors by hand, and the REST `Error` has only `code`
  and `message` (`internal/adminapi/adminapi.go`). The web reads raw error text in 94 places (#778).
- **Request ID.** The REST path makes `req-` and 16 hex characters and returns `X-Request-Id`. The Connect path makes
  a UUID only in a handler that calls `requestCtx`, and it never returns it. The audit chain (ADR 0100) records the
  request ID, so a missing or a client-chosen ID weakens it.
- **One RPC with two answers.** `SetOutwardTargetDisabled` has one `disabled` field. Disable is allowed while held and
  enable is refused (ADR 0110 decision 5). A static table row cannot see the field.

The design note for this work (`design-09-request-entry.md`) was lost. The maintainer answered the six open
questions of the 2026-11-19 comment on #771 on 2026-11-23. This ADR records those answers.

## Decision

### 1. One table of RPC facts, and one entry that reads it

Every Connect RPC and every admin REST route has one row in a table of **RPC facts**, in Go, next to the entry. The
entry is the one place that reads the table. It runs before the handler, on both transports, and it enforces the row.
A row holds:

| Field | Meaning |
| --- | --- |
| `role` | The minimum Role in the named org (the floor). |
| `raises` | Optional: `owner` or `platform_admin`. The call has a field rule that needs more than the floor. |
| `held` | `refuse` or `allow`. Required. There is no default value. |

The registry spec (#672) fails when an RPC has no row, and when a row has no `held` value. A person must decide for a
new RPC. So a new RPC that reaches outward is never left unheld without a decision.

### 2. Hold: two layers with fixed roles

- **The entry** refuses a request when the row says `held: refuse` and the install is held. It uses the existing
  refusal (`FailedPrecondition` on Connect, `409` on REST) and adds the reason `INSTALL_HELD`. The reason is new:
  today the refusal has no reason string.
- **The service-level hold check** (`restorehold.Guard`) stays only for callers that are not requests: approvals
  Execute, the importer, the sweepers, the A2A edge and remote agents. A spec lists those callers. A service that
  such a caller reaches keeps its guard. A caller that is a request does not need a guard of its own.
- #775 deletes the checks that only requests reach. It must not delete the check that any listed caller uses.
- **The A2A edge.** Today `a2aedge.go` calls `refuseWhileHeld` from `hold.go`, and `hold.go` is deleted last. In the
  migration, the check of the A2A edge moves to the service-level guard (`restorehold.Guard`) before `hold.go` is
  deleted (section 7). The other listed callers already use the guard.

### 3. `SetOutwardTargetDisabled` becomes two RPCs

Add `DisableOutwardTarget` (`held: allow`) and `EnableOutwardTarget` (`held: refuse`). Deprecate
`SetOutwardTargetDisabled`. The proto change is add-only (`make breaking` passes). On REST, `PUT
/outward-targets/{kind}/{targetId}/disabled` disables, and a new `DELETE` on the same path enables. This is option A of the 2026-11-19 comment. Today the REST `PUT` takes a `{disabled}` body
(`api/admin/v1/openapi.yaml`); that body is deprecated. The behaviour of
ADR 0110 decision 5 does not change: disable reaches nothing and is allowed while held, and enable is refused while
held. Each row stays static.

### 4. Roles: a floor in the row, field rules in the domain

The entry checks the floor `role`. A row with `raises` tells a reader that the domain applies a stricter rule on a
field of the request (for example trace content level `full` needs `owner`, and an audit list with no org ID needs
`platform_admin`). The domain returns a typed refusal with the reason `ROLE_TOO_LOW` and the Role it needs. The registry
spec calls each `raises` row with the floor Role and expects that refusal.

### 5. Errors: `ErrorInfo` with reasons from one enum

- Keep `google.rpc.ErrorInfo` (domain `koinon.dev`) on the Connect path, as in AIP-193.
- The reason strings come from one proto enum. The value names are the strings. REST `error.reason` carries the same
  strings.
- A spec proves that the proto enum, the OpenAPI document and the Go code agree.
- Add a reason only when a client acts on it differently. All other errors carry the kind reason (`INVALID`,
  `NOT_FOUND`, `INSTALL_HELD`) and a message. Reasons are add-only. The web shows the message for a reason it does not
  know.

The enum is `koinon.refusal.v1.Reason` (`api/proto/koinon/refusal/v1/refusal.proto`, #777). It holds the 8 trace reasons
(`PLATFORM_ADMIN_REQUIRED` is one), `PRICE_REFRESH_OFF`, `CREDENTIAL_TOO_SHORT` and the four reasons of this ADR: `INVALID`,
`NOT_FOUND`, `INSTALL_HELD` and `ROLE_TOO_LOW`. The REST schema is `ErrorReason`. The web has `describeError`
(`web/src/lib/api-error.ts`). The trace screens use it first; the other screens move in #778. The REST path sends the
kind reasons `INVALID`, `NOT_FOUND` and `INSTALL_HELD` now. The Connect path sends them when the entry lands (#772,
#773); until then a Connect error that has no specific reason carries none.

The details of sections 1, 4 and 5 are as in option A of the 2026-11-19 comment: the table is in Go next to the entry,
the kind reasons, the add-only reasons and the web fallback.

### 6. Request ID: the entry always makes it, and returns it

- The entry always makes the request ID, in one format, for both transports, and returns it in `X-Request-Id`.
- A request ID that the client sends is written to the log as a separate field. It never goes into the audit chain.
  This keeps forged or duplicate IDs out of the chain (ADR 0100).
- The REST format changes once. Docs and tests that match `req-` change with it.

### 7. Order of the work

- #783, #784 and #785 keep their waits on #770, #759, #746 and #731.
- #787 is split. Each part of the old path is deleted when its last user moves: `problem()`, the per-file mappers,
  `mcpCaller`, `providerCaller`, `orgCaller` and `platformCaller`.
- The check of the A2A edge moves to `restorehold.Guard` before `hold.go` is deleted.
- `authorize`, `requestCtx`, `hold.go` and the comparison hook are deleted last. That pull request changes CODEOWNERS,
  so it merges with `--admin`.

### Amendments to earlier ADRs

- **ADR 0107, decision 2** (the guard): the service layer is no longer the layer for requests. Requests are held by
  the entry through the `held` column. The service guard stays for the callers that are not requests (section 2).
- **ADR 0110, decision 5** (the outward switch): the one method `SetOutwardTargetDisabled` is replaced by
  `DisableOutwardTarget` and `EnableOutwardTarget` (section 3).

Each amended ADR carries a note that names this ADR.

## Alternatives considered

Taken from the options of the 2026-11-19 comment on #771.

- **Hold: the entry only (delete all service guards).** A future workflow or sweeper that calls a service that was
  guarded is not held, and nothing finds that.
- **Hold: the service layer only (today).** The table only records the answer for a spec. The 8 handler calls plus the call on the A2A edge, and the
  per-file mappers stay, and #775 is dropped.
- **Split vs predicate for `SetOutwardTargetDisabled`: a request predicate on the row** (`HeldWhen func(req) bool`).
  The table then holds code, and the spec must call every predicate with each value of the field. A service check kept
  for this one RPC is an exception to section 2, and a reader of the table gets the wrong answer.
- **Roles: split the RPCs where the scope differs** (for example `ListOrgAudit` and `ListInstallAudit`). More RPCs and UI
  changes. It fits the audit scope, but not field rules such as `full`. Predicates in the table have the cost of the
  held predicate above.
- **Errors: a Koinon detail message with a typed enum field.** The trace UI and `traces_test.go` change, and generic gRPC
  tools cannot read it. **One reason for every domain sentinel:** many values freeze into a public API, and most have no
  client that uses them.
- **Request ID: accept a well-formed ID from the client.** A client can then choose the ID in audit events. **Use the
  OpenTelemetry trace ID:** with no tracer it is empty or random, and one trace can hold many requests.
- **Order: migrate the three waiting files now.** Merge conflicts with three refactors that rewrite the same files, and
  work done twice. **Keep #787 as one pull request:** the comparison hook and two authorization paths stay until the
  last of four other issues merges.

## Prior art

Quick check (about 15 minutes), 2026-11-23.

- **Kubernetes authorization attributes.** The API server builds one `Attributes` value for every request (verb,
  resource, and whether it is a resource request at all) and one `Authorizer` decides on it. Non-resource requests are a
  named case that the code handles on purpose. We borrow the idea that one entry derives the facts and decides, and that the
  non-request case is named. We differ: our facts are a static table per RPC, not a value built per request, because
  our rules are few and a spec can list them.
  [`interfaces.go`](https://github.com/kubernetes/kubernetes/blob/43cbb94ee845ad387717fe851bf5f421b7284329/staging/src/k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go#L35)
- **gRPC interceptors.** gRPC puts cross-cutting policy in a unary and a stream interceptor that sees the full method
  name, outside the handler. We borrow the place of the check (before the handler, on every method). Connect has the same
  shape. We add the table, so that a method with no row fails a spec instead of passing unchecked.
  [`interceptor.go`](https://github.com/grpc/grpc-go/blob/341c59808a9aaaf920baa4f1c1cd6c9087cae229/interceptor.go)
- **Google API errors (AIP-193 and `google.rpc.ErrorInfo`).** One machine-readable `reason` string and a `domain`, in a
  standard detail message that generic tools read. We borrow it as it is and add one proto enum as the source of the
  strings, so that REST and Connect cannot drift.
  [`0193.md`](https://github.com/aip-dev/google.aip.dev/blob/b78e92a9bfdaa03013e32325ba84068ebc570cd8/aip/general/0193.md),
  [`error_details.proto`](https://github.com/googleapis/googleapis/blob/0616ac54ade8ad70502627e445fa786f93154459/google/rpc/error_details.proto#L51)

## Consequences

- Positive: one place answers "who may call this and is it held". A new RPC fails the registry spec until a person
  decides `role` and `held`. Clients get a stable reason and a request ID on every refusal. The audit chain never holds
  a client-chosen ID.
- Negative: two layers for the hold, so two places to read. The field rules of `raises` live in the domain, and the
  table only names them. The REST request ID format changes once. A deprecated RPC stays for one period, and the REST `PUT` body
  `{disabled}` is deprecated with it (option A, 2026-11-19 comment).
- Follow-ups: the one request ID format is decided (one format for both transports); which format is chosen in #773.
  #772 to #788 (the entry on both paths, the table-driven hold spec, the refusal vocabulary, the web, the
  handler groups and the removal of the old path); #672 (the registry spec); GLOSSARY entries (**RPC facts**,
  **Request entry**).

## Open questions

None.

## Links

- #771 (decisions comment of 2026-11-23), #695, #672, #772 to #788
- [0107](0107-restore-hold-mode.md), [0110](0110-held-copy-isolation.md), [0100](0100-install-audit-chain.md)
