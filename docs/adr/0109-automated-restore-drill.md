# 0109. Automated restore drill

- Status: Accepted
- Date: 2026-11-10
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
- The guide's blocks name the namespace `koinon` and the release `koinon`. The drill substitutes both
  in the text it runs. That is an interim measure; its exit is the parameterised restore guide, which
  makes the blocks take variables. The substitution refuses a block that still has a bare
  `koinon` after it ran, so a new guide command cannot slip past it.
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
