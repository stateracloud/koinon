# DRAFT. A restored copy does not act as its source

- Status: Proposed (draft; the number is reserved by the parent session with `hack/claim.sh adr`). Decisions 1 to 6
  record procoduck's answers of 2026-11-10; decision 0 and the guards of decision 4 are the design they rest on.
- Date: 2026-11-10
- Amends: [0107](0107-restore-hold-mode.md) (what a held install writes; how a release works)
- Relates to: [0106](0106-install-mark-owns-openshell-objects.md) (the install mark),
  [0085](0085-upgrade-compatibility-and-recovery-policy.md) (Context 6, decision 15), [0100](0100-install-audit-chain.md)
  (recorded settings, install audit chain), [0023](0023-observability-and-audit.md) (fixed metric labels),
  [0004](0004-human-identity.md) (sessions)
- Issues: #516; refs #514 (drill phase 1), #512 (break-glass admin access), #275

## Context

ADR 0107 holds a restored install: nothing acts until a rollout releases it. The hand drill of 2026-11-10
(friction notes 7, 8, 9, 15) and the drill design (#514) found five gaps. The hold stops new work, but the copy
still has effects, and after the release it acts with the source's identity:

1. **The importer writes while held.** `removeAbsent` (`internal/declarative/declarative.go:480-540`) deletes the
   providers, disables the MCP servers and marks removed the connections whose Secret is missing. A copy in a
   fresh namespace has none of the source's declarative Secrets, so a held copy loses them before anyone checks.
2. **An admin cannot turn off an outward target.** `mcp_servers.disabled_reason` exists (`00015_mcp_servers.sql`),
   but only the system sets it (shadowed, Secret removed). Model providers, code-host connections, trace
   destinations and notification targets have no switch at all. After the release, the copy calls the source's
   endpoints with the source's credentials.
3. **Every source credential works on the copy.** API tokens (`internal/tokens`, default TTL 30 days, max 90) and
   sessions (`internal/auth/session.go`, idle 2 h, max 12 h) are rows keyed by a plain SHA-256 of the secret. The
   rows travel with the dump, so a token or cookie held by a source user, or stolen from one, works on the copy.
4. **Plain chart values point the copy at the source.** With the source's `checkpoints.s3` values the copy's agentd
   writes snapshots into the source's kopia repositories and applies retention to them (`checkpoint/writer.go:211`),
   so it can delete source snapshots. With the source's `publicUrl` or `oidc.redirectUrl`, the IdP sends the copy's
   logins to the source.
5. **The operator cannot see what the release will start.** There is no single view of the outward credentials in
   use or of the backlog (due schedules, queued runs and their caps).
6. **Break-glass needs a shell.** #512 proposes `koinon-api tokens create` and `users reset-password` run in the api
   pod. The images are distroless (`static:nonroot`) and clusters can forbid `pods/exec`, so an operator may have no
   way to run them; on a copy with every token refused, there would be no way in at all.

The install mark (ADR 0106) is the one value that does not travel with a dump. Today only agentd and the reconciler
read it (`KOINON_INSTALL_MARK`); koinon-api does not. The chart cannot compare the copy's values with the source's:
renders are deterministic and use no `lookup`.

Two restore shapes must keep working:

- **In place** (ADR 0085 Context 6; `test/e2e/backup-restore.sh`): the same install, the same namespace, the same
  mark Secret. There is no second copy, so the source's credentials are the install's own.
- **Fresh namespace**: a drill copy, a staging copy, or a disaster-recovery replacement. The mark is new unless the
  operator applied the mark backup (ADR 0106 Consequences: a replacement does, a drill copy does not).

## Decision

### 0. The database records which install it belongs to

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

The `adopt-restore` recovery action (decision 6) is the one way to move a `copy` to `same`. The state is exported as
`koinon_install_binding{state}` (fixed set: same, copy, unknown; ADR 0023).

### 1. The importer only plans while held, and the release refuses deletions unless accepted

**Decided (procoduck, 2026-11-10): plan only while held; the release refuses a plan with deletions unless the
operator accepts them, audited.**

- **While held**, one importer pass computes the plan it would apply (apply, update, remove per kind) and writes
  nothing to the database. The plan is a dry-run mode of the same apply functions, not a second implementation, so
  the plan and the release cannot disagree. It logs `held: would <action> <kind> <name>` once per change, records
  one Kubernetes Event per Secret (reason `HeldNotApplied`), and exports
  `koinon_declarative_held_changes{kind,action}` (kinds: model-provider, mcp-server, connection; actions: apply,
  remove). `restore status` (decision 5) prints the plan.
- **The release check.** A release is still a rollout (ADR 0107 decision 3). The chart adds a `pre-upgrade` hook
  Job, `restore-release-check`, that runs whenever `restoreHold` is false. It reads the database: when the last
  recorded settings say `restore-hold=true` (ADR 0100), this upgrade is a release; otherwise the Job exits at once.
  On a release it computes the importer plan. When the plan holds a removal and the chart value
  `restore.acceptImporterDeletions` is not true, the Job fails with the list of removals, Helm stops the upgrade
  before any Deployment changes, and the install stays held. With the value set, the Job records
  `restore.release` in the install audit chain with the accepted plan (its hash and the counts per kind and
  action), and the upgrade goes on. The value is read only by this Job and only on a release, so leaving it set
  does nothing later; the chart NOTES say to remove it.
- The drill sets `restore.acceptImporterDeletions: true` on purpose on its fresh copy, and has one spec that a
  release without it fails and leaves the hold on.

Alternatives: apply additive changes and skip only removals while held (the copy still writes, so the drill cannot
assert "no write while held"); not run the importer while held (the operator does not see the removals coming); a
check inside koinon-api at its first unheld start (too late: agentd and the reconciler are already released while
koinon-api refuses).

In place: the Secrets are there and the plan is usually empty, so the check passes without the value. Fresh copy:
the plan lists every source Secret as a removal; the operator applies the Secrets (DR) or accepts the removals (drill,
staging).

### 2. The release of an in-place restore signs everyone out

**Decided (procoduck, 2026-11-10).** A session or token revoked after the dump time is valid again after an
in-place restore, because the revocation was in the lost data. The release check Job (decision 1) therefore
revokes every session on every release, audited as `restore.sessions_revoked` with the count. On a copy the
sessions are already refused (decision 3); the revoke makes that permanent. Users log in again.

API tokens are **not** revoked by the release. `restore status` lists the tokens that were active at the dump time
(not revoked, not expired, with `last_used_at` and creator) so a platform admin can revoke the ones that were
revoked or lost after the dump. Whether the in-place release should also quarantine tokens is open (Q1).

### 3. Source API tokens are refused on a copy; a platform admin can re-enable them

**Decided (procoduck, 2026-11-10): refuse on the copy, audited re-enable by a platform admin; sessions never come
back.**

- `api_tokens` and `sessions` get a nullable `install_mark` (the hash). A new row is stamped with the creating
  process's mark. Authentication refuses a row whose mark is not the process's own mark, with the reason
  `issued by another install (restored copy)`, audited as `token.use_failed` and counted in
  `koinon_auth_quarantined_total{kind}` (kind: token, session).
- A refused token is re-enabled only by a platform admin: the `adopt-restore` recovery action (all tokens, decision
  6), or a per-token `RebindToken` on the platform admin API by a platform admin signed in on the copy. Both are
  audited (`token.rebind`, with the old and the new mark hash). Org owners and org admins cannot re-enable a token.
- A refused session is never re-enabled. The user logs in again, which makes a session bound to the copy (password
  login works, the hash came with the dump; OIDC login depends on decision 4).
- Legacy rows (no mark) count as own only in state `same`; an unheld start in state `same` stamps them, as ADR 0106
  claims unmarked objects. In `copy` and `unknown` they are refused.
- Cost: two nullable columns (expand-only, allowed in a patch line, ADR 0085) and one comparison per request.

Effect per restore shape:

| Shape | Tokens | Sessions |
|---|---|---|
| In place (same mark) | work; listed in `restore status` (Q1) | all revoked at release (decision 2) |
| Fresh copy, new mark (drill, staging) | refused, "restored copy"; re-enable by a platform admin | refused, then revoked at release |
| Fresh namespace, mark backup applied (DR) | work | revoked at release |
| Fresh namespace, mark lost (DR) | refused until `adopt-restore` | refused, then revoked at release |

Alternatives: key the hashes with the mark (HMAC, the Rails `secret_key_base` pattern; a lost mark has no way back,
the reason reads "wrong secret", every hash is rewritten); revoke every token on restore (final, so a DR replacement
that lost its mark loses every CI and integration token at once).

### 4. A copy does not write to the source's checkpoints or send logins to its URL

The chart cannot see the source's values, but the database knows them: `install_binding` holds the source's outward
targets (decision 0).

- **Runtime guard.** In state `copy`, each binary compares its own targets with the recorded ones. An equal
  checkpoint endpoint and bucket puts agentd in **restore-only** checkpoint mode: it reads snapshots (archived homes
  still come back), but it creates none, applies no retention and runs no kopia maintenance. An equal public URL or
  OIDC redirect URL turns OIDC login off (`/auth/oidc/*` answers 409 with the reason; password login stays). Each
  conflict is logged once and exported as `koinon_restore_isolation_conflict{target}` (target: checkpoints,
  public-url, oidc-redirect). Held or released, the guard holds until the value changes or `adopt-restore` runs.
- **Chart switch.** A chart value `checkpoints.readOnly` (default false) sets restore-only mode on agentd for any
  install, and NOTES.txt warns when `restoreHold` is true with checkpoints on and `readOnly` false. A new
  `hack/chart-check.d` file asserts both.

While held, agentd already writes no checkpoint (ADR 0107: it only serves `checkpoints resync`, which lists). This
decision is about what happens after the release. In place and DR with the mark backup: state `same`, no guard.
Fresh copy: the guard fires on every target left at the source's value.

Alternatives: chart only, with operator-typed `restore.source.*` values compared at render (a forgotten value turns
the guard off); runtime only (no way to choose read-only before the first start).

### 5. One audited admin switch disables any outward target

**Decided (procoduck, 2026-11-10): one pattern for all outward targets.**

- The outward targets are: MCP servers, model providers, code-host connections, trace destinations and
  notification targets. Each table (`mcp_servers`, `providers`, `connections`, `trace_destinations`,
  `platform_trace_destination`, and the notification target rows) gets the same columns: `admin_disabled_at`,
  `admin_disabled_by`, `admin_disabled_note`. They are separate from system states such as `disabled_reason` or
  `removed_at`, so re-declaring or un-shadowing an item never clears an admin's disable.
- One Go package (`internal/outward`) owns the switch: a `Target` kind enum, one `SetDisabled(kind, id, disabled,
  note)` service, one audit action pair `outward.disable` and `outward.enable` with `targetKind`, and one check
  `outward.Enabled` that every caller of a target uses before it dials. A disabled target is not called, not probed,
  and not offered to runs (its tools, its models); it keeps its bindings and its credential.
- Surfaces: one admin REST and Connect method (`SetOutwardTargetDisabled`), a switch on each target's UI page, and
  the declarative field `disabled: true` for the kinds that have declarative Secrets (providers, MCP servers,
  connections; the Secret wins, and the UI cannot enable an item its Secret disables).
- **Disable is allowed while held** (it reaches nothing). **Enable is refused while held** with `restorehold.ErrHeld`,
  because an enabled target may be dialled (preflight, probe).
- `restore status` lists every outward target with its enabled or disabled state (decision 7).

Alternatives: a flag on MCP servers only (#516 as filed; the other kinds keep the same leak); reusing
`disabled_reason` (mixes admin intent with system state); one install-wide "outward off" switch (the hold again
under another name, and it cannot keep one safe target on).

### 6. Platform admin recovery is a Kubernetes request object handled by the reconciler

**Decided (procoduck, 2026-11-10).** Break-glass and `adopt-restore` are platform admin jobs, never an org owner's.
They must not need `kubectl exec`: the images are distroless and clusters can forbid exec. The route is a
one-shot request object.

- **The object.** A namespaced custom resource `PlatformAdminRecovery` (group from `internal/project`, e.g.
  `recovery.koinon.dev/v1alpha1`) in the release namespace. Its CRD ships in the chart. Spec:
  - `action`: `mint-admin-token`, `reset-password` or `adopt-restore` (a closed enum; a new action is a new
    decision);
  - `user` (for `reset-password`; for `mint-admin-token` the platform admin user the token is for, default the
    bootstrap admin);
  - `requester` (the Kubernetes username) and `requesterGroups`;
  - `nonce` (16 to 64 characters, chosen by the requester);
  - `ttl` (at most 1 h; default 15 min).
- **Who may run it.** Kubernetes RBAC decides: `create` on `platformadminrecoveries` in the namespace. The chart
  renders a Role `<fullname>-platform-recovery` and binds it to nobody (`recovery.subjects` in values, empty by
  default). Nothing in the Koinon database grants the right.
- **How the reconciler knows the requester.** A native **ValidatingAdmissionPolicy** (never Kyverno) on the CRD
  refuses a CREATE whose `spec.requester` is not `request.userInfo.username` or whose `spec.requesterGroups` is not
  `request.userInfo.groups`, and refuses every UPDATE of `spec`. So the identity in the object is the one the API
  server authenticated, recorded at admission; a requester cannot name someone else. The Kubernetes API server audit
  log is a second record but is not required. (A MutatingAdmissionPolicy could stamp the field instead, but it is
  not GA on every supported Kubernetes version; validation needs only VAP.)
- **What the reconciler does.** It watches the CRD in its own namespace. It refuses an object older than its `ttl`
  or whose `nonce` is already in the new table `recovery_requests` (nonce, CR UID, action, requester, result), so a
  re-applied manifest (a GitOps sync, a copy-paste) does nothing. It then runs the action with the functions #512
  planned for the CLI, which become internal actions of a shared package (`internal/recovery`) and not a pod-exec
  command:
  - `mint-admin-token`: a platform admin API token, TTL at most 24 h, bound to the current mark;
  - `reset-password`: a one-time password with a forced change at the next login; all the user's sessions revoked;
  - `adopt-restore`: rewrites `install_binding` with this install's mark and targets (state `same`), and re-enables
    every refused token that is not revoked or expired (`token.rebind`).
- **Audit.** Each action is one install audit chain event (`recovery.<action>`), actor
  `kubernetes:<requester>` with the groups, the CR UID and the nonce; the token or password itself is never in the
  event.
- **The result.** A secret result goes into a Secret `<cr-name>-result` with an owner reference to the CR, plus a
  Role and RoleBinding that give `get` on that one Secret (`resourceNames`) to the requester's username only. The
  reconciler's own RBAC grows by `create` and `delete` on Secrets, Roles and RoleBindings in its namespace and the
  `get` it grants (RBAC escalation rules: it can grant only what it holds). The status says `Done` or `Refused` with
  the reason. The reconciler deletes the CR, the Secret and the Role after the `ttl`.
- **Not in Git.** The guide says to apply it with `kubectl create`, never from a Git repository; the nonce and the
  TTL make a synced copy inert, and the VAP refuses an object without `requester` equal to the creating user, which
  a GitOps controller's service account would fail unless it is bound to the Role.
- **Works with the API down and while held.** The reconciler talks to Postgres directly and needs no koinon-api.
  Under the hold it serves only this handler, as agentd serves `checkpoints resync` (an ADR 0107 amendment). It needs
  the database: a database that is down is a different recovery.
- **Removed routes.** There is no org-owner API for adopt or break-glass, and the pod-exec CLI is not a supported
  route. Per-token `RebindToken` stays on the platform admin API (decision 3) for a platform admin who can already
  sign in.

Alternatives: a CLI in the api pod (#512 as filed; needs exec, which distroless images and cluster policy may
forbid);
a Helm value plus a Job (`--set recovery.resetPassword=…`; the value lands in the release history and in Git, and it
is not one-shot); an org-owner API call (an org role must not take over the install).

### 7. `restore status` shows what the release will do

One status function behind a platform admin REST endpoint (`GET /admin/v1/restore/status`, JSON for the drill) and
the CLI. It reads only the database and the cluster, never outward. It also works without the API: the
`restore-release-check` Job prints the same report in its log. Sections:

- the binding state and its reason; refused token and session counts;
- the tokens active at the dump time (decision 2);
- every outward target (decision 5) with its enabled or disabled state and its key fingerprint, marked when it
  equals a recorded source target; the checkpoint target, the OIDC issuer and redirect URL, the public URL;
- the importer plan (decision 1) and whether the release needs `restore.acceptImporterDeletions`;
- the release preview, per org: the schedule triggers due for a catch-up fire (at most one each, look-back 1 h), the
  DBOS workflows `ENQUEUED` and `PENDING` (read from the `dbos` schema without a runtime), parked runs, and the org
  run and trigger limits that will apply to them.

Alternatives: CLI only (the drill and a future UI need JSON); extend `keys verify` and `org-limits show` (no whole
picture).

## Drill specs this turns from known gap to required (#514)

| Item | Label | Spec |
|---|---|---|
| 1 | `hold` | While held, a declared provider of the source is still present; the database has no importer write; a `HeldNotApplied` Event exists. |
| 1 | `hold` | A release without `restore.acceptImporterDeletions` fails the upgrade and the hold stays on; with it, the removals happen once and `restore.release` is audited. |
| 2 | `inplace` | In `backup-restore.sh`, every session made before the dump is refused after the release; a token issued before the dump still works and is listed in `restore status`. |
| 3 | `isolation` | A source session cookie and a source API token are refused on the copy with the reason "restored copy"; a new login works; `RebindToken` by a platform admin makes one token work, audited; an org owner's rebind is refused. |
| 0 | `mark` | `koinon_install_binding{state="copy"}` is 1 on the copy and `same` on an in-place restore. |
| 4 | `isolation` | With the source's checkpoint values, the copy reports a checkpoints conflict, and the source's snapshot list is unchanged after release (phase 2, needs the kind S3); with the source's public URL, OIDC login is refused. |
| 5 | `isolation` | Each outward kind disabled while held is not called after the release (the fake sinks get no request); enable while held is refused. |
| 6 | `isolation` | A `PlatformAdminRecovery` with another user's name in `requester` is refused at admission; `mint-admin-token` works while held and with koinon-api scaled to 0; a re-applied nonce does nothing; the result Secret is readable only by the requester. |
| 7 | `hold` | `restore status` lists the seeded targets, the due catch-up fire and the N-1 queued runs, and these match what happens after the release. |

## Prior art

- **Vault DR secondaries.** A DR secondary serves no client requests until it is promoted, and promotion needs a
  DR operation token made from unseal or recovery keys, an out-of-band credential. Tokens replicate, so a promoted
  DR secondary accepts the primary's tokens: right for a replacement, which is our state `same` (mark backup
  applied) and `adopt-restore`. https://developer.hashicorp.com/vault/tutorials/disaster-recovery/break-glass-failover
- **GitLab Geo.** A secondary must have the primary's `gitlab-secrets.json`, so it shares identity by design and
  forwards writes to the primary; a feature request asks for a warning on a secrets mismatch (gitlab#6080). Shared
  identity is right for a planned failover target, wrong for a drill copy.
  https://docs.gitlab.com/administration/geo/replication/configuration/ , https://gitlab.com/gitlab-org/gitlab/-/issues/6080
- **Discourse.** A restore sets "disable emails" to non-staff by default, so a staging copy does not mail real
  users; `--disable_emails=false` opts out. The fail-safe default for outward effects matches decisions 1 and 5.
  https://meta.discourse.org/t/when-restoring-a-backup-the-disable-email-will-be-set-to-non-staff-from-no/115143
- **Mattermost.** A test server from a production backup needs its own Site URL, or links and notifications point
  at production. Decision 4's public URL guard. https://docs.mattermost.com/configure/environment-configuration-settings.html
- **Rails per-environment credentials and `secret_key_base`.** Cookies signed in one environment do not validate in
  another; changing the key logs everyone out. This is the rejected HMAC option of decision 3.
  https://blog.saeloun.com/2023/08/11/rails-7-1-store-secret-key-base-in-rails-config/
- **Velero restore resource modifiers and restore hooks.** Velero rewrites resources on restore with JSON patches
  and runs hooks in restored pods. Decision 4's chart switch is the same idea; the runtime guard catches the case
  where the operator did not. https://velero.io/docs/main/restore-resource-modifiers , https://velero.io/docs/main/restore-hooks
- **Platform admin recovery (decision 6).** Argo CD resets the admin password by editing `argocd-secret` (Kubernetes
  RBAC on a Secret decides who may); Gitea's Helm chart `admin.passwordMode: keepUpdated` re-applies the admin
  password from a value on every start; Keycloak creates a temporary admin with a bootstrap-admin command, run as a
  Job; Vault's `operator generate-root` makes a new root token from a quorum of unseal keys, with a nonce and a
  one-time pad. All use an out-of-band credential that the database does not hold, as we do with Kubernetes RBAC.
  We found no product that uses a one-shot request object for this; ours combines the Kubernetes RBAC check of Argo
  CD with the nonce and one-time result of Vault.

## Consequences

- Positive: a copy cannot act with the source's credentials or write to its checkpoints, from its first request;
  the operator sees what the release will do; a release cannot delete declared items without an audited accept;
  recovery works without exec, without koinon-api and while held; the drill asserts isolation instead of noting a
  gap.
- Negative: koinon-api gets a new startup input (the mark), so a lost mark Secret now also refuses tokens and
  sessions on the live install until `adopt-restore`. The mark backup (ADR 0106) becomes more important.
- Negative: every release signs everyone out, also a release with no data loss (a hold kept only for a check).
- Negative: an operator on a copy cannot use a source API token; automation against a copy needs a new token.
- The chart gets its first CRD and a ValidatingAdmissionPolicy for it, and the reconciler more RBAC in its namespace
  (Secrets, Roles, RoleBindings). `make test-chart-smoke` and `make test-admission` gain cases.
- Schema: new tables `install_binding` and `recovery_requests` and nullable columns only (expand-only; allowed in a
  patch line, ADR 0085).
- ADR 0107 is amended: the importer no longer writes while held (only the purger and the audit anchor do); the
  reconciler serves recovery requests while held; a release runs the `restore-release-check` Job; the release of a
  copy keeps the guards of decisions 3 and 4 until `adopt-restore`.
- #512 changes shape: its CLI functions become the internal actions of decision 6.
- A dump taken before this ADR, restored unheld, is claimed silently (state `unknown`, unheld). The runbook already
  starts every restore held.

## Open questions (procoduck)

- Q1: does the release of an in-place restore also refuse (quarantine) the API tokens active at the dump, until a
  platform admin re-enables them, or only list them (the current decision)?
- Q2: for `mint-admin-token` with no platform admin user left (deleted, or OIDC-only), does the action create a
  temporary platform admin user (the Keycloak pattern), or must it name an existing one?

## Links

- #516, #514, #512, #275; friction notes https://claude.ai/artifact/BZfuPayDnc1QEfaYSsjuJ4
- `internal/declarative/declarative.go`, `internal/mcpservers/declared.go`, `internal/tokens/tokens.go`,
  `internal/auth/session.go`, `internal/restorehold/restorehold.go`, `internal/execution/names.go`,
  `charts/koinon/values.yaml` (checkpoints, publicUrl, installMark, restoreHold, oidc),
  `charts/koinon/templates/key-admin.yaml` (the Job pattern for the release check)
