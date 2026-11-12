# 0060. Migrations run as a Job, not on start; an Argo CD Sync hook under Argo

- Status: Accepted
- Date: 2026-10-26
- Area: install-operations
- Tags: migrations, helm-chart, database, kubernetes
- Closes: an open question in [0037](0037-tooling.md) (run migrations at API start, or as a Job?)
- Amends: [0037](0037-tooling.md) (the decision "run on startup or by a migrate subcommand": migrations run as a Job)
- Relates to: [0003](0003-postgres-source-of-truth.md), [0027](0027-repo-and-versioning.md)

## Context

[0037](0037-tooling.md) left open whether goose migrations run when `koinon-api` starts or as a
Job. The chart already ran them as a Job (`koinon-api migrate`), but no ADR said why.

Under Argo CD that Job was wrong in cnpg mode (old issue 118).
In cnpg mode the release creates the database, so on install the migration cannot be a Helm hook
(a hook waits for a database that only appears once the hook has finished) and is an ordinary Job,
`<release>-migrate-install`; on upgrade it is a `pre-upgrade` hook. Argo CD renders with
`helm template`, where `.Release.IsInstall` is always true. So under Argo the migration was always the
ordinary Job, whose spec is immutable: an image bump failed to apply it, and nothing ran the
migration before the new pods started. Flux's helm-controller runs real Helm installs and upgrades
and was not affected.

## Decision

1. **Migrations run as a Job with the migration role, never on start.** [0003](0003-postgres-source-of-truth.md)
   splits the roles: `koinon_migrate` owns the schema and the components get DML only. Migrating on
   start would give `koinon-api` the schema owner's credentials (or a second credential in its pod),
   plus a lock across replicas. The Job keeps those credentials in one short-lived pod.
2. **Plain Helm and Flux** keep the Helm hooks: `pre-install,pre-upgrade` in external mode, and in
   cnpg mode the ordinary `migrate-install` Job on install and a `pre-upgrade` hook after.
3. **Argo CD** gets annotations on the same Job that Helm and Flux ignore:
   `argocd.argoproj.io/hook: Sync`, `argocd.argoproj.io/hook-delete-policy: BeforeHookCreation` and
   `argocd.argoproj.io/sync-wave: "1"`. Argo recreates the Job on every sync, so a new image gets a
   fresh Job, and the wave puts it after wave 0 (the CNPG Cluster, which Argo waits to be healthy,
   the ServiceAccounts, Secrets and other objects) and before wave 2: koinon-api, koinon-reconciler,
   koinon-agentd and the standalone gateway Deployment. The gateway is in wave 2 because its config
   ConfigMap is made by the reconciler, so left in wave 0 it would never become healthy and would
   stall the sync before the migration. goose `Up` is idempotent, so running on every sync is
   harmless. The Jobs that create the passwords (`ensure-secrets`) and the preflight stay Helm
   `pre-install,pre-upgrade` hooks, which Argo runs as PreSync.

The Argo CD documentation says that defining any Argo hook makes Argo ignore all Helm hooks. In
argo-cd 3.5.6 that is per resource: a resource with an `argocd.argoproj.io/hook` annotation uses it,
and one with only `helm.sh/hook` still maps to a phase. Verified on kind with Argo CD v3.5.6 (the
Application rendered the chart itself from a git repository): `ensure-secrets` and `preflight` ran
in PreSync, the migration Job in Sync wave 1 and finished before the Deployments were created, and
after a sync with a new image tag a new Job with that image ran again before the new pods. If a
later Argo release applies the rule app-wide, give those two Jobs `argocd.argoproj.io/hook: PreSync`
as well.

## Consequences

- On an existing Argo install, the first sync after this change replaces the ordinary
  `migrate-install` Job with the hook: `BeforeHookCreation` deletes the old Job by name and creates
  the new one.

- An Argo CD install migrates before the new pods on first install and on every upgrade.
- A sync that changes nothing reruns the Job, which does nothing.
- The Job is a Sync hook, so Argo shows it as a hook, not as a managed resource, and removes the old
  run when the next sync starts.
- A schema-version readiness gate in `koinon-api` (not Ready until the schema is current) would
  protect old pods during a RollingUpdate in every mode. It is not part of this decision.

## Note (2026-11-07, issue #328)

In cnpg mode the chart has a second migration hook (maintainer decision 2026-11-07): a `post-upgrade`
Job, `<release>-migrate-grants` (Argo CD: `PostSync`), that runs `koinon-api migrate --grants-only`
with the migration role, applies no migration and grants the install audit writer role once the
updated Cluster has created it. Why it runs after the release's objects, and what it waits for, is
[ADR 0100](0100-install-audit-chain.md) decision 1.1a. In external mode the `pre-upgrade` migrate Job
runs with `--require-install-writer` instead and fails when that role is missing.
