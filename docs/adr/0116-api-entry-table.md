---
status: accepted
date: 2026-11-23
area: security-audit
tags: [api, auth, audit, backup-restore, testing]
amends: ["0107", "0110"]
relates: ["0100"]
---
# 0116. The API entry table: one row of facts for each RPC

- Status: Accepted (decided by procoduck, 2026-11-23)
- Date: 2026-11-23
- Area: security-audit
- Tags: api, auth, audit, backup-restore, testing
- Amends: [0107](0107-restore-hold-mode.md) (decision 2: which layer refuses a call while held),
  [0110](0110-held-copy-isolation.md) (decision 5: how enable and disable are held)
- Relates to: [0100](0100-install-audit-chain.md) (the audit chain records the request ID)
- Issues: #771; the work is in #672, #772, #773, #775, #778, #783, #784, #785 and #787

## Context

Each API request goes through checks that today live in each handler. Line numbers are from `origin/main` at
a0ddef1, 2026-11-23.

1. **Role.** `authorize` checks one minimum Role for each call (`internal/services/services.go:308`). Some rules
   are finer than one Role. A trace content level of `full` needs an owner (`internal/tracedest/settings.go:119`).
   `AuditService.List` without an org ID is a platform admin call (`internal/services/audit.go:93`).
2. **Restore hold.** ADR 0107 decision 2 puts the hold check in the service layer: each service that calls out has
   a `restorehold.Guard` and returns `restorehold.ErrHeld`. The handlers map that error in `dbError`
   (`internal/services/services.go:339`) and in `problem()` (`internal/adminapi/adminapi.go:310`). The connection,
   binding, task and direct message handlers, and the A2A edge, also call `refuseWhileHeld`
   (`internal/services/hold.go`).
   Not every caller of a guarded service is a request: approvals `Execute` also runs from
   `internal/apiassembly/domain.go:91`.
3. **One RPC, two hold answers.** `SetOutwardTargetDisabled` has one `disabled` field
   (`api/proto/koinon/outward/v1alpha1/outward.proto:36`). ADR 0110 decision 5 allows disable while held and refuses
   enable while held (`internal/outward/outward.go:203`). A check at the entry sees the RPC, not the field.
4. **Errors.** TraceDestinationService attaches `google.rpc.ErrorInfo` (domain `koinon.dev`) with a reason
   (`internal/services/tracedestinations.go:106`), and the web reads it (`web/src/lib/trace-errors.ts`). The REST
   `Error` has only `code` and `message` (`api/admin/v1/openapi.yaml:1219`). In other places the web shows raw
   error text (#778).
5. **Request IDs.** REST makes `req-` and 16 hex characters for each request and returns it in `X-Request-Id`
   (`internal/adminapi/adminapi.go:175` to `:185`). Connect makes a UUID only in the handlers that call `requestCtx`
   (`internal/services/tokens.go:109`, 15 files), and it does not return the ID.

The design note that first framed this work (`design-09-request-entry.md`) is lost. The issue comment of 2026-11-19
replaced it with six questions, and the maintainer answered them on 2026-11-23. This ADR records those answers.

## Decision

Decisions 1 to 6 record the maintainer's answers (procoduck, 2026-11-23) to the six questions of 2026-11-19. They
do not add new choices; the open questions below are the parts the answers leave open.

The **entry** is the one place where each API request starts, on the Connect and on the REST path. The **entry
table** holds one row of facts for each RPC. The entry reads the row and applies the facts before the handler runs.
The registry spec (#672) checks every row.

### 1. Restore hold: two layers, each with a fixed role

- Each row of the entry table must say `held: refuse` or `held: allow`. The field has no default value. A new RPC
  fails the registry spec until a person decides its value.
- The entry refuses a `held: refuse` RPC while the install is held. The error is the one of ADR 0107 decision 2
  (`FailedPrecondition` on Connect, `409` on REST).
- The service-level hold check (`restorehold.Guard` in the service) stays only for callers that are not requests:
  - approvals `Execute` (`internal/apiassembly/domain.go:91`);
  - the declarative importer (`internal/declarative/declarative.go`);
  - the sweepers (for example the approvals sweeper, `internal/approvals/approvals.go:540`);
  - the A2A edge (`internal/services/a2aedge.go:38`);
  - remote agents (`internal/remoteagents/remoteagents.go:142`).
- A spec lists those callers.
- #775 deletes the hold checks that only requests reach. It must not delete the hold check that the A2A edge uses.

### 2. Split the outward switch into two RPCs

`SetOutwardTargetDisabled` is split into `DisableOutwardTarget` (`held: allow`) and `EnableOutwardTarget`
(`held: refuse`). The old RPC is deprecated. The proto change is add-only (`make breaking` passes). Each fact in
the table thus stays static, and the table stays the one source.

### 3. Role rules: a floor in the row, field rules in the domain

- The row holds the minimum Role of the RPC. The entry checks it.
- A row can also have an optional `raises: owner` or `raises: platform_admin` mark. The mark says that some
  requests of this RPC need more than the minimum Role.
- The domain checks the field rule and returns a typed `ROLE_TOO_LOW` refusal.
- The registry spec (#672) tests each `raises` row (for example the trace content level `full`, and an audit list
  with no org ID).

### 4. Refusal reasons on the wire

- Connect errors keep `google.rpc.ErrorInfo` (domain `koinon.dev`).
- The reason strings come from one proto enum. REST `error.reason` uses the same strings.
- A spec proves that the proto enum, the OpenAPI document and the Go code agree.
- A new reason is added only when a client acts on it differently from the reasons that exist.

### 5. Request IDs

- The entry always makes the request ID, in one format for the two transports, and returns it in `X-Request-Id`.
- An ID that the client sends is logged. It never goes into the audit chain (ADR 0100), so a client cannot choose
  the ID of an audit event.
- The REST ID format changes once.

### 6. Order of the work

- #783, #784 and #785 keep their waits for #770, #759, #746 and #731.
- #787 is split. The #787 work deletes each part of the old path when its last user moves to the entry.
- The #787 work deletes `authorize`, `requestCtx`, `hold.go` and the comparison hook last. That pull request changes
  CODEOWNERS, so it needs `--admin`.

## Amendment to ADR 0107 decision 2

ADR 0107 decision 2 puts the hold check at the service layer, "so the hold holds for every caller and both API
surfaces". From this ADR, two layers share the work, each with a fixed role:

- **The entry** decides the hold for every request. The `held` fact of the RPC's row decides.
- **The service guard** holds only the callers that are not requests. Decision 1 lists them, and a spec lists
  them too.

The list of what is refused while held in ADR 0107 does not change; only the place of the check changes
_(inferred)_. The
`refuseWhileHeld` checks in the handlers go away when their RPCs move to the entry (#775), except the A2A edge.

## Amendment to ADR 0110 decision 5

ADR 0110 decision 5 has one method, `SetOutwardTargetDisabled`, that allows disable and refuses enable while held.
From this ADR, `DisableOutwardTarget` (`held: allow`) and `EnableOutwardTarget` (`held: refuse`) replace it, and
the old method is deprecated. The rule does not change: disable is allowed while held, and enable is refused.

## Alternatives considered

- **Restore hold at the entry only.** Delete all service guards. Rejected: a future workflow or sweeper that calls
  a guarded service is not held, and nothing finds that.
- **Restore hold at the service layer only (today).** The table only records the answer. Rejected: the handler
  checks and the per-file error mappers stay.
- **A request predicate in the row** (`HeldWhen func(req) bool`, or the same for the Role). Rejected: the table
  holds code, and the spec must call each predicate with each value of each field.
- **Keep the service check for the outward enable.** Rejected: an exception to decision 1, and a reader of the table
  gets the wrong answer.
- **Separate RPCs for each Role scope** (for example `ListOrgAudit` and `ListInstallAudit`). Not chosen for now:
  more RPCs and UI changes, and it does not fit field rules such as `full`.
- **A Koinon error detail message with a typed enum field.** Rejected: the trace UI and its tests change, and
  generic gRPC tools cannot read it.
- **One reason for each domain sentinel error.** Rejected: many values become a public API that no client uses.
- **Accept a well-formed request ID from the client.** Rejected: a client could choose the ID in the audit chain
  (forged or duplicate IDs, ADR 0100).
- **Use the OpenTelemetry trace ID as the request ID.** Rejected: with no tracer the ID is empty or random, and one
  trace can contain many requests.
- **Migrate the waiting handler files now** (#783 to #785). Rejected: merge conflicts with the refactors of #770,
  #759, #746 and #731, which change the same files, and work done twice.

## Consequences

- Positive: one table says, for each RPC, the minimum Role, the hold answer and the `raises` mark. The registry spec
  checks every row, so a new RPC cannot ship without these decisions.
- Positive: one request ID on both transports, returned to the client, and never chosen by the client.
- Positive: clients read one set of reason strings on Connect and on REST.
- Negative: a hold rule lives in two places (the entry and the service guard). The roles of the two layers are
  fixed, and a spec lists the callers of the second layer.
- Negative: a `raises` rule lives in two places (the row marks it, the domain checks it).
- Negative: the outward switch needs a proto change, a UI change and a deprecation period.
- Negative: the REST request ID format changes; docs and tests that match `req-` change with it _(inferred)_.
- Negative: each new reason needs a review: does a client act on it differently?
- Follow-ups: #672, #772, #773, #775, #778, #783, #784, #785 and #787 (split as decision 6 says).

## Open questions

The maintainer's answers do not decide these. They stay open until the maintainer decides them.

- The name of the reason enum and of its proto package.
- The one format of the request ID.
- The REST shape of the two outward RPCs.

## Links

- Issue #771: the questions of 2026-11-19 and the decisions of 2026-11-23.
- [0107](0107-restore-hold-mode.md) restore-hold mode, [0110](0110-held-copy-isolation.md) a restored copy does not
  act as its source, [0100](0100-install-audit-chain.md) install audit chain.
- AIP-193 (errors): `google.rpc.ErrorInfo`.
