# 0100. An install-level audit chain for platform actions

- Status: Proposed
- Date: 2026-11-07
- Amends: [0023](0023-observability-and-audit.md) (the 2026-10-26 amendment's "no org-less events
  and no platform chain"; the lock order; the deferred superuser item and open question 1)
- Relates to: [0004](0004-human-identity.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md),
  [0074](0074-run-traces-trace-router.md), [0085](0085-upgrade-compatibility-and-recovery-policy.md),
  [0098](0098-price-catalogue-updates-from-public-sources.md), [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md)
- Issue: [#324](https://github.com/stateracloud/koinon/issues/324)

## Context

Koinon's audit log is one hash chain per org (ADR 0023, amendment of 2026-10-26). `audit_events` is
keyed by `(org_id, id)`, `org_id` is not null and references `orgs`, and a `SECURITY DEFINER`
trigger locks the org's row in `audit_chain_heads` and computes `chain_seq`, `prev_hash` and `hash`.
There is no place for an event that belongs to no org.

Install-wide actions exist anyway, and today they are recorded in one of three ways:

- **Copied into every platform admin's personal org.** `keys forget` (`internal/keyscli/forget.go`)
  and the platform prices of PR #316 (`auditPlatform` in `internal/prices/platform.go` on
  `feat/265-price-catalogue-fetch`) write one event per platform admin. With no platform admin, a
  person's change is refused and a change Koinon makes itself (the price refresh, a retirement, the
  settings sync) is only a `slog` warning: "this platform price change is in no org's audit log".
  This is the gap issue #324 names, and #305 cannot merge while platform prices can change unaudited.
- **Written in the acting admin's personal org.** `platform_admin.grant` and `platform_admin.revoke`
  (`internal/auth/oidc.go`), the platform trace destination and the platform trace ceiling
  (`internal/tracedest/platform.go`: "the log has no org-less events (ADR 0023)").
- **Not audited at all.** The first admin's creation (`internal/auth/bootstrap.go`), `keys add`,
  `activate`, `rewrap`, `reencrypt` and `retire` (`internal/keyscli`), the KEK that `koinon-api`
  generates on its first start (`docs/ops/secret-keys.md`), the Secrets the ensure-secrets Job
  generates, and the platform settings Helm sets.

The copies cost more than they save. Nothing is recorded before the first admin exists, which is
exactly when an install is set up. N admins mean N copies. A revoked admin keeps the platform
history in their personal org, and a new admin's personal org lacks everything before them. Each
new install-wide action has to solve the problem again.

ADR 0023 also leaves a known gap: a database superuser, or the table owner, can rewrite a whole
chain, head included, and `koinon-api audit verify` still passes. External anchoring or a key held
outside the database was deferred.

Prior art (research report of 2026-11-07, kept outside the repository): GitLab separates instance
audit events from group and project events and shows them to administrators only; GitHub Enterprise
keeps an enterprise log across all organisations for enterprise owners and streams it as JSON; Coder
gives the log to an Auditor role. None of them make the log tamper-evident. Certificate
Transparency (RFC 9162) and the C2SP signed-note checkpoint format show how a signed head lets an
outsider check a log without trusting its host.

## Decision

All five points are maintainer decisions of 2026-11-07.

### 1. A separate install chain

The install gets its own audit chain, next to the org chains, and does not replace them.

**Tables** (one forward-only goose migration, `internal/db/migrations/NNNNN_install_audit_chain.sql`,
numbered when it is written):

```sql
CREATE TABLE install_audit_events (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),  -- the generator org audit_events uses
    chain_seq   bigint NOT NULL UNIQUE,
    prev_hash   bytea NOT NULL,
    hash        bytea NOT NULL,
    actor_kind  text NOT NULL CHECK (actor_kind IN ('user','token','agent','secret','system','cli')),
    actor_id    text NOT NULL CHECK (actor_id <> ''),
    action      text NOT NULL,
    target_kind text NOT NULL,
    target_id   text NOT NULL,
    summary     jsonb NOT NULL,
    request_id  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE install_audit_chain_head (
    singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    chain_seq  bigint NOT NULL,   -- 0 before the first event
    hash       bytea NOT NULL     -- 32 zero bytes before the first event
);
-- the migration inserts the one head row
```

The table is keyed by `id` alone. It has no `org_id` and no foreign key to `orgs`, so it is not
reached by `TRUNCATE orgs CASCADE` or by an org's deletion. The columns are those of `audit_events`
without `org_id`, so one Go encoder and one verify loop serve both kinds of chain (decision 1.3).
The convention "tables owned by an org are keyed by `(org_id, id)`" (AGENTS.md) does not apply: no
org owns these rows.

**1.1 Guards, copied from migration `00019`.** A `BEFORE INSERT` trigger
`install_audit_events_chain` (`SECURITY DEFINER`, fixed `search_path`) takes `FOR UPDATE` on the head
row, sets `chain_seq = head.chain_seq + 1`, `prev_hash = head.hash`, `hash =
install_audit_event_hash(NEW)`, and moves the head. Values a writer supplies are overwritten. A
row trigger refuses `UPDATE` and `DELETE` on both tables, and a statement trigger refuses
`TRUNCATE` for every role, the owner included. `koinon_app` gets `SELECT` on both tables and nothing
else; `INSERT` on `install_audit_events` is granted to the login role `koinon_api` only (when the
role exists, as `00019` does for `koinon_app`), because only `koinon-api` writes this chain.
`koinon-reconciler` and `koinon-agentd` cannot. `docs/ops/secret-keys.md` ("Append-only audit log
with an external database") gains the matching `REVOKE` and `GRANT` lines for `postgres.mode:
external`. Test resets that truncate it use `session_replication_role = replica`, as
`pgtest.TruncateOrgs` does.

**1.2 Writer and transaction.** `koinon-api` writes the chain: its API handlers, its scheduler and
its CLI subcommands run in its pod (`kubectl exec deploy/koinon-api -- koinon-api keys ...`). The
Go entry point is `audit.RecordInstall(ctx, q, audit.InstallEvent{...})`, called with the
transaction of the change it records. A failed write fails the change, as `audit.RecordErr` does
for org events. It uses the same summary scrubber (`ErrSecretInSummary`) and request ID. Key
material appears only as a KEK ID and fingerprint; a Secret only by name.

**1.3 Hash input (version 1).** `hash = sha256(canonical_row || prev_hash)`, with the field
encoding of the org chain: each field is an 8-byte big-endian byte length followed by its UTF-8
bytes, and `prev_hash` is appended as 32 raw bytes (32 zero bytes for `chain_seq` 1). The fields,
in order:

1. the tag `koinon.install_audit.v1`;
2. the install ID from `install_identity`, as lower-case UUID text (in the place the org chain puts
   `org_id`, so an exported install chain cannot be passed off as another install's);
3. `id` as lower-case UUID text;
4. `chain_seq` in decimal;
5. `actor_kind`, `actor_id`, `action`, `target_kind`, `target_id`;
6. `summary` as the text Postgres prints for the `jsonb` value;
7. `request_id`;
8. `created_at` as decimal microseconds since the Unix epoch.

The SQL function `install_audit_event_hash` and the Go function that the verifier uses implement it,
and a test checks that they agree, as for `audit_event_hash` and `audit.RowHash`. The Go side
generalises `RowHash` over a row type that yields the scope ID (org or install) and the shared
fields, and takes the tag as a parameter; the org chain's bytes do not change. A change to the
encoding is a new tag and a new migration.

Fields the org chain has no column for go in `summary`, so the two encoders stay identical apart
from the tag and the scope ID: `summary.reason` (the reason a person gave, when the action takes
one), `summary.koinon_version` (the build that wrote the entry), and, on an org event caused by an
install event, `summary.install_event_id`.

**1.4 Before any platform admin.** The chain needs no person: the first admin's creation is itself
an install event (decision 3), and a system actor (decision 2) writes everything that happens
before. `auditPlatform` and the platform part of `keys forget` stop refusing when there is no
platform admin, and the `slog`-only path goes away.

**1.5 One record per action.** Each install-wide action is written once, in the install chain. The
copies into platform admins' personal orgs end at the cutover. Rows already written in personal orgs
stay where they are (the chains are append-only).

**1.6 Cause and effect across scopes.** An install-level cause goes in the install chain; its
effects in an org stay in that org's chain, each with `summary.install_event_id` pointing back.
Example: lowering the platform trace ceiling is one `trace.platform_ceiling.set` install event and
one `trace.settings.lowered` event in each org whose level was lowered. Effects that are already per
org stay so: `trace.settings.lowered`, `keys.forget` for each org's values,
`home.repository_replaced`, `model.format_rederived`, org prices, and `admin_api.call` (an admin API
token is bound to an org).

**1.7 Lock order.** A transaction that writes both kinds of event writes the install event first,
then its org events in org ID order. This extends the ADR 0023 lock order (org heads in org ID
order) with one head that always comes first, so two such transactions cannot deadlock. A
transaction that has already written an org event must not write an install event. Every install
write serialises on the one head; the volume is small (a few rows on a quiet day, one per price
version, one per key operation), and none of these transactions is long.

### 2. Every entry has an actor; system actors are named by component

Actor kinds stay the six of migration `00056` (`user`, `token`, `agent`, `secret`, `system`,
`cli`). A `system` actor's ID names the component and the function that acted, as
`<component>:<function>`; it is never empty in the install chain (the `CHECK` above). The first
vocabulary:

| Actor ID | What acts |
| --- | --- |
| `koinon-api:bootstrap` | the start-up step that creates the first admin and the default org |
| `koinon-api:startup` | start-up observation of Helm-set settings and keys (decision 3) |
| `koinon-api:keys` | key generation by `koinon-api` itself (the first KEK, the saved-work key) |
| `koinon-api:oidc-sync` | the platform admin grant and revoke from IdP groups at login (today ID `oidc`) |
| `koinon-api:price-refresh` | the price refresh and its bounded auto-apply (ADR 0098 decision 3) |
| `koinon-api:audit-anchor` | the anchor and checkpoint job (decision 4) |

`audit.System()` takes the actor ID as an argument, so no caller can write an unnamed system actor.
A `cli` actor keeps the command as its ID (`keys forget`).

**Alignment with ADR 0098.** ADR 0098 calls the auto-apply's actor `auto`; the code on PR #316 is
`AutoActor = audit.Actor{Kind: system, ID: "auto"}`. Under this ADR its ID is
`koinon-api:price-refresh`, and ADR 0098's "actor a user or `auto`" reads "a user, a `cli` actor
or `system koinon-api:price-refresh`". The platform price tables' actor column stores the same text
the audit log shows (`Actor.String()`), so the version history and the audit event name the actor
the same way. The edit to ADR 0098's wording lands with the first implementation issue, which also
touches #316's code, so the two PRs do not both edit that ADR.

Org chains follow the same rule from the second implementation issue on: the `system` writers in
`koinon-reconciler` and `koinon-agentd` get component IDs (`koinon-reconciler:sandbox-repair`,
`koinon-agentd:home-seed`, ...), and a `CHECK (actor_kind <> 'system' OR actor_id <> '') NOT VALID`
on `audit_events` holds new rows to it without touching old ones.

### 3. Scope for v0.1: everything install-wide

| Action (install chain) | Today | Actor |
| --- | --- | --- |
| `platform_price.set`, `platform_price.rollback`, `platform_price.retired`, `price.proposal.accepted`, `price.proposal.dismissed`, `price.refresh.settings_changed` (ADR 0098 decision 5) | copies per admin, or `slog` only (#316) | user, `cli` or `system koinon-api:price-refresh` |
| `platform_admin.grant`, `platform_admin.revoke` | the user's personal org | user, or the OIDC group sync (today `system oidc`, renamed `koinon-api:oidc-sync`) |
| `platform_admin.bootstrap` (the first admin's creation, with the default org's ID as target) | not audited | `system koinon-api:bootstrap` |
| `trace.platform_destination.set`, `.delete`, `.test`; `trace.platform_ceiling.set` | the acting admin's personal org | user |
| `keys.add`, `keys.activate`, `keys.rewrap`, `keys.reencrypt`, `keys.retire`, `keys.forget` (its platform part; each org's part stays per org) | `keys.forget` copies per admin; the rest not audited | `cli` |
| `keys.generated` (a KEK or the saved-work key `koinon-api` makes) | not audited | `system koinon-api:keys` |
| `keys.observed` (a key or generated Secret `koinon-api` sees for the first time, by ID and fingerprint or by name and UID) | not audited | `system koinon-api:startup` |
| `platform.settings.observed` (Helm-set platform settings that differ from the last recorded ones) | not audited | `system koinon-api:startup` |

**Keys.** Making a key is a Kubernetes API call, not a database write, so it cannot share a
transaction with its event. `koinon-api` writes `keys.generated` right after it created the Secret,
and on every start writes `keys.observed` for any KEK ID or generated Secret it has no event for,
which also catches a crash between the two and keys made outside it (`secrets.kek.existingSecret`,
the ensure-secrets Job, which runs as a Helm hook before the database may exist). A KEK is named by
ID and fingerprint (as `keys status` prints it); another generated Secret by name, UID and creation
time, never by a hash of its value. `keys.*` events from the CLI are written in the transaction of
the database change they make (`rewrap`, `reencrypt`, `forget`) or, for a Secret-only change
(`add`, `activate`, `retire`), right after the Secret update succeeded, with the KEK ID and
fingerprint before and after.

**Helm-set settings.** `koinon-api` builds, on start, a fixed list of install-wide settings from its
flags (price refresh mode, bootstrap login on or off, OIDC issuer and group mappings, the platform
trace content maximum, retention maximums, isolation tier defaults, the chart and image version;
names of Secrets, never their contents). In one transaction under a fixed advisory lock (so replicas
starting together write once) it compares the list with the `after` of the newest
`platform.settings.observed` event and, when they differ, writes one event with the changed keys in
`before` and `after`. Settings an admin changes through the API are audited by that API call, not
here. The list is a Go table next to the flags; adding a flag means deciding whether it is on it,
and a test lists the flags that are not, so none is forgotten.

### 4. Anchors and signed checkpoints: designed now, built after v0.1

The format above is version 1 and stays as it is when these arrive; they are new install events and
new tables, not a new encoding.

**4.1 Anchors.** On a schedule (default hourly, and on demand by `koinon-api audit anchor`), the
`system koinon-api:audit-anchor` actor writes one `audit.anchor` install event. In the same
transaction it reads every row of `audit_chain_heads` (a plain read; the anchor states "at least
these heads") and inserts them into an append-only side table:

```sql
CREATE TABLE install_audit_anchor_heads (
    anchor_event_id uuid   NOT NULL REFERENCES install_audit_events (id),
    org_id          uuid   NOT NULL,  -- no FK: a deleted org's last head stays anchored
    chain_seq       bigint NOT NULL,
    hash            bytea  NOT NULL,
    PRIMARY KEY (anchor_event_id, org_id)
);  -- same UPDATE, DELETE and TRUNCATE guards
```

The event's summary holds `org_count` and `heads_root`: sha256 over the encoded `(org_id, chain_seq,
hash)` triples in org ID order, each field length-prefixed as in decision 1.3. The verifier
recomputes the root from the side table and checks that each org chain still has the anchored hash
at the anchored `chain_seq`. An org chain rewritten or rolled back behind an anchor then fails, as
long as the install chain is intact.

**4.2 Signed checkpoints.** After each anchor, `koinon-api` signs the install head. The checkpoint is
a C2SP signed note:

```text
<origin>                  e.g. koinon.dev/install/<install id>/audit (domain from internal/project)
<chain_seq of the install head>
<base64 of the install head hash>

— <key name> <base64 of key hash + Ed25519 signature>
```

The key is an Ed25519 key in a Secret that only `koinon-api` mounts (`<release>-audit-signing`,
created by `koinon-api` on its first start like the KEK Secret, never by Helm, never overwritten, and
never mounted by the reconciler or agentd). Neither existing key fits: `koinon-agentd` mounts the
saved-work HMAC key and `koinon-reconciler` mounts the KEKs (`docs/ops/secret-keys.md`). Its public key is published by an install event
`audit.signing_key.added`; a rotated key keeps its old public keys for verifying older checkpoints.
A signature rather than an HMAC, so that a verifier needs only the public key. The checkpoint is
recorded as an `audit.checkpoint` install event and written to the export sink (decision 5).

Because the install head commits to the anchor, and the anchor to every org head, one exported
checkpoint covers every chain up to that point. A superuser who rewrites any chain, head included,
can no longer produce a history that matches a checkpoint already held outside the cluster. This
closes the ADR 0023 superuser gap and its open question on HMAC custody or external anchoring. A
compromised `koinon-api` can still sign a false checkpoint for the future; external witnesses or an
RFC 3161 timestamp are a later step.

### 5. Read, export and retention

**Read.** Only platform admins read the install chain. `AuditService.List` gains a `scope` field
(`AUDIT_SCOPE_ORG`, the default and today's behaviour, or `AUDIT_SCOPE_INSTALL`); install scope
requires the caller to be a platform admin and an empty `org_id`, and takes the same filters
(action prefix, actor, since, until, paging). The admin API follows: `GET
/admin/v1/audit?scope=install`, under the same rule. The reply's `Event` gains `chain_seq`, and an
install event has no `org_id`. Org admins and owners never see the install chain; they see the
effects in their own chain, with the pointer back. A read-only platform auditor role and a web page
may come later.

**Verify.** `koinon-api audit verify` checks the install chain as well as the org chains by default
(`--install` for the install chain alone, `--org <id>` as today), and once anchors exist also checks
every anchor against the org chains.

**Export.** `koinon-api audit export --scope install|org [--org <id>] [--from-seq N]` and `GET
/admin/v1/audit/export` (same scope rules as the read) write JSON lines: a header line
`{"format":"koinon.audit.export.v1","chain":"install"|"org","install_id":...,"org_id":...,
"head":{"chain_seq":...,"hash":...}}`, then one line per event in `chain_seq` order with every
hashed field exactly as hashed (`summary_text` is Postgres's text of the `jsonb`, and `summary` is
the parsed object for convenience), `prev_hash` and `hash` in hex, and `created_at_micros`. A copy
verifies off the cluster with `koinon-api audit verify --file <export> [--checkpoint <note>
--public-key <key>]` (the binary runs anywhere, with no database), or with any tool that implements
decision 1.3, which the export docs spell out. A partial export starts from `--from-seq` and its
first `prev_hash` is checked against an earlier export or a checkpoint. Streaming to object storage
or a webhook (at least once, `id` for de-duplication) is later work; checkpoints ride the same sink.

**Retention.** Nothing is deleted in v1, from either chain kind; the triggers refuse it. Audit
retention (#29) is designed once for both kinds: a prefix of a chain may only be dropped behind a
recorded, and once built signed, checkpoint, which the verifier then treats as the chain's start.
Run-content retention (ADR 0023, 2026-10-31 amendment) still never touches audit.

**Metrics** (fixed labels, ADR 0023 label rule; no org, user, key or actor ID):

- `koinon_audit_writes_total{chain="org"|"install", result="ok"|"error"}`;
- once built, `koinon_audit_anchor_age_seconds` and `koinon_audit_checkpoint_age_seconds`
  (gauges, no labels), so an alert can say anchoring stopped.

## Alternatives considered

| Option | Gains | Costs, now and later |
| --- | --- | --- |
| **Keep the per-admin copies** (status quo, and #316 as written) | No work; the swap later is one function | Everything before the first admin stays out of audit for good, which is the setup period. N copies per action, a revoked admin keeps the history, a new one lacks it, and every new install-wide action re-solves this. Install events accumulate in personal orgs that an install chain would later have to explain. |
| **A hidden platform org** holding install events in `audit_events` | No new audit schema | A fake org leaks into every org listing, budget, deletion, cascade, membership and role check ("org admin of the platform org"); every org-iterating path needs an exception for good. Moving off it later breaks its chain. |
| Nullable `org_id` in `audit_events` | One table | A primary key cannot contain a nullable column, so `(org_id, id)` becomes a rebuild, against the keying convention; the trigger and verifier grow special cases. |
| One global chain for every event | One verify | Every org's writes serialise on one lock, and an org can no longer verify or export its own chain alone; ADR 0023 chose per-org chains for this reason. |
| HMAC instead of signed checkpoints | Simpler key | A verifier would need the secret, so the auditor who checks can also forge. |

## Consequences

- Positive: every install-wide change is recorded once, from the install's first start, whoever is
  or is not a platform admin. #305 can merge with platform prices audited.
- Positive: org chains, their encoding and their verification do not change; org admins see the
  same log as before, plus pointers to install causes.
- Positive: the install chain is the anchor that later closes the superuser-rewrite gap for all
  chains at once, and the version 1 format already allows it.
- Negative: a second table, trigger, hash function and verify path to keep in step with the first;
  the shared Go encoder and a SQL/Go agreement test for each tag contain it.
- Negative: one more lock order rule (install head first) that code review must hold; a test that
  writes both kinds in one transaction checks it.
- Negative: Secret-only key operations are recorded right after the Secret change, not in one
  transaction with it; the start-up `keys.observed` pass covers a crash in between, at the cost of
  an event that says "observed" rather than "generated".
- Negative: the existing platform rows in personal orgs stay there, so the history of platform
  actions before the cutover is split between personal orgs and the install chain. There is no
  schema promise before v0.3 (ADR 0099), so no backfill is owed; the cutover release notes say where
  the older rows are.

## Implementation plan

Each item is its own issue; the first blocks #316 and #305.

1. **Install audit chain, and platform prices in it.** Migration (tables, hash function, triggers,
   grants), `audit.RecordInstall`, the shared encoder, `audit verify` over both kinds, the
   `koinon_audit_writes_total` metric, and `docs/ops/secret-keys.md`'s external-database lines.
   Replace `auditPlatform` on #316 (or a follow-up that lands before #305) with install events,
   rename `AutoActor` to `system koinon-api:price-refresh`, and align ADR 0098's actor wording.
2. **Named system actors and the remaining install actions.** `audit.System(id)`; component IDs for
   every system writer, the org chain's `NOT VALID` check; `platform_admin.grant/revoke/bootstrap`,
   platform trace destination and ceiling, and `keys forget`'s platform part move to the install
   chain, with `install_event_id` on their org effects. Retire `ListPlatformAdminPersonalOrgs` as an
   audit target.
3. **Key administration and key generation.** `keys.add/activate/rewrap/reencrypt/retire/forget`,
   `keys.generated` and the start-up `keys.observed` pass, with RBAC for reading generated Secrets'
   metadata if needed (chart smoke test).
4. **Helm-set settings.** The settings list, `platform.settings.observed` under an advisory lock,
   and the test that lists flags left off it.
5. **Read and export.** `AuditService.List` scope, `GET /admin/v1/audit?scope=install`, JSON lines
   export (CLI and admin API), `audit verify --file`, and the export format in the docs site.
6. **Anchors** (after v0.1). `install_audit_anchor_heads`, the hourly job, verifier checks, anchor
   age gauge.
7. **Signed checkpoints** (after v0.1). The signing Secret, `audit.signing_key.added`,
   `audit.checkpoint`, the C2SP note, off-cluster verification, checkpoint age gauge, rotation.

Retention stays with #29 and is designed against decision 5.

## Open questions

1. Does `koinon-api` need new RBAC (`get` on the named ensure-secrets Secrets) to record them in
   `keys.observed`, or should the ensure-secrets Job leave its result where `koinon-api` can read it
   without that? (Issue 3.)
2. Should anchors (decision 4.1) ship in v0.1 after all? They need no key and close rollbacks of org
   chains behind the install chain; the maintainer decision puts them after v0.1 with checkpoints.
3. Which of today's start-up flags belong on the Helm-settings list (decision 3) is decided in issue
   4; the list above is a first cut.

## Links

- Issue [#324](https://github.com/stateracloud/koinon/issues/324); PR #316 (#265, live prices);
  #305; audit retention #29.
- ADR [0023](0023-observability-and-audit.md) (per-org chains, encoding, label rule),
  [0098](0098-price-catalogue-updates-from-public-sources.md) decision 5 (price audit events).
- RFC 9162 (Certificate Transparency 2.0): https://www.rfc-editor.org/rfc/rfc9162.html
- GitLab audit events by scope: https://docs.gitlab.com/user/compliance/audit_event_types/
- GitHub Enterprise audit log streaming:
  https://docs.github.com/en/enterprise-cloud@latest/admin/monitoring-activity-in-your-enterprise/reviewing-audit-logs-for-your-enterprise/streaming-the-audit-log-for-your-enterprise
- Coder audit logs: https://coder.com/docs/admin/security/audit-logs
