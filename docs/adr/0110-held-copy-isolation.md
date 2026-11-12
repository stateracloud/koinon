# 0110. A restored copy does not act as its source

- Status: Accepted (decided by procoduck, 2026-11-10). Decisions 1, 2, 3, 5 and 6 record his answers; decision 0,
  the guards of decision 4 and decision 7 are the design they rest on.
- Date: 2026-11-10
- Area: install-operations
- Tags: backup-restore, identity, auth, openshell, mcp
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
| present | different, or no mark (unbound) | `copy` | Decisions 3 and 4 apply. The row is never overwritten automatically. |
| absent | any | `unknown` | Held: the process writes an **unbound** row (mark NULL), so the state becomes `copy` and stays `copy` after the release. Unheld: an upgrade of a live install; it claims the row (as ADR 0106 decision 4). |

- **Held processes re-assert the hold every 30 seconds.** A held process checks the row at start and then every 30
  s (a fixed interval in `internal/restorehold`, the same in all three binaries), not only at start, because the
  restore guide starts the chart held and then loads the dump, which replaces the table and the audit chain. On
  each check it writes the unbound row when the row is absent, and sets `install_binding.held_since` when it is NULL
  **and its own process start time is after `install_binding.released_at`** (or `released_at` is NULL). Only the
  release step (decision 1) clears `held_since`, and it sets `released_at` and `released_by` (the `OwnerID` of the
  releasing process's mark). So an old held pod that is still running during the rolling release cannot set
  `held_since` again after the new koinon-api released, and the sessions are revoked once.
- **After a load, the hold is not visible for up to 30 s.** In that window the database carries only the dump's
  rows. For a **copy** this does not matter: the dumped row carries the source's mark and the source's
  `released_by` (or NULL), never this install's mark, so decision 1 counts the release as pending from the first
  read, held or not, until this install's own release step sets `released_by`. For an **in-place** restore (state
  `same`) nothing in the database tells the window from a normal upgrade, so the runbook is the guard there: after
  the load, run `restore status` until it shows `held_since` set, then release. No chart Job refuses an upgrade on
  this rule: an absent row (the first upgrade of an existing install) and a live install that lost its mark must
  stay upgradable, security fixes included.
- **Who writes what.** Each binary writes only its own columns: koinon-api the public URL and the OIDC redirect
  URL, agentd the checkpoint endpoint and bucket; any binary the mark (only by the claim) and `held_since`. Every
  write is guarded by the state: the targets are refreshed only in state `same`. The claim is
  `INSERT … ON CONFLICT DO NOTHING` followed by a re-read, so two processes that start at once agree on one row and
  compute the state from what is stored, not from what they tried to write.
- A dump taken before this ADR and restored **unheld** is still claimed silently (state `unknown`, unheld). The
  runbook starts every restore held, and a held start makes the row unbound.

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
- **A release is pending** while any of these holds:
  - `install_binding.held_since` is set;
  - a `restore-hold=true` settings record (ADR 0100) is newer than the newest `restore.release` event;
  - the state is `copy` and `install_binding.released_by` is not this install's `OwnerID`.

  The third clause catches a fresh copy inside the 30 s window (decision 0) and ends with the copy's own release
  step, so a copy that stays `copy` until `adopt-restore` is not pending at every start. A live install that lost
  its mark Secret is pending once and runs the audited release step once (its sessions are revoked; see
  Consequences). An absent row (`unknown`, the first upgrade of an existing install) is not pending, so that upgrade
  is never blocked. A live install with its mark has none of these.
- **The release step** runs once per hold, in koinon-api, at its first unheld start while a release is pending (one
  replica wins: the step holds a row lock on `install_binding` and re-checks "pending" under it). In one
  transaction it:
  - **always** revokes every session and records `restore.sessions_revoked` with the count (decision 2), also when
    removals wait for an accept, so an unheld koinon-api never serves the sessions of the dump;
  - computes the importer plan and writes each removal to the table `restore_accepted_removals` (kind, org, Secret
    name, item ID, release ID, state), with state `accepted` when `restore.acceptImporterDeletions` is true (a chart
    value passed to koinon-api and to the Job) and `waiting` otherwise;
  - records `restore.release` in the install audit chain with the plan hash, the list of removals and their state;
  - clears `held_since`, sets `released_at` and `released_by`, and replaces the list of any earlier release (a
    successful step clears the old list).
- **The importer is the gate, also after the release.** While a release is pending, removals are plan-only. After
  it, an item on `restore_accepted_removals` is removed only in state `accepted`, and the removal and the delete of
  its row are one transaction (a crash leaves both or neither); in state `waiting` the removal stays plan-only and is logged (`restore: removal of <kind> <name>
  waits for restore.acceptImporterDeletions`), however long ago the release was. When the Secret comes back, its row
  is deleted and nothing is removed. Items not on the list (declared after the release) follow the normal rules, so
  the list blocks only what the restore brought in.
- **A later accept.** When `waiting` rows exist and koinon-api starts with `restore.acceptImporterDeletions` true, it
  turns them to `accepted` in one audited step (`restore.removals_accepted`, with the list). It revokes no session:
  that was done once, at the release step.
- **No change after the accept (TOCTOU).** The step computes the plan and records the list in the same transaction,
  so what is accepted is what is on the list. A removal that is not on the list is never applied under the accept.
- **Metric.** `koinon_restore_release_pending` is exported by koinon-api only. It is 1 while unheld with a pending
  release or with `waiting` rows, and 0 while held (the hold has its own gauge and alert). The chart alert
  `KoinonRestoreReleasePending` fires when it is 1 for 15 minutes.
- **The `restore-release-check` Job is a preview and an early stop.** The chart renders it whenever `restoreHold` is
  false. Under Helm it is a `pre-upgrade` hook with `helm.sh/hook-weight: "10"`, after the migrate Job
  (`charts/koinon/templates/migrate-job.yaml`, weight `"0"`). Under Argo CD it carries `argocd.argoproj.io/hook:
  Sync` and `argocd.argoproj.io/sync-wave: "2"`, after the migrate Job's wave `"1"`. It exits 0 when the schema or
  the audit table does not exist yet (a first install; Argo CD's hooks run then too) and when no release is pending
  (decision 1's rule, the same function koinon-api uses). On a pending release it prints the plan and fails only
  when the plan has a removal and the value is not true, so `helm upgrade` stops before any Deployment changes and
  the install stays held. It refuses nothing else; in particular it never blocks an upgrade because of the
  binding state. Under
  Argo CD the Deployments (wave 0) are applied before wave 2, so there the Job is only a preview; with
  `--no-hooks` or a plain `kubectl apply` it does not run at all. The importer gate above holds in every case.
- **Leaving the value set is not harmless.** It is read on a pending release and at every koinon-api start while
  `waiting` rows exist, so a value left set auto-accepts at the next start every removal that waits. NOTES.txt
  prints a warning on every render with the value true ("restore.acceptImporterDeletions is set: every waiting
  removal of a restore is accepted at the next start; remove it after the release"), and `restore status` shows it.
- The drill sets `restore.acceptImporterDeletions: true` on purpose on its fresh copy, and has one spec that a
  release without it fails under Helm, leaves the hold on, and that the removals stay `waiting` (and the sessions
  are still revoked) when the Job is
  skipped (`--no-hooks`).

Alternatives: apply additive changes and skip only removals while held (the copy still writes, so the drill cannot
assert "no write while held"); not run the importer while held (the operator does not see the removals coming); the
hook Job as the only gate (it is skipped by `--no-hooks` and `kubectl apply`, and under Argo CD it runs after the
Deployments, so it can only preview; the importer gate holds in every case); keeping koinon-api held until the
accept (agentd and the reconciler would already be released, so the install would be half released).

In place: the Secrets are there and the plan is usually empty, so the check passes without the value. Fresh copy:
the plan lists every source Secret as a removal; the operator applies the Secrets (DR) or accepts the removals (drill,
staging).

### 2. The release of an in-place restore signs everyone out

**Decided (procoduck, 2026-11-10).** A session or token revoked after the dump time is valid again after an
in-place restore, because the revocation was in the lost data. The release step (decision 1, in koinon-api, so
also under Argo CD and `--no-hooks`) therefore revokes every session on every release, audited as
`restore.sessions_revoked` with the count. On a copy the
sessions are already refused (decision 3); the revoke makes that permanent. Users log in again.

API tokens are **not** revoked by the release. `restore status` lists the tokens that were active at the dump time
(not revoked, not expired, with `last_used_at` and creator). The **dump time** is the timestamp of the newest audit
or install chain event that is older than the first held record (`install_binding.held_since`, or the oldest
`restore-hold=true` settings record after the newest `restore.release`); it is an upper bound, since the data can
be a little older than its last event. With no held record (a restore started unheld, or the 30 s window of decision
0) the dump time is unknown: `restore status` says so and lists every active token. The dump time is used only for
this list; nothing refuses or blocks on it. They are **not** refused (procoduck, 2026-11-10). A
platform admin re-checks the list, and the runbook says to revoke each token that was revoked or reported lost since
the dump.

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
| In place (same mark) | work; listed in `restore status` for a platform admin to re-check (decision 2) | all revoked at release (decision 2) |
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
- **Alert.** The chart rule `KoinonRestoreIsolationConflict` fires when `max(koinon_restore_isolation_conflict) ==
  1` for 10 minutes (severity warning). It also catches the case below, where a live install that lost its mark
  Secret turns the guards on against itself.
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
  `platform_trace_destination`) gets the same columns. Koinon has no outward notification target yet:
  `notifications` is the in-app inbox (`00013_prices_and_budgets.sql`), which reaches nothing outside. A future outward sink (Slack, a
  webhook) uses this switch from its first migration. The columns are: `admin_disabled_at`,
  `admin_disabled_by`, `admin_disabled_note`, and for the kinds with declarative Secrets `admin_disabled_under_by` and
  `admin_disabled_under_note`: when a Secret's `disabled: true` claims an item an admin disabled (or an admin disables an item
  the Secret already holds disabled), the admin's actor and note are kept there, and the disable comes back to the
  admin, still disabled and audited, when the Secret drops the field. They are separate from system states such as `disabled_reason` or
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
  `recovery.koinon.dev/v1alpha1`) in the release namespace. Its CRD ships in the chart's `crds/` directory (see
  Consequences: one CRD per cluster, shared by every release). Spec:
  - `action`: `mint-admin-token`, `reset-password` or `adopt-restore` (a closed enum; a new action is a new
    decision);
  - `user` (for `reset-password`; for `mint-admin-token` the platform admin user the token is for, default the
    bootstrap admin);
  - `requester` (the Kubernetes username) and `requesterGroups`;
  - `nonce` (16 to 64 characters of `[A-Za-z0-9_-]`, chosen by the requester; the policy refuses any other
    character, so the nonce is safe in a user name, a log line and a label);
  - `ttl` (at most 1 h; default 15 min), measured from `metadata.creationTimestamp` only (the API server sets it;
    nothing in `spec` or `status` moves it);
  - `recipientPublicKey`: an age X25519 recipient (`age1…`) the requester made for this request; required for the
    actions with a secret result (`mint-admin-token`, `reset-password`).

  The CRD has a `status` subresource, so the reconciler's status writes do not go through the spec rule below.
- **Who may run it.** Kubernetes RBAC decides: `create` on `platformadminrecoveries` in the namespace. The chart
  renders a Role `<fullname>-platform-recovery` and binds it to nobody (`recovery.subjects` in values, empty by
  default). Nothing in the Koinon database grants the right.
- **How the reconciler knows the requester.** A native **ValidatingAdmissionPolicy** (never Kyverno) on the CRD
  refuses a CREATE whose `spec.requester` is not `request.userInfo.username` or whose `spec.requesterGroups` is not
  `request.userInfo.groups`, and refuses every UPDATE of `spec`. So the identity in the object is the one the API
  server authenticated, recorded at admission; a requester cannot name someone else. The Kubernetes API server audit
  log is a second record but is not required. (A MutatingAdmissionPolicy could stamp the field instead, but it is
  not GA on every supported Kubernetes version; validation needs only VAP.) The policy also refuses a `ttl` over
  1 h, a missing or malformed `nonce`, and a secret-result action without `recipientPublicKey`.
- **One policy per release.** Each release renders its own policy and binding with a release-scoped name
  (`<namespace>-<fullname>-platform-recovery`, cut to 63 characters with a hash suffix), and the binding's
  `matchResources.namespaceSelector` selects only the release namespace (`kubernetes.io/metadata.name`). Two
  releases on one cluster (the drill copy next to the live install, ADR 0106) never render the same cluster-scoped
  name, and each policy judges only its own namespace's requests.
- **The policy must be in force, or nothing runs.** A VAP fails open when its binding is missing or changed, and it
  does not judge objects made before the binding. So before each request the reconciler reads the policy and its
  binding (it holds `get` on these two named objects only) and refuses the request when either is missing, when the
  binding's `validationActions` is not exactly `[Deny]`, when the policy's `failurePolicy` is not `Fail`, when the
  binding's `policyName` is not this release's policy, when its `matchResources` does not select the release
  namespace or excludes it (a changed `namespaceSelector`, an `excludeResourceRules` entry, an `objectSelector`), or
  when the CR's `creationTimestamp` is older than the binding's. The refusal reason names the failed check. The
  `get` is a ClusterRole limited to those two `resourceNames`, the same shape as the reconciler's read of the
  OpenShell config (`charts/koinon/templates/reconciler-openshell-config.yaml:5-29`).
- **What the reconciler does.** It watches the CRD in its own namespace. It refuses an object whose
  `creationTimestamp` plus `ttl` has passed or whose `nonce` is already in the new table `recovery_requests` (nonce,
  CR UID, action, requester, result), so a re-applied manifest (a GitOps sync, a copy-paste) does nothing. It then
  runs the action with the functions #512 adds for its CLI (password reset and token creation), which move into a
  shared package (`internal/recovery`) as internal actions and are no longer a pod-exec command:
  - `mint-admin-token`: a platform admin API token, TTL at most 24 h, bound to the current mark. When no platform
    admin user is left (deleted, or OIDC-only with no admin group member), it creates a **temporary platform admin**
    (the Keycloak pattern; procoduck, 2026-11-10). Users have no disable state (`00001_initial.sql`), so the
    temporary part is the grant, not the user: a new user `recovery-<nonce prefix>` (the `users` columns that are
    NOT NULL are filled like any new user's: the fixed issuer `koinon:recovery` with the CR UID as subject, so two
    temporary admins never collide and no IdP can log in as one, and a new personal org) and a `platform_admins` row with
    the new source value `recovery` (enum `platform_admin_source`, `00018`) and a new nullable `expires_at` (default
    now + 24 h, `spec.temporaryFor` at most 72 h). The UI shows a "temporary" badge on that admin and a banner to
    every platform admin, and every audit event of that user carries `temporary: true`. The grant ends at
    `expires_at`, or at once when a platform admin whose grant is not `recovery` exists. Then the reconciler
    deletes the grant and revokes that user's tokens and sessions (`recovery.temporary_admin_expired`); the user
    row stays for the audit trail;
  - `reset-password`: a one-time password with a forced change at the next login; all the user's sessions revoked;
  - `adopt-restore`: rewrites `install_binding` with this install's mark and targets (state `same`), and re-enables
    every refused token that is not revoked or expired (`token.rebind`). It does not need to set `released_by`: the
    state is `same` after it, so the `copy` clause of decision 1 no longer applies. It sets it anyway, to this
    install's `OwnerID`, so the row never names another install as the releaser. It lists the isolation conflicts it clears
    (decision 4: checkpoints, public-url, oidc-redirect, each with the old and the new value as `kindURL`) in the
    audit event and in the CR status, so the operator sees what the adopt turned back on.
- **Audit.** Each action is one install audit chain event (`recovery.<action>`), actor
  `kubernetes:<requester>` with the groups, the CR UID and the nonce; the token or password itself is never in the
  event.
- **The result: only the requester can read it.** This is how procoduck's decision ("a Secret only the requester
  can read") is carried out. A namespaced Secret does not do it: anyone with `get secrets` in the namespace reads
  it, and Roles that the reconciler makes on the fly would be a new escalation path (it already holds create, get,
  update and delete on every Secret, `charts/koinon/templates/rbac.yaml`). So the reconciler seals the token or
  password to `spec.recipientPublicKey` (age, X25519) and writes the armored ciphertext to `status.sealedResult`. Only
  the holder of the private key, which never leaves the requester's machine, can open it; the guide shows
  `age-keygen` and `age -d`. This is the Vault `generate-root` pattern (the result is encrypted to a key or a
  one-time pad the requester holds). No Secret, Role or RoleBinding is created, and the reconciler gets no RBAC
  beyond the CR, its status and the `get` on the policy and binding. The status says `Done` or `Refused` with the
  reason. The reconciler deletes the CR after its `ttl`.
- **Not in Git.** The guide says to apply it with `kubectl create`, never from a Git repository; the nonce and the
  TTL make a synced copy inert, and the VAP refuses an object without `requester` equal to the creating user, which
  a GitOps controller's service account would fail unless it is bound to the Role.
- **Works with the API down and while held.** The reconciler talks to Postgres directly and needs no koinon-api.
  Under the hold it serves only this handler, as agentd serves `checkpoints resync` (an ADR 0107 amendment). It needs
  the database: a database that is down is a different recovery.
- **Removed routes.** There is no org-owner API for adopt or break-glass, and the pod-exec CLI is not a supported
  route. Per-token `RebindToken` stays on the platform admin API (decision 3) for a platform admin who can already
  sign in.

**Built so far (issue #512).** The request object works end to end for `mint-admin-token` and `reset-password`:
the CRD in the chart's `crds/`, the release-scoped VAP and binding, the requester Role (bound to
`recovery.subjects`), the `recovery_requests` table (`00086`), the `internal/recovery` handler and the reconciler's
poll loop (`internal/recovery/controller.go`; polling, not a watch, so it needs no informer and no `watch` verb). The
result is sealed with age to `spec.recipientPublicKey` and written to `status.sealedResult`. Not built yet, each its own
change: `adopt-restore` (#516), the temporary platform admin for an install with no platform admin left (a
`mint-admin-token` request then refuses with "not a platform admin"), and the `spec.temporaryFor` field. Also not built
yet: the `koinon_recovery_crd_ready` metric, the `KoinonRecoveryCRDNotReady` alert and the CRD-version annotation (a
missing CRD is silent today), and a `recovery.refused` audit event (a refusal is logged and written to the object's
status only).

Amendment to ADR 0100 decision 1.1a: with `recovery.enabled` the `koinon_reconciler` login is also a member of
`koinon_install_audit_writer`, because the reconciler appends the `recovery.<action>` install events and the API may be
down. It appends only those; the reconciler skips its "login can write the install audit chain" start-up warning in
that case. With `recovery.enabled=false` only koinon-api is a member, as before. `mint-admin-token` does not end the
user's existing sessions (only `reset-password` does); an operator who suspects a compromised account resets the
password.

Alternatives: a CLI in the api pod (#512 as filed; needs exec, which distroless images and cluster policy may
forbid);
a Helm value plus a Job (`--set recovery.resetPassword=…`; the value lands in the release history and in Git, and it
is not one-shot); an org-owner API call (an org role must not take over the install).

### 7. `restore status` shows what the release will do

One status function behind a platform admin REST endpoint (`GET /admin/v1/restore/status`, JSON for the drill) and
the CLI. It reads only the database and the cluster, never outward. It also shows whether a release is pending, the
accepted plan hash, and the `waiting` and `accepted` rows of `restore_accepted_removals` (decision 1). It also works without the API: the
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
| 6 | `isolation` | A `PlatformAdminRecovery` with another user's name in `requester` is refused at admission; `mint-admin-token` works while held and with koinon-api scaled to 0; a re-applied nonce does nothing; the result opens only with the requester's age key; with the policy binding deleted, the reconciler refuses the request. |
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
- Negative: **a live install that lost its mark Secret turns the guards on against itself.** Its new mark differs
  from the bound one, so the state is `copy`: its own targets equal the recorded ones, so checkpoints go
  restore-only (no new snapshots) and OIDC login is off, until a platform admin runs `adopt-restore` or applies the
  mark backup. The alerts `KoinonRestoreIsolationConflict` (decision 4) and the `koinon_install_binding{state="copy"}`
  gauge make this visible within minutes; it also runs the release step once (decision 1), which signs everyone
  out. It stays upgradable throughout. The backup and restore guide gets a "lost mark Secret" section, which also
  says: because the install is pending once, its importer queues the declarative removals of that pass as
  `waiting`, and the `restore-release-check` Job then fails each later `helm upgrade` whose plan holds one of them,
  until `restore.acceptImporterDeletions` is set (or the Secret is restored).
- Negative: every release signs everyone out, also a release with no data loss (a hold kept only for a check).
- Negative: an operator on a copy cannot use a source API token; automation against a copy needs a new token.
- **CRD.** The chart ships its first CustomResourceDefinition (`PlatformAdminRecovery`), in `crds/`. A CRD is
  cluster-scoped and one per cluster, while a cluster can hold several releases (the drill copy next to the live
  install). As a template, the second release would render the same object and Helm would refuse it as owned by the
  first; `resource-policy: keep` would also leave the first release's ownership annotations after uninstall, so a
  later install fails. A `recovery.crd.create` switch fixes that only for an operator who remembers it. Helm's
  `crds/` semantics match a shared API instead: install when missing, never owned by a release, never deleted.
  The cost: Helm does not upgrade a CRD in `crds/`. So the CRD changes only by adding fields (ADR 0085), each chart
  version names the CRD version it needs in an annotation, and the upgrade guide carries the
  `kubectl apply --server-side -f crds/` step. A missing or older CRD never stops the reconciler: it starts and
  works normally (a routine `helm upgrade --wait` must not crashloop until someone with cluster rights applies the
  CRD); only the recovery handler refuses, each request with the reason `CRD older than required (kubectl apply
  --server-side -f crds/)` or `CRD missing`, and the reconciler exports `koinon_recovery_crd_ready` (0 or 1, no
  labels; ADR 0023) with the chart alert `KoinonRecoveryCRDNotReady` after 30 minutes at 0. Argo CD applies
  `crds/` on every sync. Installers that cannot create CRDs use `helm install --skip-crds` (the guide says so;
  `crds/` ignores `recovery.enabled`, so this flag is the way to leave it out); recovery is then refused with the
  same reason. Uninstall leaves the CRD; the guide says how to delete it once no release is left.
- **Two releases on one cluster.** A new chart check (`hack/chart-check.d/99-platform-recovery.sh`) renders two
  releases in two namespaces and asserts that no cluster-scoped object name repeats (policy, binding,
  ClusterRole), that each binding selects only its own namespace, and that the CRD is not in the templates.
- **RBAC.** The chart renders the Role `<fullname>-platform-recovery` (`create`, `get`, `list`, `delete` on
  `platformadminrecoveries`) bound to the subjects in `recovery.subjects` only (empty by default). The reconciler's
  Role gains `get`, `list`, `watch`, `update`, `delete` on `platformadminrecoveries`, `update` on their `status`,
  and a cluster-scoped `get` on the one named ValidatingAdmissionPolicy and its binding (`resourceNames`; a
  ClusterRole of two names with a release-scoped name, like `reconciler-openshell-config.yaml:5-29`). It gets no
  new RBAC on Secrets, Roles or
  RoleBindings: the sealed result (decision 6) needs none, and the reconciler never makes RBAC objects. The CRD, the
  ValidatingAdmissionPolicy and its binding are the cluster-scoped objects the chart adds. `hack/chart-check.d` gets
  `99-platform-recovery.sh` and `99-restore-release.sh` (hook weight and Argo CD wave of the Job included).
- **Chart smoke.** New startup inputs: `KOINON_INSTALL_MARK` on koinon-api, the CRD the reconciler watches, and the
  `restore-release-check` Job. `make test-chart-smoke` asserts koinon-api starts with the mark, the reconciler
  starts and becomes ready with the CRD present, missing, or older than required (in the last two cases recovery
  requests are refused and `koinon_recovery_crd_ready` is 0), and the Job exits 0 on a first install (no schema
  yet), on the first upgrade of an existing install (no `install_binding` row), and on a plain upgrade.
- **Admission.** The ValidatingAdmissionPolicy is native (never Kyverno), `failurePolicy: Fail`, binding
  `validationActions: [Deny]`. `make test-admission` gets cases: a create with another user's `requester` or groups
  is refused; an update of `spec` is refused while a status update passes; a create with a `ttl` over 1 h, with no
  `nonce`, or a secret-result action with no `recipientPublicKey` is refused; a valid create is admitted. The
  reconciler-side check (policy and binding present and strict, CR newer than the binding) gets a unit test.
- Schema: new tables `install_binding` (with `held_since`, `released_at` and `released_by`), `restore_accepted_removals` and `recovery_requests`, the enum value `recovery` on
  `platform_admin_source`, a nullable `expires_at` on `platform_admins`, and nullable columns elsewhere only
  (expand-only; allowed in a patch line, ADR 0085).
- ADR 0107 is amended: the importer no longer writes while held (only the purger, the audit anchor and the hold
  re-assert of decision 0 do) and keeps the removals the restore brought in until they are accepted; the reconciler serves recovery
  requests while held; a release runs the release step in koinon-api and, under Helm, the `restore-release-check`
  Job first; the release of a copy keeps the guards of decisions 3 and 4 until `adopt-restore`.
- #512 changes shape: the functions it adds for its CLI become the internal actions of decision 6.

## Implementation

Each part is one pull request. The drill specs of its decisions move from known gap to required in the same PR.

- **(a) Outward disable switch** (decision 5). `internal/outward`, the `admin_disabled_*` columns on the five
  target kinds, `SetOutwardTargetDisabled` (REST and Connect), the UI switch, the declarative `disabled: true`, the
  hold rule (disable yes, enable no). Depends on nothing.
- **(b1) Install binding, token and session refusal, rebind and adopt** (decisions 0, 3). `install_binding` with
  the claim, the unbound row and the hold re-assert (`held_since`), the mark on koinon-api (chart smoke),
  `install_mark` on tokens and sessions, the refusal reason and metric, `RebindToken`, and the internal
  `adopt-restore` function. Depends on nothing.
- **(b2) Runtime guards** (decision 4). The target comparison in koinon-api and agentd, restore-only checkpoints,
  OIDC off, `koinon_restore_isolation_conflict` and its alert, `checkpoints.readOnly` and the NOTES warning.
  Depends on (b1).
- **(c) Importer gate, release step and `restore-release-check` Job** (decisions 1, 2). The dry-run mode of the
  importer, the `HeldNotApplied` Event and metric, the "release pending" rule, the release step in koinon-api (plan
  hash, session revoke, `restore.release`, `koinon_restore_release_pending` and its alert), the Job (Helm weight
  10, Argo CD wave 2, exit 0 without a schema), `restore.acceptImporterDeletions`, `restore_accepted_removals`,
  `released_at`, and the drill's accept flag. The Job prints the plan only; the full `restore status` report is
  added by (e). Depends on (b1) (`held_since`, `released_at`, the binding state).
- **(d) `PlatformAdminRecovery`** (decision 6). The CRD with its status subresource, the ValidatingAdmissionPolicy
  and the reconciler's check that it is in force, the Roles, the reconciler handler (also while held),
  `recovery_requests`, the sealed result, the temporary platform admin grant and its expiry, and the admission and
  chart-smoke cases, the two-release chart check, the CRD version check of the recovery handler (`koinon_recovery_crd_ready` and its alert). The age library (`filippo.io/age`) enters
  at a version at least 3 days old (ADR 0079, `hack/dep-age-check.sh`). Built on the password-reset and token
  functions of #512, moved into `internal/recovery`. Depends on #512, on (b1) for `adopt-restore`, and on (b2)
  because `adopt-restore` clears decision 4's conflicts and lists them.
- **(e) `restore status`** (decision 7). The status function, `GET /admin/v1/restore/status`, the full report in the
  release-check Job's log (added to (c)'s Job), the tokens-at-dump list, the `waiting` removals. Depends on (a), (b1), (b2) and (c) for its sections; it can land
  first with those sections empty and fill them as they merge.

## Open questions

None. procoduck answered the last two on 2026-11-10 (decisions 2 and 6).

## Links

- #516, #514, #512, #275; friction notes https://claude.ai/artifact/BZfuPayDnc1QEfaYSsjuJ4
- `internal/declarative/declarative.go`, `internal/mcpservers/declared.go`, `internal/tokens/tokens.go`,
  `internal/auth/session.go`, `internal/restorehold/restorehold.go`, `internal/execution/names.go`,
  `charts/koinon/values.yaml` (checkpoints, publicUrl, installMark, restoreHold, oidc),
  `charts/koinon/templates/migrate-job.yaml` (the hook weight and Argo CD wave the release check follows),
  `charts/koinon/templates/rbac.yaml`, `internal/db/migrations/00018_*.sql` (`platform_admin_source`)
