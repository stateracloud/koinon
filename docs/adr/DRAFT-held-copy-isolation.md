# DRAFT. A restored copy does not act as its source

- Status: Proposed (draft; the number is reserved by the parent session with `hack/claim.sh adr`)
- Date: 2026-11-10
- Amends: [0107](0107-restore-hold-mode.md) (what a held install writes; release)
- Relates to: [0106](0106-install-mark-owns-openshell-objects.md) (the install mark),
  [0085](0085-upgrade-compatibility-and-recovery-policy.md) (Context 6, decision 15), [0100](0100-install-audit-chain.md)
  (recorded settings), [0023](0023-observability-and-audit.md) (fixed metric labels), [0004](0004-human-identity.md) (sessions)
- Issues: #516; refs #514 (drill phase 1), #512 (break-glass admin access), #275

## Context

ADR 0107 holds a restored install: nothing acts until a rollout releases it. The hand drill of 2026-11-10
(friction notes 7, 8, 9, 15) and the drill design (#514) found five gaps. The hold stops new work, but the copy
still has effects, and after the release it acts with the source's identity:

1. **The importer writes while held.** `removeAbsent` (`internal/declarative/declarative.go:480-540`) deletes the
   providers, disables the MCP servers and marks removed the connections whose Secret is missing. A copy in a
   fresh namespace has none of the source's declarative Secrets, so a held copy loses them before anyone checks.
2. **An admin cannot turn off an MCP server.** `disabled_reason` exists (`00015_mcp_servers.sql`), but only the
   system sets it (shadowed, Secret removed). After the release, every MCP server of the copy dials the
   source's endpoints with the source's credentials.
3. **Every source credential works on the copy.** API tokens (`internal/tokens`, default TTL 30 days, max 90) and
   sessions (`internal/auth/session.go`, idle 2 h, max 12 h) are rows keyed by a plain SHA-256 of the secret. The
   rows travel with the dump, so a token or cookie stolen from, or still held by, a source user works on the copy.
4. **Plain chart values point the copy at the source.** With the source's `checkpoints.s3` values the copy's agentd
   writes snapshots into the source's kopia repositories and applies retention to them (`checkpoint/writer.go:211`),
   so it can delete source snapshots. With the source's `publicUrl` or `oidc.redirectUrl`, the IdP sends the copy's
   logins to the source.
5. **The operator cannot see what the release will start.** There is no single view of the outward credentials in
   use or of the backlog (due schedules, queued runs and their caps).

The install mark (ADR 0106) is the one value that does not travel with a dump. Today only agentd and the reconciler
read it (`KOINON_INSTALL_MARK`); koinon-api does not. The chart cannot compare the copy's values with the source's:
renders are deterministic and use no `lookup`.

Two restore shapes must keep working:

- **In place** (ADR 0085 Context 6; `test/e2e/backup-restore.sh`): the same install, the same namespace, the same
  mark Secret. There is no second copy, so the source's credentials are the install's own.
- **Fresh namespace**: a drill copy, a staging copy, or a disaster-recovery replacement. The mark is new unless the
  operator applied the mark backup (ADR 0106 Consequences: a replacement does, a drill copy does not).

## Decision

### 0. Foundation: the database records which install it belongs to

A new one-row table `install_binding` holds the hash of the mark (`execution.OwnerID`, the same hash as the
OpenShell label) of the install that last ran unheld on this database, and that install's **outward targets**: the
public URL and OIDC redirect URL (as `kindURL`), and the checkpoint S3 endpoint and bucket. koinon-api gets
`KOINON_INSTALL_MARK` (a new startup input: `make test-chart-smoke` covers it). At start each binary compares its
own mark with the row:

| Row | Mark | State | Effect |
|---|---|---|---|
| present | equal | `same` | Normal. An unheld start refreshes the outward targets. |
| present | different | `copy` | Decisions 3 and 4 apply. The row is never overwritten automatically. |
| absent | any | `unknown` | Held: treated as `copy` (fail safe; a dump taken before this ADR). Unheld: an upgrade of a live install; it writes the row (claim, as ADR 0106 decision 4). |

`koinon-api restore adopt` (CLI in the api pod, audited `install.adopt`, recorded in the install chain of ADR 0100)
is the one way to move a `copy` to `same`: it rewrites the row with the copy's mark and targets and re-enables the
quarantined tokens (decision 3). A disaster-recovery replacement that lost its mark Secret uses it; a drill copy
never does. The state is exported as `koinon_install_binding{state}` (fixed set: same, copy, unknown; ADR 0023).

### 1. The importer reports and changes nothing while held

Options:

- **A. Plan only (recommended).** While held, one importer pass computes the plan it would apply (apply, update,
  remove per kind) and writes nothing to the database. It logs `held: would <action> <kind> <name>` once per
  change, records one Kubernetes Event per Secret (reason `HeldNotApplied`), and exports
  `koinon_declarative_held_changes{kind,action}` (kinds: model-provider, mcp-server, connection; actions: apply,
  remove). `restore status` (decision 5) prints the plan. The first unheld pass applies it. Cost: the plan code
  must share the apply code's comparisons, or it reports a plan that the release does not carry out; the plan is
  therefore a dry-run mode of the same functions, not a second implementation.
- B. Skip only the removal pass while held, apply the rest. Smaller change, but the held copy still writes, so the
  drill cannot assert "the database does not change while held", and an apply can still clear a code-host scope.
- C. Do not run the importer while held. Simplest, but the operator does not see that the release will delete
  every declared provider of a fresh copy.

Release note (an ADR 0107 amendment): on a fresh copy the release still removes what has no Secret. Decision 5
shows that count before the release; question Q3 asks whether the release needs a confirmation.

In place: the Secrets are there, the plan is usually empty. Fresh copy: the plan shows every source Secret as a
removal; the operator applies the Secrets (DR) or accepts the removal (drill).

### 2. An admin can disable an MCP server

Options:

- **A. A separate admin flag (recommended).** New columns `admin_disabled_at`, `admin_disabled_by`,
  `admin_disabled_note`. A disabled server is not dialled, not probed, and its tools are not offered to runs; it
  keeps its bindings and credential. Surfaces: admin REST and Connect (`DisableMCPServer`, `EnableMCPServer`), the UI
  server page, and the declarative field `disabled: true` (the Secret wins, as with every declared field; the UI
  cannot enable a server whose Secret disables it). Audited `mcp_server.disable` and `mcp_server.enable`. Disable is
  allowed while held (it reaches nothing). Enable is refused while held, because it runs the preflight
  (`restorehold.ErrHeld`). The reconciler trigger of migration 00059 adds the new columns.
- B. Reuse `disabled_reason` with an admin reason. No migration, but an admin intent and a system state share one
  column: re-declaring a shadowed server would clear the admin's disable, and the unique index
  `mcp_servers_org_name_enabled` then treats a disabled server as free to shadow.
- C. One install-wide "outward off" switch for every integration. Coarse, and it is the hold again under another
  name; it does not let an operator keep one safe server.

The same pattern (an admin disable flag, audited, declarable) fits model providers, code-host connections, trace
destinations and notification sinks. This ADR does only MCP servers (#516); question Q4 asks whether the others
follow.

### 3. Tokens and sessions of the source do not work on a copy

Options:

- A. **Key the hashes with the mark** (HMAC-SHA-256 with a key derived from the mark, the Rails
  `secret_key_base` pattern). A copy with another mark computes other hashes, so every source credential fails with
  no extra state. Costs: a lost mark Secret locks every user out of the live install with no way back except new
  tokens; an adopt cannot re-enable a token (the raw secret is not stored); the failure reason is "wrong secret", so
  an operator cannot tell a copy from an attack; and the change rewrites every existing hash (dual verification
  during a migration window).
- B. **Revoke on restore.** In state `copy`, the first start revokes every session and every token, audited.
  Clear and simple. Costs: it cannot be undone, so a disaster-recovery replacement that lost the mark Secret loses
  every CI and integration token at once; and a copy started held still accepts the source's credentials until
  the revoke pass ran.
- **C. Bind each row to the mark, quarantine foreign rows (recommended).** `api_tokens` and `sessions` get a
  nullable `install_mark` (the hash). A new row is stamped with the creating process's mark. Authentication
  refuses a row whose mark is not the process's own mark, with the reason `issued by another install (restored
  copy)`, audited as `token.use_failed` and counted in `koinon_auth_quarantined_total{kind}` (kind: token,
  session). A quarantined token is re-enabled only by an audited action: `koinon-api tokens rebind <id>` or
  `restore adopt` (all tokens). A quarantined session is never re-enabled: the user logs in again, which makes a
  session bound to the copy (password login works, the hash came with the dump; OIDC login depends on decision 4).
  Legacy rows (no mark) count as own only in state `same`; an unheld start in state `same` stamps them, as ADR
  0106 claims unmarked objects. In `copy` and `unknown` they are quarantined. Costs: two nullable columns
  (expand-only, allowed in a patch line, ADR 0085), one comparison per request, and an operator on a copy needs a
  way in: a new login, or the break-glass path of #512.

Why C: it refuses the source's credentials from the first request of a held copy, it is reversible for the one
case that needs it (a replacement that lost the mark), and the refusal says why. It uses the mark that ADR 0106
already made the identity of an install, so "my objects" and "my credentials" mean the same thing.

Effect per restore shape:

| Shape | A (HMAC) | B (revoke) | C (bind) |
|---|---|---|---|
| In place (same mark) | all work | all work (no `copy` state) | all work |
| Fresh copy, new mark (drill, staging) | all fail, "wrong secret" | all revoked, final | all refused, "restored copy"; new logins work |
| Fresh namespace, mark backup applied (DR) | all work | all work | all work |
| Fresh namespace, mark lost (DR) | all fail, no way back | all revoked, no way back | refused until `restore adopt` |

None of the options closes the in-place gap: a token or session revoked after the dump is valid again after an
in-place restore, because the revocation was in the lost data. Decision 5 lists the sessions and tokens that were
active at the dump time, and the runbook gains `koinon-api sessions revoke-all` (audited) for that case. Question
Q1 asks whether the in-place release revokes all sessions by default.

### 4. A copy does not write to the source's checkpoints or send logins to its URL

The chart cannot see the source's values, so a chart-only check needs the operator to type them. The database does
know them: `install_binding` holds the source's outward targets (decision 0).

Options:

- A. **Chart only.** With `restoreHold: true` the chart requires `restore.source.publicUrl` and
  `restore.source.checkpointBucket` and fails the render when the new values are equal. Deterministic and early,
  but it trusts the operator to type the source values, and a forgotten value turns the guard off.
- B. **Runtime only.** In state `copy`, each binary compares its own targets with the recorded ones. An equal
  checkpoint endpoint and bucket puts agentd in **restore-only** checkpoint mode: it reads snapshots (archived
  homes still come back), but it creates none, applies no retention and runs no kopia maintenance. An equal public
  URL or OIDC redirect URL turns OIDC login off (`/auth/oidc/*` answers 409 with the reason; password login stays).
  Each conflict is logged once and exported as `koinon_restore_isolation_conflict{target}` (target:
  checkpoints, public-url, oidc-redirect). Held or released, the guard holds until the value changes or `restore
  adopt` runs.
- **C. Runtime guard plus a chart switch (recommended).** B, plus a chart value `checkpoints.readOnly` (default
  false) that sets restore-only mode on agentd for any install, and a NOTES.txt warning when `restoreHold` is true
  with checkpoints on and `readOnly` false. The chart asserts both in a new `hack/chart-check.d` file. The runtime
  guard is the protection; the chart switch lets an operator choose read-only before the first start and is the
  documented step for a drill.

While held, agentd already writes no checkpoint (ADR 0107: it only serves `checkpoints resync`, which lists).
Decision 4 is about what happens after the release.

In place: state `same`, no guard; the targets are the install's own. Fresh copy: the guard fires on every target
left at the source's value. DR with the mark backup: state `same`, no guard, which is right, because the source is
gone and the replacement owns its bucket.

### 5. `restore status` shows what the release will do

Options:

- **A. One status function behind the CLI and the admin REST API (recommended).** `koinon-api restore status`
  (in the pod, works with every credential quarantined) and `GET /admin/v1/restore/status` (admin token; JSON for
  the drill). It reads only the database and the cluster, never outward. Sections: the binding state and its
  reason; quarantined token and session counts; sessions and tokens active at the dump time; the outward
  credentials in use (model providers with key fingerprints, code-host connections, enabled MCP servers, trace
  destinations, notification sinks, the checkpoint target, the OIDC issuer and redirect URL, the public URL), each
  marked when it equals a recorded source target; the importer plan (decision 1); and the release preview: per org,
  the schedule triggers due for a catch-up fire (at most one each, look-back 1 h), the DBOS workflows `ENQUEUED`
  and `PENDING` (read from the `dbos` schema without a runtime), parked runs, and the org run and trigger limits
  that will apply to them. Connect and a UI page can follow on the same function.
- B. CLI only. Enough for an operator, but the drill and a future UI need a machine-readable surface.
- C. Extend `keys verify` and `org-limits show`. No new command, but the runbook grows a list of commands and
  nothing shows the whole picture.

## Drill specs this turns from known gap to required (#514)

| Item | Label | Spec |
|---|---|---|
| 1 | `hold` | While held, a declared provider of the source is still present; the database has no importer write; a `HeldNotApplied` Event exists. After release, the removal happens once. |
| 2 | `isolation` | An MCP server disabled while held is not dialled after the release (the fake sink gets no request); enable while held is refused. |
| 3 | `isolation` | A source session cookie and a source API token are refused on the copy with the reason "restored copy"; a new login works; `tokens rebind` makes one token work, audited. |
| 3 | `inplace` | In `backup-restore.sh`, a token issued before the dump still works (state `same`). |
| 0, 3 | `mark` | `koinon_install_binding{state="copy"}` is 1 on the copy and `same` on an in-place restore. |
| 4 | `isolation` | With the source's checkpoint values, the copy reports a checkpoints conflict, and the source's snapshot list is unchanged after release (phase 2, needs the kind S3); with the source's public URL, OIDC login is refused. |
| 5 | `hold` | `restore status` lists the seeded provider, MCP server and schedule, the due catch-up fire and the N-1 queued runs, and these match what happens after the release. |

## Alternatives considered

The per-item options are above. Two cross-cutting ones:

- **Detect a copy from the namespace or cluster ID instead of the mark.** Rejected for the same reasons as in ADR
  0106: a copy can use the same namespace name on another cluster, and a namespace move would look like a copy.
- **Make the copy state a chart value (`restore.mode: copy`).** Explicit, but an operator who forgets it gets a
  copy that acts as the source. The mark comparison needs no input; the chart switch of decision 4 is additive.

## Prior art

- **Vault DR secondaries.** A DR secondary serves no client requests until it is promoted, and promotion needs a
  DR operation token made from unseal or recovery keys, an out-of-band credential. Tokens replicate, so a promoted
  DR secondary accepts the primary's tokens: right for a replacement, which is our state `same` (mark backup
  applied) and `restore adopt`. https://developer.hashicorp.com/vault/tutorials/disaster-recovery/break-glass-failover
- **GitLab Geo.** A secondary must have the primary's `gitlab-secrets.json`, so it shares identity by design and
  forwards writes to the primary; a feature request asks for a warning on a secrets mismatch (gitlab#6080). Shared
  identity is right for a planned failover target, wrong for a drill copy.
  https://docs.gitlab.com/administration/geo/replication/configuration/ , https://gitlab.com/gitlab-org/gitlab/-/issues/6080
- **Discourse.** A restore sets "disable emails" to non-staff by default, so a staging copy does not mail real
  users; `--disable_emails=false` opts out. The fail-safe default for outward effects matches decisions 1 and 2.
  https://meta.discourse.org/t/when-restoring-a-backup-the-disable-email-will-be-set-to-non-staff-from-no/115143
- **Mattermost.** A test server from a production backup needs its own Site URL, or links and notifications point
  at production. Decision 4's public URL guard. https://docs.mattermost.com/configure/environment-configuration-settings.html
- **Rails per-environment credentials and `secret_key_base`.** Cookies signed in one environment do not validate in
  another; changing the key logs everyone out. This is option 3A; its cost (no way back) is why we bind instead.
  https://blog.saeloun.com/2023/08/11/rails-7-1-store-secret-key-base-in-rails-config/
- **Velero restore resource modifiers and restore hooks.** Velero rewrites resources on restore with JSON patches
  and runs hooks in restored pods: the operator changes the copy's targets at restore time. Decision 4C's chart
  switch is the same idea; the runtime guard catches the case where the operator did not.
  https://velero.io/docs/main/restore-resource-modifiers , https://velero.io/docs/main/restore-hooks

## Consequences

- Positive: a copy cannot act with the source's credentials or write to its checkpoints, from its first request;
  the operator sees what the release will do; the drill can assert isolation instead of noting a gap.
- Negative: koinon-api gets a new startup input (the mark), so a lost mark Secret now also quarantines tokens and
  sessions on the live install until `restore adopt`. The mark backup (ADR 0106) becomes more important.
- Negative: an operator on a copy cannot use a source API token; automation against a copy needs a new token.
- Schema: one new table and nullable columns only (expand-only; allowed in a patch line, ADR 0085).
- ADR 0107 is amended: the importer no longer writes while held (only the purger and the audit anchor do), and
  the release of a copy keeps the guards of decisions 3 and 4 until `restore adopt`.
- A dump taken before this ADR, restored unheld, is claimed silently (state `unknown`, unheld). The runbook already
  starts every restore held.

## Open questions (procoduck)

- Q1: does the release of an in-place restore revoke all sessions by default?
- Q2: quarantine (reversible, 3C) or revoke (final, 3B) for a copy's tokens?
- Q3: does the release of a fresh copy need a confirmation flag before the importer removes the declared items?
- Q4: do model providers, connections, trace destinations and notification sinks get the same admin disable flag
  now, or later?
- Q5: may `restore adopt` run only from the pod (break-glass, #512), or also through the admin API by an owner?

## Links

- #516, #514, #512, #275; friction notes https://claude.ai/artifact/BZfuPayDnc1QEfaYSsjuJ4
- `internal/declarative/declarative.go`, `internal/mcpservers/declared.go`, `internal/tokens/tokens.go`,
  `internal/auth/session.go`, `internal/restorehold/restorehold.go`, `internal/execution/names.go`,
  `charts/koinon/values.yaml` (checkpoints, publicUrl, installMark, restoreHold, oidc)
