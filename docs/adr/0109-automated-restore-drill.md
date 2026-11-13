# 0109. Automated restore drill

- Status: Accepted
- Date: 2026-11-10
- Area: install-operations
- Tags: backup-restore, testing, e2e
- Amends: [0085](0085-upgrade-compatibility-and-recovery-policy.md) (decisions 15 and 18, by reference)
- Relates to: [0106](0106-install-mark-owns-openshell-objects.md) (the install mark), [0107](0107-restore-hold-mode.md) (restore hold),
  [0105](0105-merge-queue-route.md) (the only merge evidence is the queue; the drill is not a merge check)
- Issues: closes #514 (phase 1); refs #275, #516 (held copy has no outward effects), #515 (archive and dead-executor, phase 2), #512 (lab sign-in)

## Context

ADR 0085 decision 15 gates v0.1.0 on a restore drill: a full restore into a fresh namespace, with five
checks (sign in, a provider key decrypts, a schedule fires, a pending workflow recovers, an archived
agent restores). The hand drill of 2026-11-10 could not be run. It needed about 18 manual decisions;
the live data had no pending workflow, no archived agent and no working login; and nobody could repeat
it. A drill that cannot be repeated does not gate a release.

Research (`2026-11-10-automated-restore-drill.md`, kept with the maintainers' research notes) compared
five shapes: more bash in `backup-restore.sh`, a Ginkgo suite on seeded data, two clusters, a copy of
live data, and a browser-driven drill. It also looked at how others test restores: GitLab restores its
backups into a fresh instance in its own QA, CloudNativePG and Velero run restore end to end in CI
against a fresh namespace or cluster, and DBOS and Temporal test recovery by killing the process with
work in flight. All of them seed known data and assert on it; none gates a release on a copy of
production.

## Decision

1. **The drill is an automated test suite with seeded test data** (procoduck, 2026-11-10). It replaces
   the hand drill as the evidence for decision 15. A live-data copy may come later as a separate
   "shadow restore" smoke; it is never the gate, because it has no fixtures and shares live credentials.
2. **It runs at each release gate: "rc: kind; final: lab"** (procoduck, 2026-11-10). A release candidate
   tag needs a green drill on kind for its commit. A final tag needs a green drill on the home lab for
   the rc commit it promotes. The same suite runs in both places; a topology file says where.
3. **Suite and product fixes are built in parallel** (procoduck, 2026-11-10). A check the product cannot
   pass yet is not skipped and not left out: it fails with the message `known gap: #N`, where N is the
   issue that fixes it. A known-gap spec is declared in the suite (label `known-gap`) and listed in
   `drill-result.json`. When the fix lands, the gap marker is removed and the spec is required.
   Nothing in the drill skips: a check that cannot run (a missing tool, a missing cluster) fails the
   run through `testenv.Require`, which CI turns from a skip into a failure.
4. **Suite design.**
   - `test/drill` is a Ginkgo v2 suite. One `Ordered` container is one scenario; one `Label` is one
     check (`signin`, `provider-key`, `schedule`, `workflow-queue`, `hold`, `mark`, `isolation`, and in
     phase 2 `archive` and `workflow-dead-executor`). `backup-restore.sh` stays as the `inplace`
     scenario.
   - It seeds a source install through supported APIs only: the bootstrap password changed to a fixed
     fake one, an API provider with the fake key `koinon_test_not-a-real-key` on the fake model, an
     agent with a `* * * * *` schedule, low org trigger limits plus N "Run now" calls so N-1 runs stay
     ENQUEUED, and a marker writer for the RPO. No SQL writes.
   - It backs up with the guide's own command blocks (`test/e2e/commands/backup-restore.e1`, run through
     `hack/docs-e1.sh`), with the namespace and release substituted. This keeps the guide tested.
   - It restores into a fresh namespace with its own OpenShell gateway and `restoreHold: true`
     (ADR 0107), runs the held checks, releases, and runs the released checks.
   - Held checks: `koinon_restore_hold` is 1 on all three binaries; no schedule fires, no run is
     claimed, no DBOS workflow changes status, and no request reaches the fake model; `keys verify`
     passes; the target's install mark differs from the source's (ADR 0106).
   - Released checks: sign-in with the seeded password; a run reaches the fake model with the seeded
     key; one catch-up fire per due trigger, then a normal fire; every ENQUEUED run completes once; the
     source's sandboxes and providers are unchanged (isolation).
   - Known-gap specs in phase 1: the importer only reports while held, an MCP server can be disabled,
     and a source session token is refused on the target (#516).
5. **RTO and RPO are recorded, not targeted** (ADR 0085 decision 15 stays: no targets).
   - `t_disaster` is the moment the suite declares the source lost (it scales the source's components
     to zero). `RTO = t(last released check passes) - t_disaster`, reported with sub-times: target
     install, database restore, held checks, release, first run, queue drained.
   - `RPO = t_disaster - t(the latest marker present in the target)`. The marker writer posts a
     recorded setting change every 2 seconds on the source until `t_disaster`.
   - Both go into the Ginkgo report (`AddReportEntry`) and into `drill-result.json`: commit, chart
     version, topology, timings, one verdict per check, the known gaps. The suite fails on a check,
     never on a time.
6. **Teardown is by label.** Every object the drill creates carries `koinon.dev/drill-run=<id>`;
   namespaces start with `koinon-drill-`. `DeferCleanup` removes what a spec made. A sweep
   (`test/drill/sweep.sh`) deletes by label and refuses an object without the label or a namespace
   outside the prefix. It also removes the install-mark Secrets that survive `helm uninstall`
   (ADR 0106). CI deletes the kind cluster in an `always()` step.
7. **Report files.** The suite runs with `--junit-report` and `--json-report`; `test/e2e/run.sh` gets the
   same flags for the quickstart, and the workflows upload them.
8. **Gate wiring.** A `drill` job in `e2e-kind.yml` with `continue-on-error: true` (a red advisory drill must not turn
   that run red: `hack/e2e-bisect.sh` reads its conclusion; GitHub starts a dispatch or schedule only from a
   workflow file on the default branch, so a new workflow file could not be proven before merge), runs on the
   nightly schedule, on rc tags and on a dispatch with `suite=drill`. On a tag push and on that dispatch the
   existing `decide`, `quickstart` and `result` jobs skip, so only the drill runs. It is **advisory**: listed in `hack/ci-required.txt` as a job of another workflow, never a
   per-PR check and never in the merge queue (ADR 0105 keeps the quickstart as the one merge proof; the
   drill is too heavy for it). `hack/release-gate.sh` reads the drill result for a tag (a later phase,
   with the lab profile).

## Amends

ADR 0085 decision 15 (the drill's checks and the RTO and RPO record) is carried out by this suite; the
decision text stands. Decision 18 (cadence) changes in one way: the nightly kind run keeps the in-place
backup-and-restore step, and the fresh-namespace drill is a separate advisory job. Neither decision is
edited.

## Consequences

- A release candidate cannot be cut on a restore nobody tested. The drill is repeatable by one command
  on kind (`test/drill/run.sh`) and by one more on the lab with `DRILL_TOPOLOGY`. A topology's optional
  `imageRegistry` is passed to both installs as `image.registry` (run.sh sets it on kind to the cluster's
  path on the local registry, #45).
- The suite shows what is missing before the product fixes land. `known gap: #N` is a red line in the
  report on purpose: it is the list of work, and it ends when the issue closes.
- Each gateway has its own `gatewayId` (`drill-src-<run>`, `drill-dst-<run>`; the gateway chart value and
  Koinon's `openshell.gatewayId`). The restored org has the source's workspace name, so with one ID (the
  default `openshell`) both gateways would manage the same `openshell-<id>-<workspace>` namespace. The
  topology check refuses equal IDs; the sweep deletes the `openshell-drill-*-<run>-*` namespaces of the
  run by the `openshell.ai/gateway-id` label.
- A second OpenShell gateway and an object store run in the kind cluster, which loads a 4 vCPU runner.
  Phase 1 measures it; the answer decides whether the drill needs a larger runner.
- The guide's blocks take the namespace and the release as the variables `NS` and `REL` (default `koinon`,
  issue #517). The drill sets both to the install under test and runs the blocks as written. It refuses a
  block that still names the default install literally, so a new guide command cannot slip past it. (The
  first drill substituted the names in the text it ran; that interim measure is gone.)
- Phase 2 adds the archived agent and the dead-executor workflow, which need a fake sink with a release
  endpoint and an object store with checkpoints on; phase 3 (the lab profile and `release-gate.sh`)
  follows.

## Amendment 2026-11-11 (phase 2, #515)

- **Object store.** SeaweedFS (Apache-2.0), image `chrislusf/seaweedfs:4.50` pinned by digest in the topology file
  (`objectStoreImage`), in the fakes namespace with one fake identity and an `emptyDir`. It is the server the
  checkpoint writer's own test already uses (`internal/checkpoint/s3_test.go`), plain HTTP and path-style as
  `checkpoints.s3` requires. MinIO's community edition is AGPL-3.0 and is not used. Both installs share one
  bucket; the drill creates it over a port-forward with minio-go.
- **The archive check.** The source runs with `checkpoints.enabled`, `reconciler.idleTimeout: 2m` and
  `archiveAfter: 1m`. An agent with no trigger gets one task; the fake model's `drill-archive-write` script
  makes it write `/sandbox/work/drill-marker.txt` with its shell tool. The seed waits for `archived_at`. After
  the release the check runs the guide's `resync` block, expects the source's snapshot among the target's
  complete `home_checkpoints`, appends a `drill-archive-read` message to the same task, and expects the run
  to print `drill-marker-found` (read from `run_events`), the task's harness session ID to be unchanged and
  `archived_at` to be clear.
- **The dead-executor check holds the workflow with a row lock, not a sink.** Phase 1 planned a fake HTTP sink
  with a release endpoint. No Koinon notification kind posts to an HTTP endpoint (`wake.agent` and
  `question.asked` write to the database), so a sink would have nothing to hold. The drill instead locks the
  scheduled agent's `agent_sandboxes` row from a database session, creates a task, and the `wake.agent`
  notification's one step (an `UPDATE` of that row) waits: the workflow is PENDING under the source api's
  executor ID when the dump runs. The disaster kills the pod and the session. After the release the target's
  dead-executor job recovers it; the check expects status SUCCESS, another executor, and one `deliver:` step.
  A webhook notification kind would let a real sink replace the lock; that is a product decision.
- **RTO** gains the sub-time `dbosRecovery` (release to the recovered workflow).

## Amendment 2026-11-13 (phase 3, #518)

- **Two profiles.** A topology file names its `profile`, `kind` or `lab`, and `drill-result.json` carries it
  (`topology.profile`). The release gate tells the results apart by that field, not by a file name. The lab's own
  topology file (hosts, contexts, registries, storage classes) stays outside the repository; the repository holds the
  schema and a made-up example (`test/drill/topology/lab.example.yaml`) that a test keeps loadable. Paths in a topology
  may be absolute, so the lab's values file can live outside the tree.
- **The browser check.** The lab profile has `browserSignin: true`, which adds the check `browser-signin`: a
  Playwright spec (`web/e2e-drill`) signs in through the shipped web UI of the restored API. The spec is in the tree
  only when the topology asks for it, because no drill spec skips (decision 3). Kind does not run it (the
  alternative below stands: a browser for every check is slow; one sign-in on the lab is the limit).
- **The gate.** `hack/release-gate.sh` demands, for an rc tag, a `drill-logs` artifact for the commit whose result
  says that commit, profile `kind` and verdict `pass`; for a final tag, a `drill-result-lab.json` asset on the release
  of the rc it promotes (the highest `vX.Y.Z-rc.N` whose commit has the final commit's tree), with that rc's commit,
  profile `lab` and verdict `pass`. A check that is not `pass` or `known-gap` makes any result red. Known gaps do not
  block, and the gate prints them. Whether a final tag may carry known gaps is for the v0.1.0 checklist (#313) to decide.
- **Sign-in on the copy.** The drill seeds a fixed fake admin password, so the restored copy signs in with it. After a
  real disaster an operator has no such password: the break-glass request (#512) makes one, and the same spec takes it
  from `KOINON_DRILL_WEB_PASSWORD`.
- **Sandbox enforcement.** Every run on the restored copy reports sandbox enforcement `enforce`, read through
  `koinon-api run-evidence export` in the target's api pod (#955, ADR 0045). This is a released check.

## Amendment 2026-11-13 (a drill-owned OIDC provider, #983)

- **Why.** The `oidc-signin` check first signed in through an external identity provider (the lab's Pocket ID) with a
  virtual WebAuthn passkey. A lab run failed on the provider's first-time consent page, not on Koinon: the check
  depended on the provider's pages, its re-authentication and rate limits and its translated text, and it needed
  two private keys and a counter in a secret store. It also proved little of the restore. Koinon writes the
  IdP-sourced group memberships again at each login (`internal/auth/oidc.go`), so a role read after the target
  sign-in proves the mapping config, not the restored rows.
- **Decision.** The drill runs its own provider, Dex (`ghcr.io/dexidp/dex:v2.46.1`, Apache-2.0, pinned by its index
  digest in the topology field `fakeIdpImage`), in the fakes namespace, next to the fake model. Memory storage, the
  password database, `skipApprovalScreen`, PKCE enforced, one public static client `koinon-drill` with the two loopback
  redirect URLs of the drill, and three static users: `drill-admin` (group `koinon-admins`), `drill-member` and
  `drill-disabled` (group `koinon-users`). Their password is one obviously fake string; the bcrypt hash is made at run
  time (`test/drill/fakeidp.go`), so none is committed. The release builds nothing for it: kind pulls the upstream image
  by digest and a lab mirrors that digest. Every run starts with an empty provider, so nothing outlives a run.
- **Issuer.** `http://dex.<fakes namespace>.svc.cluster.local:5556/dex`, one string for koinon-api, the Go client and
  Chromium. koinon-api reaches it by cluster DNS. The drill host reaches it through a port-forward on a fixed local
  port: the Go client maps the name in its dialer, Chromium with `--host-resolver-rules` and a proxy bypass entry. The
  chart overlay always turns OIDC on for it, with the scopes `email`, `profile` and `groups` and the same group mappings.
  The topology's `oidc:` block for an external issuer, the persona passkey environment (`KOINON_DRILL_OIDC_*`), the CDP
  WebAuthn code and `oidc-signcounts.json` are gone.
- **The checks (every profile).** `oidc-source` (a step before the backup): a Go HTTP client with a cookie jar runs the
  authorization code flow through the install's real `/auth/oidc/start` and `/auth/oidc/callback` for the three users,
  records each user ID and each role from `WhoAmI`, then disables `drill-disabled` in Koinon (`users.deleted_at`; the
  product has no API for it) and checks that the source's callback now answers 403. `oidc-signin` on the restored copy:
  first, before any sign-in, the restored rows (the same user IDs, the group membership role in the default org, the
  disabled state) and the user count; then the Go flow for the admin and the member gives the same user ID and the same
  roles and makes no user, and the disabled user gets 403 at the callback. Kind runs all of it, so every rc has the proof.
- **The lab adds one browser sign-in.** The lab profile (`browserSignin: true`) also signs the admin in on the copy
  through the Dex login form with Playwright (`web/e2e-drill/oidc.spec.ts`), then checks the Inbox, a reload and
  `WhoAmI` (same user ID, same role). The step does not wait for a heading. After each action it waits for the first of
  "back at Koinon", a known Dex page (login form, wrong password, approval page, connector chooser, second factor, Dex
  error page) or a refusal of the callback, and fails with a named error (`web/src/lib/drill-dex.ts`; the Go flow uses
  the same rules in `test/drill/oidcflow.go`; both are tested against fixture pages in `test/drill/testdata/dex`).
- **Not covered.** The restore hold's 503 at the callback (the copy refuses OIDC logins until the release step) is not
  checked: the check runs after the release. Whether Koinon works with a given product (Pocket ID, Keycloak) is a
  compatibility question, not a restore one; it belongs in a separate smoke, not in this gate.

## Alternatives considered

- **Extend `backup-restore.sh`.** Cheapest now, but bash assertions give no report, test only an
  in-place restore, and a pending workflow and an archived agent are fragile to seed in bash. It is kept
  as the `inplace` scenario.
- **Two kind clusters.** Strongest isolation, but doubles the control planes on one runner, and a
  second namespace with its own gateway and its own gateway ID shows that nothing is shared. Revisit if a
  cluster-scoped object is added.
- **A copy of live data as the gate.** Real shapes, but no fixtures, not repeatable, and the copy holds
  valid live credentials.
- **A browser for every check.** Slow and brittle; most checks are not UI. One sign-in spec on the lab
  profile at most.
