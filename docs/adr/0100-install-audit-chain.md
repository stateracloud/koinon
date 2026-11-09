# 0100. An install-level audit chain for platform actions

- Status: Accepted (maintainer decision 2026-11-07; anchors in v0.1, signed checkpoints after v0.1)
- Date: 2026-11-07
- Amends: [0023](0023-observability-and-audit.md) (the 2026-10-26 amendment's "no org-less events
  and no platform chain"; the lock order; the deferred superuser item and open question 1)
  and [0004](0004-human-identity.md) (where platform admin grants are audited)
- Relates to: [0056](0056-model-providers-pricing-and-budgets-per-org.md),
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
    chain_seq   bigint NOT NULL UNIQUE CHECK (chain_seq >= 1),
    prev_hash   bytea NOT NULL CHECK (length(prev_hash) = 32),
    hash        bytea NOT NULL CHECK (length(hash) = 32),
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
    seq        bigint NOT NULL CHECK (seq >= 0),          -- 0 before the first event
    hash       bytea  NOT NULL CHECK (length(hash) = 32)  -- 32 zero bytes before the first event
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
row, sets `chain_seq = head.seq + 1`, `prev_hash = head.hash`, `hash =
install_audit_event_hash(NEW)`, and moves the head. Values a writer supplies are overwritten. A
row trigger refuses `UPDATE` and `DELETE` on both tables, except, on the head only, an update made
from inside the chain trigger (`pg_trigger_depth() > 1`, as `audit_chain_heads_guard` in `00019`
allows), so the chain trigger can move the head and nothing else can. A statement trigger refuses
`TRUNCATE` on both tables for every role, the owner included. Test resets that truncate them use
`session_replication_role = replica`, as `pgtest.TruncateOrgs` does.

**1.1a Who may insert.** Only the `koinon-api` binary writes this chain (decision 1.2);
`koinon-reconciler` and `koinon-agentd` cannot. The grant cannot name the login role `koinon_api`
directly: with the chart's CloudNativePG setup that role is a managed role the operator creates
asynchronously, and the migrate Job may already have run by then. Instead:

- the chart's `postInitApplicationSQL` creates a `NOLOGIN` group role
  `koinon_install_audit_writer` next to `koinon_app`, so it exists before any migration runs;
- the `koinon_api` managed role lists it in `inRoles` (`[koinon_app, koinon_install_audit_writer]`);
  the reconciler and agentd roles do not;
- the migration `REVOKE`s `INSERT, UPDATE, DELETE, TRUNCATE` on both tables from `koinon_app`
  (which the default privileges would otherwise give it) and `GRANT`s `SELECT` on both to
  `koinon_app`. It does not touch the group role. The chart also lists the group role as a CNPG
  managed role (`ensure: present`, `login: false`), so a cluster initialised before this change gets
  it from the operator;
- the grants to the group role (`INSERT` on `install_audit_events`, `SELECT` on both) are an
  idempotent step of `koinon-api migrate` after goose (`migrations.GrantInstallAuditWriter`), not part
  of the migration. A Helm `pre-upgrade` hook runs before the updated Cluster reaches the API server,
  and the migrate role has no `CREATEROLE`, so a migration that needs the role would fail on an
  existing cnpg install and abort the upgrade before the role could appear, on every retry. When the
  role is missing the step logs a warning and grants nothing. New installs have the role from
  `postInitApplicationSQL` and never see this;
- in cnpg mode the chart adds a `post-upgrade` hook Job (`templates/install-audit-grants.yaml`; Argo
  CD: `PostSync`) that runs `koinon-api migrate --grants-only` as the migrate role: no migration, only
  the grant step, polled until the role and the tables exist, within `migrations.wait`. Helm runs it
  after the release's objects, so after the updated Cluster, and with `--wait` once the pods are
  ready. Running out of time fails the hook, so the upgrade reports a role CNPG never created. One
  `helm upgrade` (or sync) is enough (maintainer decision 2026-11-07, after review: an earlier
  version of this decision asked for a second `helm upgrade`). The hook waits only for the role to
  exist, not for `koinon_api`'s membership in it, which CNPG adds on its own schedule and may add
  after the hook succeeded; that is harmless, because `koinon-api` retries every refused install
  write (next item) until the membership lands. `migrations.wait` (default 4m) stays below Helm's
  default `--timeout` (5m), so a role CNPG never created fails the hook with its own message;
- with `postgres.mode: external` nothing creates the role after the migrate Job, so the chart passes
  `koinon-api migrate --require-install-writer` and a missing role fails the Job, naming the role and
  `docs/ops/secret-keys.md`, instead of `koinon-api` logging a refused write every 30 seconds with no
  hook to end it (maintainer decision 2026-11-07, after review);
- `koinon-api` never needs the grant to start, or the post-upgrade hook could never run under
  `--wait`: an install event the database refuses for lack of privilege (SQLSTATE 42501, wrapped as
  `audit.ErrInstallWriteDenied`) at startup (the price refresh setting, the retirement of caught-up
  platform prices) is retried in the background every 30 seconds until the grant lands. Each startup
  write (the bootstrap, the retirement of caught-up platform prices, the price refresh setting and the
  key-set check) logs one error line when it is first refused and one more on each change of state. Requests that write install events (platform price changes) fail with the permission
  error in that window, which is the hook's runtime. A setting whose event is not recorded does not
  take effect where it would widen what runs: the scheduled price refresh runs each tick in the
  recorded setting (`price_refresh_settings`) wherever that fetches less, less often or applies
  less than the configured one (`prices.Refresher.Effective`: the mode, the interval and both
  auto-apply bounds, which the row records with the same event), so a change to `on` or to a looser
  bound runs only from the first tick after its `price.refresh.settings_changed` event is written. A
  change that does less (to `off`, a tighter bound) applies at once. The same rule keeps an old replica of a rolling upgrade to what a new one recorded
  (maintainer decision 2026-11-07, after review);
- `koinon-api` checks its own membership, at startup and every 30 seconds on its own pool
  (`SELECT current_user, pg_has_role(current_user, 'koinon_install_audit_writer', 'USAGE')`, a
  missing role counting as not a member; `audit.InstallWriterCheck`). On a missing role or
  membership it logs one error per change of state with the statement for its own login filled in
  (`GRANT koinon_install_audit_writer TO "<current_user>"`, the identifier quoted) and sets the gauge
  `koinon_install_audit_writer_ok` (no labels, ADR 0023) to 0, and never fails readiness; the
  documented alert fires on the gauge as well as on refused writes, so a missing grant shows before
  anything writes. `koinon-reconciler` and `koinon-agentd` warn at startup when their own login can
  write the chain (a shared login). The check runs in the API, not in the migrate Job and not from a
  chart value naming the api login (maintainer decision 2026-11-07, after research): the api login
  already lives in the api URI Secret, and a second copy in a value drifts; the name in that value is
  wrong where the server sees another role than the URI names (a Cloud SQL group login connects as
  its own IAM user, an Azure Entra group connects as the group role, PgBouncer can force `user=`);
  and under workload identity the migrate Job runs as another ServiceAccount and cannot log in as the
  API. Only the process that connects knows its `current_user`. Cloud IAM token logins (RDS IAM,
  Entra) are out of scope here (#353).

With `postgres.mode: external` the roles are the operator's. `docs/ops/secret-keys.md` ("Append-only
audit log with an external database") gains the lines to run once as the schema owner:

```sql
CREATE ROLE koinon_install_audit_writer NOLOGIN;
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON install_audit_events, install_audit_chain_head FROM <app role>;
GRANT SELECT ON install_audit_events, install_audit_chain_head TO <app role>;
GRANT koinon_install_audit_writer TO <koinon-api login role>;
```

and says plainly that when `koinon-api`, `koinon-reconciler` and `koinon-agentd` share one login
role, that role holds the grant and the separation is lost: the reconciler and agentd could then
append install events. The chain still detects edits and removals; only who may append is weaker.

**1.2 Writer and transaction.** The `koinon-api` binary writes the chain, as the `koinon_api`
database role: its API handlers, its scheduler, its CLI subcommands run in its pod (`kubectl exec
deploy/koinon-api -- koinon-api keys ...`), and the key-admin Job (`charts/koinon/templates/
key-admin.yaml`, `koinon-api key-admin`, only with a KMS), which creates the KEK Secret, adds a
KEK and, with `activateIfNoActive`, activates one. The Go entry point is the audit batch
(decision 1.7): `audit.InTx` or `audit.InPgTx`, whose `b.Install(audit.InstallEvent{...})` writes
the event in the transaction of the database change it records. A failed write fails the change,
as it does for the org events of the same batch (`b.Org`). Where the change is not a
database change (a Secret update) or is spread over many transactions (decision 3, keys), the
event is written right after it and is not atomic with it; decision 3 says how a lost event is
caught. Key material appears only as a KEK ID and fingerprint; a Secret only by name.

**1.2a The event type.** `audit.Event` today fills only `before` and `after` into the summary. Both
`audit.Event` and the new `audit.InstallEvent` gain `Reason`, `KoinonVersion` (filled by
the audit batch and `audit.RecordAfter` from the build, not by callers) and, on `Event`, `InstallEventID`; they
land in the summary as `reason`, `koinon_version` and `install_event_id`. Every summary key still
passes the same scrubber (`ErrSecretInSummary`) and the request ID is taken from the context as
today. Keys must not look like a secret value to the scrubber: a Secret is named under
`secret_name` (allowed), never under a bare `secret`, which is refused.

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

**1.7 Lock order.** The audit heads are the last locks a transaction takes. A transaction first
takes its domain locks (the platform price lock `LockPlatformPrices`, `LockPlatformTraceSettings`,
the key-set advisory lock of decision 3, row locks on what it changes), then writes the install event, then its org events in org ID order.
The audit module holds that order, not the caller (amended 2026-11-08, below). This extends the ADR 0023 lock order (org heads in org ID order) with one head that always comes
first among the heads, so two such transactions cannot deadlock. A transaction that has already
written an org event must not write an install event. Every install write serialises on the one
head; the volume is small (a few rows on a quiet day, one per price version, one per key
operation), and none of these transactions is long.

**Amendment 2026-11-08: the audit batch.** A rule that only a comment and code review hold is broken
by the next caller: six paths kept it by hand (bootstrap's flush closure, `LowerOrgTraceLevels`'s
`ORDER BY org_id`, the OIDC sync, platform prices, `keys forget`, writers of several events), and
`audit.Record` trusted that its `q` belonged to the change's transaction. So the module owns the order:

- The module opens the transaction (`audit.InTx`, or `audit.InPgTx` for a caller that needs the pgx
  transaction) and hands the caller its queries and an **audit batch**. The caller adds events to the
  batch as the change happens, in any order; just before commit the module writes the install events
  in the order added, then the org events sorted by org ID (stable within an org). No caller chooses
  the order, so two transactions that audit orgs A,B and B,A take the heads in the same order and
  cannot deadlock. One caller keeps its own transaction: the key-set change (decision 3) holds one
  transaction across a Secret update, so it takes its advisory lock first and writes each event through
  `audit.WriteInTx` on that transaction, which writes in the same order.
- A batch may hold several install events (platform prices writes one per decision and one per
  version in one transaction). An org event links to the install event of its transaction through
  `install_event_id` in its summary (decision 1.6); the link means "written in the same transaction",
  not "caused by". With exactly one install event in the batch, an org event that leaves the ID unset
  gets that event's ID; an ID the caller set is kept. With several, the module does not guess: an org
  event with no ID is refused (`audit.ErrAmbiguousInstallLink`) and the transaction rolls back. No
  caller mixes several install events with org events today; a way to name one install event of the
  batch is added when the first caller needs it.
- An event commits with its change or not at all: a refused write (an unnamed actor, a summary that
  looks like a secret, an org event with no org, an ambiguous install link, a database refusal such as
  `ErrInstallWriteDenied`)
  fails the function and rolls the whole transaction back. A caller that needs a stored event reads it
  from the result, after the commit.
- The domain locks still come first, because the caller takes them while it makes its change and the
  batch is written after the function returns. The key-set check of decision 3 holds its advisory lock
  before the install head for the same reason.
- An event that is recorded after the work, not with it (the reconciler's, the admin API's call log,
  failed token checks), goes through `audit.RecordAfter`, whose name says it is not atomic. It must not
  run inside a transaction that holds an audit head.
- The direct writes `audit.Record` and `audit.RecordInstall` are gone (step 3, #371). The compiler
  enforces that only `internal/audit` writes: the sqlc inserts of the audit tables are generated into
  `internal/audit/internal/auditstore` (a second `sql` entry in `sqlc.yaml`), which Go's internal rule
  makes unreachable from any other package; the read queries stay in `internal/db/store`.
  `hack/audit-batch-lint.sh`, run by `make lint`, preflight and CI, covers what the compiler cannot see:
  it fails any code outside `internal/audit` that writes (INSERT/UPDATE/DELETE) into the audit tables
  with raw SQL, or names one in a string literal of its own (dynamic SQL, `pgx.Identifier`), and limits
  `audit.WriteInTx` to `internal/keyscli`. A test that must break the tables on purpose marks the one
  statement with `// audit-lint:allow <reason>` on the line above.
- `audit.Fake` is an in-memory runner that orders and validates exactly as the Postgres one does, so a
  caller's unit test can check what it audited. One spec suite runs on both; only the Postgres specs
  cover the hash chains and the locks.

Two paths broke the rule before this amendment and issue 2 fixed them: `SetPlatformCeiling`
(`internal/tracedest/platform.go`) wrote each org's `trace.settings.lowered` before the platform
event, and had to write the install event first (the audit batch now holds that order); and
`LowerOrgTraceLevels` (`internal/db/queries/trace.sql`) returned the lowered orgs in no fixed order,
and gained `ORDER BY org_id` for the row locks, while the audit batch orders the events. The second also means ADR 0023's line "no
current path audits two orgs in one transaction" is wrong; the 2026-11-07 note there corrects it.

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
| `koinon-api:key-admin` | the key-admin Job: KEK Secret creation, `addKey`, `activateIfNoActive`, install binding |
| `koinon-api:keyring-watch` | the lost-change check `koinon-api` runs when its keyring watch sees the key set change (decision 3) |
| `koinon-api:oidc-sync` | the platform admin grant and revoke from IdP groups at login (today ID `oidc`) |
| `koinon-api:price-refresh` | the price refresh and its bounded auto-apply (ADR 0098 decision 3) |
| `koinon-api:audit-anchor` | the anchor and checkpoint job (decision 4) |
| `koinon-api:model-formats` | the start-up re-derivation of model formats, in each org's chain (`model.format_rederived`) |
| `koinon-api:code-host-scope` | the code host connection scope sync, in each org's chain (`org.code_host_scope.replace`) |

`audit.System()` takes the actor ID as an argument, so no caller can write an unnamed system actor.

Rollout window (maintainer decision 2026-11-07): the CHECK `(actor_kind <> 'system' OR actor_id <> '') NOT VALID` on
`audit_events` does not land with the named actors. It lands in #359, one release after them (expand, then
contract). The reason is that v0.1.0-rc.9 `audit.System()` wrote an empty ID, and the old pods keep running
during a rolling upgrade. A CHECK in the same release would make their system audit inserts fail. The writers
inside a transaction would roll back and be retried on the next pass: agentd stuck-run and sandbox-wait
`agent.mark_unhealthy`, `mcp_tool.sync` and `mcp_server.health`, `approval.expire`, `question.retarget`, and
`question.answer`, `question.expire` and `question.dismiss` when the system resolves a question
(`internal/questions`, `questions.go:429` at v0.1.0-rc.9). The reconciler writes `sandbox.recreate`,
`sandbox.delete`, `sandbox.archive` and `agent.mark_unhealthy` after the work they record, outside a
transaction, so those events would be lost, and for `sandbox.archive` the pass would also abort before it marks
the sandbox archived. `token.use_failed` is best effort and would be dropped. With the CHECK one release later,
every pod that can write an unnamed system actor is gone before it applies.
A `cli` actor keeps the command as its ID (`keys forget`).

**The ID format is frozen** (maintainer decision 2026-11-07, after review): `<component>:<function>`,
both lower-case kebab-case; the component is the binary that acts (`koinon-api`, `koinon-reconciler`,
`koinon-agentd`), the function a short noun for what acted, unique within the component. An ID is
never renamed once written, since the audit log, its exports and the price tables' actor column keep
it; a new system writer takes a new ID in this format, and one that does the same work as an
existing writer reuses its ID. The IDs written today (`audit.System(` callers), beyond the table
above:

| Component | Functions |
| --- | --- |
| `koinon-api` | `approvals`, `questions`, `question-timers`, `agent-model`, `token-use` |
| `koinon-reconciler` | `sandbox-repair`, `sandbox-archive`, `ephemeral-cleanup`, `mcp-sync` |
| `koinon-agentd` | `home-seed`, `resync`, `run-outcome`, `stuck-run`, `prefix-taken` |

Tests use the component `koinon-test` (`koinon-test:spec`), which no binary writes.

**Alignment with ADR 0098.** ADR 0098 calls the auto-apply's actor `auto`; the code on PR #316 is
`AutoActor = audit.Actor{Kind: system, ID: "auto"}`. Under this ADR its ID is
`koinon-api:price-refresh`, and ADR 0098's "actor a user or `auto`" reads "a user, a `cli` actor
or `system koinon-api:price-refresh`". The platform price tables' actor column stores the same text
the audit log shows (`Actor.String()`), so the version history and the audit event name the actor
the same way. The edit to ADR 0098's wording lands with the first implementation issue, which also
touches #316's code, so the two PRs do not both edit that ADR.

Org chains follow the same rule: the `system` writers in `koinon-reconciler` and `koinon-agentd` get
component IDs (`koinon-reconciler:sandbox-repair`, `koinon-agentd:home-seed`, ...). The CHECK that holds
new `audit_events` rows to it, without touching old ones, follows in #359 (see the rollout window above).

### 3. Scope for v0.1: everything install-wide

| Action (install chain) | Today | Actor |
| --- | --- | --- |
| `platform_price.set`, `platform_price.rollback`, `platform_price.retired`, `price.proposal.accepted`, `price.proposal.dismissed`, `price.refresh.settings_changed` (ADR 0098 decision 5) | copies per admin, or `slog` only (#316) | user, `cli` or `system koinon-api:price-refresh` |
| `platform_admin.grant`, `platform_admin.revoke` | the user's personal org | user, or the OIDC group sync (today `system oidc`, renamed `koinon-api:oidc-sync`) |
| `platform_admin.bootstrap` (the first admin's creation, with the default org's ID as target) | not audited | `system koinon-api:bootstrap` |
| `platform_admin.grant` by bootstrap (it re-grants the bootstrap admin on every start; an event whenever that grant inserts a row, so a removed grant coming back shows) | not audited | `system koinon-api:bootstrap` |
| `trace.platform_destination.set`, `.delete`, `.test`; `trace.platform_ceiling.set` | the acting admin's personal org | user |
| `keys.add`, `keys.activate`, `keys.rewrap`, `keys.reencrypt` (only `keys.rewrap` and `keys.reencrypt` have an optional `.started`), `keys.retire`, `keys.forget` (its platform part, under the same advisory lock and `keys.observed` check as the others; each org's part stays per org) | `keys.forget` copies per admin; the rest not audited | `cli` |
| `keys.generated` (a KEK or the saved-work key `koinon-api` makes) | not audited | `system koinon-api:keys` |
| `keys.secret_created`, `keys.add`, `keys.activate`, `keys.bound` from the key-admin Job | not audited | `system koinon-api:key-admin` |
| `keys.observed` (the key set or a generated Secret differs from the last recorded state) | not audited | `system koinon-api:startup` at a start (the first start too, even with an empty key set); `system koinon-api:keyring-watch` when the running `koinon-api` sees the key set change; the actor of the key command (`cli` or `system koinon-api:key-admin`) when that command finds the gap before its own change |
| `platform.settings.observed` (Helm-set platform settings that differ from the last recorded ones) | not audited | `system koinon-api:startup` |

**The bootstrap admin's default org.** Bootstrap also changes the default org on every start (its
identity mode, its group mappings, and the revocation of bootstrap sessions when bootstrap login is
off). Those are org changes: they go in the default org's chain, actor `system
koinon-api:bootstrap`, each written only when it changes something, with `install_event_id` when an
install event in the same start caused it.

The actions are `org.identity_mode.set`, `org.group_mappings.set` and `org.bootstrap_sessions.revoked`. On
the first start the creation is the one event: `platform_admin.bootstrap` stands for the grant that
comes with it, and `platform_admin.grant` is written only when the grant insert adds a row for an
admin that already exists (maintainer decision 2026-11-07: one event, `platform_admin.bootstrap`, which covers the grant made with it).

When the install audit writer role is not granted yet (decision 1.1a), the bootstrap transaction rolls
back whole and a background retry runs it again after the grant. The revocation of bootstrap sessions does
not wait for that: with bootstrap login off, the revocation and its `org.bootstrap_sessions.revoked` event
run in a transaction of their own, right after the refused one. It takes the bootstrap lock and the default
org's head and no install lock, so the lock order of decision 1.7 holds. The retry then finds no session to
revoke and writes no second event. This event has no `install_event_id`: the install event of that start
did not exist when it was written, and the revocation does not depend on it.

**Keys: not atomic, so checked on start.** Key events are not atomic with the change they record,
in three ways, and the ADR says so rather than pretending otherwise:

- Making, adding, activating or retiring a key is a Kubernetes Secret update, not a database write.
  The writer (`koinon-api`, the `keys` CLI, the key-admin Job) records the event right after the
  update succeeded, with the KEK IDs and fingerprints before and after. So `keys activate` needs
  `--database-url`, like `add` and `retire` (maintainer decision 2026-11-08); the `DATABASE_URL`
  alias still works.
- `keys rewrap` and `keys reencrypt` run one transaction per row (`Tool.each` and `Tool.one` in
  `internal/keyscli/keyscli.go`). They write one `keys.rewrap` or `keys.reencrypt` install event per
  run, after the loop, with the counts done and failed, the failures by kind, and the KEK before and
  after; they may also write a `.started` event before the loop (`keys.rewrap.started`,
  `keys.reencrypt.started`, with `--record-started`), so a run that dies half way shows. With
  `--record-started`, a run that cannot write its `.started` changes nothing and fails (maintainer
  decision 2026-11-08). A run that changes no row and fails none writes nothing; a run that wrote a
  `.started`, or failed a row, always writes its closing event. A row that fails counts under one
  kind: `wrap` (no new envelope could be made), `open` (the old one does not open), `envelope` (it
  does not parse, or names a format or algorithm the keyring does not know) or `database`. A column
  whose rows cannot be listed counts as one `database` failure and ends the run's loop, so the
  columns after it are not tried; the closing event records it and the run fails.
- `keys forget` keeps its per-org events in each org's transaction. Its platform part (the platform
  trace destination) follows the same pattern as the other key commands: it begins with `Baseline`
  (the key events' advisory lock, then the check against the last recorded state), so a key-set
  change whose event was lost is written as `keys.observed`, with `keys forget` as the actor, before
  the forget event; the row update and the event are then one transaction, which `Commit` ends. The
  orgs' transactions are finished by then, so the lock order of decision 1.7 holds: the key events'
  lock, then the install head. `forget` does not change the key set, so the check only finds changes
  that others made.

So a crash or a failed audit write after a Secret update must not leave the change unrecorded. On
every start `koinon-api` writes `keys.observed` whenever the whole key-set state differs from the
state in the last recorded key event, and on the first start, when there is no recorded key event,
always, even with an empty state (no KEK and no generated Secret), so the chain says what the
install started with (maintainer decision 2026-11-08). The running `koinon-api` runs the same check,
as `koinon-api:keyring-watch`, under the same lock and with the same loader, when its keyring watch
sees the key set change, so a lost event shows without waiting for a restart (`keys.*` with a key-set `after`): the KEK IDs with their
fingerprints, which one is active, which are retired, and for the generated Secrets their name, UID
and creation time. This compares state, like `platform.settings.observed`, not just "an ID not seen
before", so an `activate` or `retire` whose event was lost is still caught at the next start. It
covers keys made outside Koinon too (`secrets.kek.existingSecret`, and the ensure-secrets Job, which
runs as a Helm hook before the database may exist). Those keys are recorded as `keys.observed`, never
as `keys.generated`: `keys.generated` is only for a key `koinon-api` made itself (maintainer decision
2026-11-08). `koinon-api` already has `get` and `list` on
Secrets in its namespace (`charts/koinon/templates/rbac.yaml`); it reads the generated Secrets'
metadata only, through a label-selected list, never their values, and never stores a hash of a
value. A KEK is named by ID and fingerprint, as `keys status` prints it.

The key-set state that `after` (and `before`) carry is `{keks: [{id, fingerprint, wrapped_by,
binding}], active, retired: [id], generated: [{secret_name, uid, created_at}]}`. A KEK that left the
key set stays in `retired`, so `retire` is told from a KEK that never was there. The key events
target `kek` (by ID), `secret` (by `secret_name`, for `keys.generated` and `keys.secret_created`) or
`keys` (`keys.observed`, ID `keyset`). A writer that does not list the generated Secrets merges what
it sees over the last recorded list, so it does not erase the saved-work key from the state. The CLI
runs with `koinon-api`'s Role (`get` and `list` on Secrets) and chooses not to list; the key-admin
Job's Role has `get` and `update` by name only, so it cannot. The key-admin Job retries the chain
while it is not ready, with one `--wait` budget for the whole run (not per call; 5 minutes by
default), and when it still cannot read or write it warns and succeeds: the change stands, is not
recorded, and the next start records `keys.observed`. The CLI makes the change and says so in an
error (`ErrNotAudited`). Both, the one budget and its 5 minutes, and warn-and-succeed for the Job
against `ErrNotAudited` for the CLI, are maintainer decisions of 2026-11-08.

A key command takes its "before" from the live key set, so it must not absorb a change whose event was
lost (an `activate` whose write failed, then an `add`). Each command (the CLI's `add`, `activate` and
`retire`, and the key-admin Job's Secret creation, recovery, `addKey` and install binding) opens one
transaction and takes the key-set advisory lock (`pg_advisory_xact_lock`, the one the start-up check
takes) before it loads the key set. It holds that lock through its Secret update and its event, and
commits only after both: a start-up or keyring-watch check that runs while the Secret is already
updated waits for the lock and then finds the change recorded, so it writes nothing. `activate` and
`retire` check and edit the key set loaded under the lock, not one read before it (on a terminal the
picker and the question run first, on an earlier read, and the choice is checked again under the
lock). The Job's recovery writes its `keys.add` and `keys.activate` in that one transaction.

Under the lock, the command compares the live KEKs, the active KEK and the retired KEKs with the last
recorded state and, when they differ, writes `keys.observed` before its own event, in the same
transaction (at its first event, or at commit when the change did not happen), so the install head is
still the last lock taken (1.7). The actor of that event is the command's own, not
`koinon-api:startup` (maintainer decision 2026-11-08): the writer is the command that found the gap,
and a start-up actor would claim a start that did not happen. The command's event then has that
observed state as its `before`. With no earlier key event there is nothing to compare, and the
generated Secrets are left to the start-up check. The start-up check itself loads the key set and the
generated Secrets inside the locked transaction, so a change recorded while it waited for the lock is
not mistaken for a missing event. `koinon-api`'s own `keys.generated` (the first KEK, the saved-work
key) is still written right after the Secret is made, not under the lock from before it; a second
replica's start-up check in that gap can record the new key as `keys.observed` first.

`koinon-api` does not know its Helm release name (no flag or environment variable carries it), so its
label-selected list of generated Secrets (`koinon.dev/generated`) is not scoped by the chart's
`koinon.dev/release` label: two releases in one namespace see each other's generated Secrets in their
`keys.observed` state. Scoping it needs a new start-up input; release identity is #375 (v0.2). Until
then one Koinon release per namespace stays the rule (maintainer decision 2026-11-08).

**Helm-set settings.** `koinon-api` builds, on start, a fixed list of install-wide settings from its
flags. For a flag that names a Secret, a file or a directory it records the name only, never the
contents. The list:

- auth and sessions: `bootstrap-login`, `bootstrap-admin-username`, `bootstrap-password-file` (name
  only), `default-org-identity-mode`, `default-org-group-mappings`, `oidc-issuer-url`,
  `oidc-client-id`, `oidc-redirect-url`, `oidc-groups-claim`, `oidc-scopes`,
  `oidc-platform-admin-groups`, `session-idle-timeout`, `session-max-lifetime`, `public-url`,
  `trusted-origins`;
- network and gateway: `egress-allowed-hosts`, `provider-allowed-hosts`, `guard-addr`,
  `leak-guard-byte-cap`, `gateway-url`, `gateway-mcp-url`, `github-api-url`;
- keys and secrets: `kek-secret`, `kek-dir`, `kek-create`, the wrapping flags (`kek-backend`,
  `openbao-*`, `awskms-*`), `saved-work-secret`, `saved-work-addr`;
- telemetry and scheduling: `otlp-addr`, `otlp-token-dir`, `durable`, `run-cap-sweep-interval`;
- the chart and image version.

The price refresh flags are not on it: #316 records them as `price.refresh.settings_changed` (ADR
0098 decision 5), and one change is recorded once. In one transaction under a fixed advisory lock
(so replicas starting together write once) `koinon-api` compares the list with the settings from
all `platform.settings.observed` events applied in order (each holds only the changed keys) and,
when they differ, writes one event with the changed keys in `before` and `after`. Settings an admin changes through the API are audited by that API
call, not here. The list is a Go table next to the flags; adding a flag means deciding whether it is
on it, and a test lists the flags that are not, so none is forgotten.

**Amendment (maintainer decision 2026-11-08): summary keys.** Five flags that name a Secret, a file
or a directory are recorded under a neutral summary key, because the audit scrubber refuses a key that
looks like a secret value: `bootstrap-password-file` as `bootstrap-admin-pw-file`, `kek-secret` as `kek-object`,
`saved-work-secret` as `saved-work-object`, `otlp-token-dir` as `otlp-auth-dir`, and
`openbao-token-file` as `openbao-auth-file`. Every other flag is recorded under its own name.

### 4. Anchors in v0.1; signed checkpoints designed now, built after v0.1

Maintainer decision 2026-11-07: anchors (4.1) ship in v0.1; signed checkpoints (4.2) stay after
v0.1. The format above is version 1 and stays as it is when checkpoints arrive; they are new install
events and a new Secret, not a new encoding.

Anchors on their own do not stop someone who can rewrite the install chain. A database superuser
can rewrite the install chain, its anchors and the org chains together, and `audit verify` still
passes. Anchors catch a rewrite or rollback of an org chain behind an anchor while the install
chain is intact, and they make the install head the one value that commits to every chain. Signed
checkpoints (4.2), exported off the cluster, close the remaining gap.

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
long as the install chain is intact. An org created after the last anchor is simply not in it yet;
an org in an anchor whose chain is gone (its head row missing) fails verification, as a deleted
chain would.

**Amendment 2026-11-08: how anchors are built (issue 6, #333).**

- **Encoding.** `heads_root` is sha256 over the fields `koinon.install_audit.anchor.v1` (a tag), the
  install ID, the number of heads, then for each head in org ID (lower-case UUID text) order the
  triple `(org_id, chain_seq in decimal, hash in lower-case hex)`. Every field is length-prefixed
  with 8 bytes as in decision 1.3. The tag and the install ID bind the root to this install. The
  event's actor is `system koinon-api:audit-anchor`, its summary `after` holds `org_count` and
  `heads_root`, and the side rows are inserted in the same batch.
- **One writer per tick.** The anchor takes a domain lock, `pg_try_advisory_xact_lock(hashtext(
  'koinon.audit_anchor'))`, before the install head (decision 1.7 lock order), and under it checks
  that the newest anchor is older than the interval. A replica that loses the try skips the tick; one
  that wins after another has just anchored sees a recent anchor and skips. `audit anchor` waits for
  the lock instead and always writes. The heads are read with a plain READ COMMITTED read, so a head
  an org write has not committed yet is not named.
- **Settings.** `--audit-anchor-interval` (default 1h, at least 5m, because every anchor stores one row per org; a Helm-set setting recorded by
  decision 3). The job polls at the lesser of the interval and 30s, so a restarted pod catches up. The
  `koinon_audit_anchor_age_seconds` gauge is the age of the newest anchor by the database's clock. The gauge keeps its last value when
  the read fails, so a database-down alert sits next to it.
- **Grants.** `install_audit_anchor_heads` has the install chain's three guards (UPDATE, DELETE and
  TRUNCATE are refused). `koinon_app` gets SELECT only, and `koinon_install_audit_writer` gets INSERT
  and SELECT from `GrantInstallAuditWriter`, like the install tables.
- **Verify.** `audit verify` recomputes each anchor's root and checks `org_count`, then checks each
  anchored head against its org chain at the anchored `chain_seq`. An org is reported once, at its
  oldest failing anchor, naming the org and the anchor. An org absent from an anchor is not an error.
  `audit verify --org` reads only that org's anchored heads (an org-filtered query).
- **The success line is a witness.** The log line of a written anchor ("anchored the org audit chains")
  carries `chain_seq`, `orgs`, `heads_root` and `install_head`, the hash of the `audit.anchor` event
  just written, in the hex of decision 1.3. They are hashes of audit chain state, not secrets. Shipped
  off the cluster, these lines are a witness the database cannot rewrite, the way CT gossip and
  CloudTrail's digests in S3 are: a rewrite of the install chain that keeps `audit verify` passing
  still disagrees with the logged `install_head` and `heads_root`.
- **Consistency before each write.** Under the anchor lock, before the new anchor is written, the
  org chains are checked against the newest earlier anchor: each org's chain must still hold the
  anchored hash at the anchored `chain_seq`, and no org's head may be below the anchored `chain_seq`.
  This is the rule of C2SP tlog-checkpoint that a signer never signs a checkpoint inconsistent with one
  it signed before. Only the newest anchor is read, one row per org: each earlier anchor was checked
  when its successor was written, and `audit verify` covers the full history. A mismatch is logged once
  per tick, naming the org (logs may; metrics may not), and counted in
  `koinon_audit_anchor_inconsistent_total{reason="hash_changed"|"seq_regressed"}` (no org label). The
  anchor is still written, so the chain of anchors goes on and `audit verify` reports the break.
- **Future purge path (follow-up, not built here).** Orgs are soft-deleted today, and `audit_events` and
  `audit_chain_heads` reference `orgs(id)` with no cascade, so an anchor whose org chain is gone fails
  verification. A future hard purge must write an install-chain `org.purge` event in the same
  transaction as the purge, under the anchor lock, naming the `org_id`, the last `chain_seq` and the
  head hash. Verify would then accept a missing chain only when such an event follows every anchor that
  names the org.
- **Header size.** `exportAnchors` carries only the anchors whose event is in the export's
  `from_seq`..head range, so export by period with `--from-seq`. A full export from seq 1 still puts
  every anchor in one header line, about 110 MB a year at 100 orgs anchored hourly. Moving each
  anchor's heads into stream lines next to their `audit.anchor` event is a follow-up before v0.3.
- **Export.** An install export's header carries `anchors`: for each anchor event in the exported
  range its `chain_seq`, event ID, `org_count`, `heads_root` and heads. `VerifyExport` checks the
  header against the events (a missing, forged or duplicated entry, a wrong root or count) with no
  database. `audit verify --file <org export> --anchors <install export>` then checks an org
  export's hashes at the anchored `chain_seq` values. The install export verifies on its own first.
  An org export with no `--anchors` is internally consistent only, and says so.

*Prior art (sources read 2026-11-08).*

- **Certificate Transparency** (RFC 9162, read from its source draft `draft-ietf-trans-rfc6962-bis-43` in google/certificate-transparency-rfcs). A signed tree head (`TreeHeadDataV2`: timestamp, tree_size, root_hash, extensions) commits to the whole log at one size. A client checks two heads against each other with a consistency proof.
- **Trillian** (`types/logroot.go`). Its `LogRootV1` (TreeSize, RootHash, TimestampNanos, Metadata) is the same idea in a storage system.
- **What anchors borrow.** One small value, the `audit.anchor` event with `heads_root` and `org_count` at an install `chain_seq`, commits to the state of every chain. A verifier later compares that commitment with the chains as they are now. Publishing the commitment outside the log's own storage is the CT gossip idea (monitors compare the heads they saw): the anchor log line carries `heads_root` and `install_head` so an operator can do that with their log pipeline.
- **Where anchors differ.** Koinon's chains are linear hash chains, not Merkle trees, so there are no consistency proofs. A verifier re-reads each org chain at the anchored `chain_seq` and compares the hash. That is linear in chain length, which is fine at audit-log sizes and needs no tree storage.
- **C2SP tlog-checkpoint and signed-note** (C2SP/C2SP `tlog-checkpoint.md`, `signed-note.md`). A checkpoint is origin, tree size and root hash lines, then em-dash signature lines with a 4-byte key ID. A signer must not sign a checkpoint that is inconsistent with one it signed before. Anchoring follows that rule by checking the org chains against the newest earlier anchor before it writes (the consistency bullet above). Decision 4.2 (issue 7) borrows the note layout. Anchors do not use it because they are unsigned and live in the database.
- **AWS CloudTrail log file integrity digests** (awsdocs/aws-cloudtrail-user-guide, `cloudtrail-log-file-validation-digest-file-structure.md`). An hourly digest lists each log file's `hashValue` and links to the previous digest through `previousDigestHashValue` and `previousDigestSignature`. A digest is delivered even for an hour with no activity, with `"logFiles": []`.
  - Anchors borrow the cadence (hourly by default) and the rule that an anchor is written every interval whether or not any head moved, so a missing anchor is itself a signal. That is what the `koinon_audit_anchor_age_seconds` gauge alerts on.
  - Anchors also borrow the shape: one record naming the hash of every covered unit (here, every org chain head).
  - They differ in two ways. The link to the previous anchor is the install chain itself, not a field. The record is not signed; signing is issue 7, and it is what moves trust outside the database the way CloudTrail's signed digest in S3 does.
  - Until then the success log line gives a cheaper off-database copy: shipped to a log store the database role cannot write, `heads_root` and `install_head` play the part of the digest delivered to S3.
- **Postgres audit projects.** pgMemento (`src/SCHEMA.sql`, `src/LOG_UTIL.sql`) and supabase/supa_audit, now archived, record changes with triggers into a log table. Neither hashes rows; pgMemento's `row_log` has no hash column.
  - Koinon's org chains already go further: hash-linked rows behind append-only triggers.
  - No project we read commits several chains to one head. That is what an anchor adds.

*Seams shared with issue 7 (signed checkpoints), not built here.* (1) Checkpoints sign the install
head after an anchor, so the anchor event is what a checkpoint's one hash covers: `audit.checkpoint`
follows `audit.anchor` and the anchor job is the natural place to call the signer. (2) The advisory
lock and the interval check are reusable: a checkpoint is written under the same lock, so one replica
signs per tick. (3) `audit verify --file ... --anchors` is where `--checkpoint` and `--public-key`
join: the anchors in an install export header are the thing a checkpoint pins from outside the
cluster. (4) The export header grows by an optional `checkpoints` list in the same way as `anchors`
(`omitempty`, so v1 files stay valid). (5) `koinon_audit_checkpoint_age_seconds` sits beside
`koinon_audit_anchor_age_seconds` in the same registration helper. None of these change the
encoding above.

**4.2 Signed checkpoints.** After each anchor, `koinon-api` signs the install head. The checkpoint is
a signed note in Koinon's own format. It borrows the layout of a C2SP signed note (origin line, body,
signature lines), but it is not a transparency-log checkpoint in the tlog sense: its body is a
hash-chain head, not a Merkle tree root, so it supports no inclusion or consistency proofs, and
tools built for tlog checkpoints do not verify it.

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
/admin/v1/audit?scope=install`, under the same rule. An admin API token is bound to an org, but a
token whose root user is a platform admin may read and export install scope: it is a read, the
token's chain is re-checked on every request (`docs/ops/secret-keys.md`, "Admin API tokens"), so
it stops when the user stops being a platform admin, and no token writes to the install chain
through this path. A token of anyone else gets `PermissionDenied`. The reply's `Event` gains
`chain_seq`, and an install event has no `org_id`. Org admins and owners never see the install chain; they see the
effects in their own chain, with the pointer back. A read-only platform auditor role and a web page
may come later.

**Verify.** `koinon-api audit verify` checks the install chain as well as the org chains by default
(`--install` for the install chain alone, `--org <id>` as today), and also checks every anchor
against the org chains.

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
- `koinon_install_audit_writer_ok` (gauge, no labels, koinon-api only): 1 while koinon-api's login
  inherits the writer role, 0 otherwise (decision 1.1a, maintainer decision 2026-11-07);
- `koinon_audit_anchor_age_seconds` (gauge, no labels, from v0.1), so an alert can say anchoring
  stopped, and, with checkpoints, `koinon_audit_checkpoint_age_seconds` (gauge, no labels);
- `koinon_audit_anchor_inconsistent_total{reason="hash_changed"|"seq_regressed"}` (counter, from
  v0.1): orgs found inconsistent with the newest earlier anchor; alert on any increase.

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
- Positive: from v0.1 the install chain anchors every org chain, so a rollback of an org chain
  behind an anchor is detected. Signed checkpoints later close the superuser-rewrite gap for all
  chains at once, and the version 1 format already allows them.
- Negative: until checkpoints ship, someone who can rewrite the install chain (a database
  superuser or the table owner) can still rewrite every chain undetected.
- Negative: a second table, trigger, hash function and verify path to keep in step with the first;
  the shared Go encoder and a SQL/Go agreement test for each tag contain it.
- Negative: one more lock order rule (install head first). The audit module writes it (the audit
  batch, amended 1.7), so code review no longer holds it for callers that use the batch; a shared spec
  suite on the Postgres batch and the in-memory fake checks the order, and a Postgres spec shows two
  transactions writing orgs A,B and B,A both finish. Since step 3 the compiler keeps every other
  package off the audit inserts (`internal/audit/internal/auditstore`), and `hack/audit-batch-lint.sh`
  keeps raw and dynamic SQL off the audit tables.
- Negative: key events are not atomic with their change: Secret updates are recorded right after
  them, and `rewrap` and `reencrypt` once per run after a loop of per-row transactions. The start-up
  `keys.observed` pass compares the whole key-set state, so a lost event is caught at the next start,
  at the cost of an event that says "observed" rather than what happened, and of the gap until then.
- Negative: the existing platform rows in personal orgs stay there, so the history of platform
  actions before the cutover is split between personal orgs and the install chain. There is no
  schema promise before v0.3 (ADR 0099), so no backfill is owed; the cutover release notes say where
  the older rows are.

## Implementation plan

Each item is its own issue; the first blocks #316 and #305. Issues 1 to 6 are v0.1; issue 7 is after
v0.1 (maintainer decision 2026-11-07).

1. **Install audit chain, and platform prices in it.** Migration (tables, hash function, triggers
   with the head's `pg_trigger_depth()` exception, grants), the `koinon_install_audit_writer` group
   role in the chart's `postInitApplicationSQL` and in `koinon_api`'s `inRoles` (decision 1.1a),
   the install event write (now the audit batch's `b.Install`, decision 1.7), the `Event` fields of decision 1.2a, `audit.System(id)`, the shared
   encoder, `audit verify` over both kinds, the `koinon_audit_writes_total` metric, the
   install-scope `AuditService.List` read and `GET /admin/v1/audit?scope=install`, and
   `docs/ops/secret-keys.md`'s external-database lines. Replace `auditPlatform` on #316 (or a
   follow-up that lands before #305) with install events, rename `AutoActor` to `system
   koinon-api:price-refresh`, and align ADR 0098's actor wording. Checks: a new
   `hack/chart-check.d/NN-install-audit-writer-role.sh` asserting that the CNPG render creates the
   group role and that only the `koinon_api` managed role is in it; and `make test-chart-smoke`
   extended so `koinon-api` writes an install event on its first start with the roles created in
   the chart's order (group role before the migration, login role after), and an insert as the
   reconciler's role is refused.
2. **Named system actors elsewhere, the remaining install actions, lock order.** Component IDs for
   every system writer in the org chain and its `NOT VALID` check; `platform_admin.grant/revoke`,
   the bootstrap events and the default org's bootstrap changes, the platform trace destination and
   ceiling, and `keys forget`'s platform part move to the install chain, with `install_event_id` on
   their org effects. Reorder `SetPlatformCeiling` (install event first) and add `ORDER BY org_id`
   to `LowerOrgTraceLevels` (decision 1.7). Retire `ListPlatformAdminPersonalOrgs` as an audit
   target.
3. **Key administration and key generation.** `keys.add/activate/rewrap/reencrypt/retire/forget`
   with one event per `rewrap` or `reencrypt` run, the key-admin Job's events (actor
   `koinon-api:key-admin`), `keys.generated`, and the start-up `keys.observed` pass that compares
   the whole key-set state (decision 3). Generated Secrets are read by metadata only, through the
   existing RBAC.
4. **Helm-set settings.** The settings list of decision 3, `platform.settings.observed` under an
   advisory lock, and the test that lists flags left off it.
5. **Export.** JSON lines export (CLI and admin API), `audit verify --file`, and the export format
   in the docs site.
6. **Anchors** (v0.1). `install_audit_anchor_heads`, the hourly job and `koinon-api audit anchor`,
   verifier checks, anchor age gauge.
7. **Signed checkpoints** (after v0.1). The signing Secret, `audit.signing_key.added`,
   `audit.checkpoint`, the signed note, off-cluster verification, checkpoint age gauge, rotation.

Retention stays with #29 and is designed against decision 5.

## Open questions

None open.

- ~~Should anchors ship in v0.1?~~ Yes, maintainer decision 2026-11-07 (decision 4); signed
  checkpoints stay after v0.1.
- ~~New RBAC for `keys.observed`?~~ No: `koinon-api` already has `get` and `list` on Secrets in its
  namespace (decision 3).
- ~~Which flags are on the Helm-settings list?~~ The list in decision 3.

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
