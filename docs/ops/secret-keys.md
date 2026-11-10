# Secret keys: rotation, backup and compromise

Koinon stores provider API keys (and a provider's optional cost key), connection secrets (a GitHub
App's private key), remote MCP server credentials, home repository passwords (the kopia repositories
that hold agent home checkpoints) and trace destination credentials in Postgres as envelopes: each
value has its own data key (DEK), wrapped by a key-encryption key (KEK) that lives in a Kubernetes
Secret. This runbook says how to look after the KEKs. The design is in
[ADR 0056](../adr/0056-model-providers-pricing-and-budgets-per-org.md); the sources for the numbers and
the procedure are in [the research](../research/kek-rotation.md).

## What is where

| What | Where | Who can read it |
| --- | --- | --- |
| KEKs | Secret `koinon-kek` (or `secrets.kek.existingSecret`): one file per KEK, named by its ID, plus `active` | `koinon-api` reads it through the Kubernetes API; `koinon-reconciler` mounts it. Nothing else does. |
| Wrapped DEKs and ciphertext | `providers.api_key` and `providers.cost_api_key`, `connections.secret_envelope`, `mcp_servers.credential`, `home_repositories.password_envelope` and `home_repository_epochs.password_envelope` (an agent's first home repository, and each one that replaced it; the current one is the newest epoch), `trace_destinations.credentials` and `platform_trace_destination.credentials`, `saved_work.envelope` (uncommitted work saved from a stuck sandbox, sealed by koinon-api, gone after 14 days) in Postgres | Anyone with the database, but they need the KEK to open them. |
| Saved-work upload key | Secret `<release>-saved-work`, key `key` (random, created by `koinon-api` on first use, never overwritten) | `koinon-api` reads it through the Kubernetes API; `koinon-agentd` mounts it read-only. It signs agentd's posts of a stuck sandbox's work; it is not a KEK and opens nothing. |
| Opened provider keys | Secret `koinon-gateway-providers` | Only the gateway pod mounts it. |
| Install ID | Table `install_identity` in Postgres (one UUID, written by the first migration, never changed) | Anyone with the database; it is not secret. A KEK wrapped by a KMS is bound to it (see "The install binding" below). |
| Bound-only marker | Table `kek_bound_only` in Postgres (one row once set, never changed or removed by Koinon) | Anyone with the database; it is not secret. Once set, every Koinon process refuses a KMS-wrapped KEK stored unbound (see "The bound-only marker" below). |

**The chart does not generate the keys.** A Helm render that made key material would make new keys
every time it ran (`helm template`, Argo CD and Flux do not have the live Secret to look at), and a new
KEK loses every stored secret. Instead `koinon-api` creates the Secret `koinon-kek` on its first start,
with one new KEK, and never overwrites it. If you set `secrets.kek.existingSecret`, `koinon-api` creates
nothing and fails to start when the Secret is missing. The reconciler waits for the Secret to appear
(not ready, with a warning in its log every minute).

**If the Secret is deleted later, `koinon-api` refuses to make a new one.** A new KEK could not open any
stored value, so on start it checks whether the database holds encrypted values; if it does, it stops with
"KEK Secret missing; restore it from your backup" and creates nothing. Restore the Secret from your
backup (`kubectl apply -f koinon-kek-backup.yaml`) and the pods start. Only a database with no
encrypted values yet gets a fresh Secret.

A KEK file holds the base64 of 32 random bytes (64 hex characters also work). KEK IDs made by Koinon
look like `kek-20261023-a1b2`; the date lets `keys status` tell the key's age.

## Look at the state

```sh
kubectl -n koinon exec deploy/koinon-api -- koinon-api keys status
```

It lists every KEK, which is active, how many values each wraps, and warns when the active KEK is
older than `secrets.kek.activePeriod` (default 365 days) or when a value is wrapped by a KEK the
Secret no longer has. `keys` reads the Secret through the Kubernetes API (`KOINON_KEK_SECRET`) and the
database (`DATABASE_URL`), both set in the `koinon-api` pod.

`keys status` counts values; it does not open them. To prove that the key set opens every stored
value, run `keys verify`. It decrypts each sealed value in memory, prints one line for each that does
not open (its table, org and row, and why: `unknown_kek`, `open`, `envelope`, or `database`), and
exits 1 if there is any. It is read-only: it takes no row lock, writes nothing and records no audit
event, and it never prints a value. It is the check after a restore, before the install is released
from restore-hold ([Back up and restore](../site/src/content/docs/guides/backup-restore.md)). Run it
also after you restore a key backup, to see that the backup holds every key the values need.

```sh
kubectl -n koinon exec deploy/koinon-api -- koinon-api keys verify
```

## Back up the KEK Secret

Do this once the install is up (the Secret exists after `koinon-api` has started) and after every
`keys add`:

```sh
kubectl -n koinon exec deploy/koinon-api -- koinon-api keys export > koinon-kek-backup.yaml
```

`keys export` prints the key set as a Secret manifest without a namespace, with a comment line per
KEK that gives its fingerprint (also in `keys status`), so you can tell which backup holds which key.
Restore it with `kubectl -n <namespace> apply -f koinon-kek-backup.yaml`.
The database backup that goes with it, and the full restore order, are in the
[Back up and restore](../site/src/content/docs/guides/backup-restore.md) guide.

**Check that the backup holds the active KEK.** The export is a snapshot: a `keys add` and
`keys activate` after it makes a backup that cannot open the values sealed since. After each export,
compare the fingerprints. `keys status` prints the active KEK's, and the export names it on a comment line:

```sh
kubectl -n koinon exec deploy/koinon-api -- koinon-api keys status
grep fingerprint koinon-kek-backup.yaml
```

The active KEK's ID and fingerprint must be in the file, and `active` in it must name that ID. The
export keeps every value base64-encoded under `data`, so decode `active` to read it:

```sh
grep '^  active:' koinon-kek-backup.yaml | awk '{print $2}' | base64 -d; echo
```

Koinon does not check this for you: there is no metric that compares the latest backup with the live key
set. Add the comparison to whatever job takes your backups.

**The export works the same in every database mode** (`standalone`, `cnpg`, `external`;
[ADR 0076](../adr/0076-database-modes.md)): `keys export` reads the KEK Secret through the Kubernetes API
and does not touch the database. The Secret is not part of any database backup, so back it up by
itself in every mode: Koinon's chart renders no job that copies it next to a database dump.

Keep the file with your other secrets, apart from the database backups. **A database backup needs the
KEKs that wrapped it.** Keep every KEK, including retired ones, for as long as a database backup that
used it exists. Retiring a KEK removes it from the Secret, not from your escrow. If every copy of a KEK
is lost, the values wrapped by it are lost, and that includes every home checkpoint whose repository
password it sealed and the trace destination credentials it sealed: see [A lost KEK](#a-lost-kek).

The Secret is not part of the Helm release, so `helm uninstall` leaves it, and so does a GitOps prune.
Delete it yourself when you delete the database.

## A lost KEK

`keys status` names a KEK that values use but the Secret does not hold as MISSING. First look for a
copy: your escrow, an older `keys export`, another cluster restored from the same backup. Restore it
with `kubectl apply` and nothing is lost. If the KEK was just deleted from the Secret by accident, the
running koinon-api pods may still hold it in memory for about a minute (they reload the Secret every
minute), but they cannot export it: only your escrow brings it back, so check it before you go on.
Only when no copy exists anywhere, give it up. **Know what it costs:** every credential the KEK
sealed (provider and cost keys, connection secrets, MCP server credentials, trace destination
credentials) must be entered again, and **every home checkpoint of an agent whose kopia repository
password the KEK sealed is gone**: that password exists only in `home_repositories.password_envelope`
(and `home_repository_epochs.password_envelope`), sealed by the KEK, so without the KEK the repository
cannot be opened by anyone. If the lost KEK was the active one and the Secret now has no usable
`active` entry, `keys forget` cannot run yet: see
[Recovering from a lost KMS key](#recovering-from-a-lost-kms-key) (or its local-install paragraph).

```sh
kubectl -n koinon exec -it deploy/koinon-api -- koinon-api keys forget <kek-id>
```

It refuses a KEK that is still in the Secret (that one is not lost: `keys rewrap`, then `keys retire`).
On a terminal it asks you to type the KEK ID, because it cannot be undone; without an ID it offers the
KEKs that seal values but are missing from the Secret. Without a terminal it does nothing unless you
pass `--yes`. After you answer it takes the key events' lock and reads the Secret again under it;
if the KEK was restored meanwhile it changes nothing, in no org. It holds that lock until it has
finished, so `keys add`, `keys activate`, `keys retire`, the key-admin Job and the start-up check of
a starting `koinon-api` wait for the whole run (one transaction per org that holds a value sealed with
the KEK). The keys `koinon-api` creates itself do not wait: after 10 s it creates them without the
lock and records them afterwards. It needs a database pool of at least 2 connections (one stays with
the lock, each org needs another) and refuses a smaller one, such as `pool_max_conns=1` in the
database URL, before it asks anything. It also switches off the server's
`idle_in_transaction_session_timeout` for the lock's transaction, so a short server timeout does not
end the run. Then:

- Provider keys and cost keys, connection secrets, MCP server and trace destination credentials sealed
  with it are marked lost. Their pages say "credential lost" and ask for the value again (an MCP
  server keeps its header and prefix); nothing uses them until then. A declared one is stored again
  from its Secret.
- Saved work sealed with it expires, as it does after its retention.
- An agent whose current home repository password was sealed with it starts a new home repository,
  sealed with the active KEK, and its next sandbox starts with a fresh home: its earlier checkpoints
  cannot be restored. The old repository is deleted after the org's home backup retention
  ([retention guide](../site/src/content/docs/guides/retention.md)).
- Each org it changed gets one `keys.forget` audit event (actor `cli`, `keys forget`) listing what was
  lost and the new repositories. The platform's own trace destination belongs to no org: it is one
  event in the install audit chain (actor `cli`, `keys forget`), written in the same transaction, and
  a platform admin enters the destination again. It needs no platform admin to exist.
  If the key set changed without a recorded event (for example a KEK removed from the Secret by hand),
  `keys forget` first records that as `keys.observed` with itself as the actor, then writes its own
  `keys.forget` event.

Running it again finds nothing left to forget. Then check that `keys status` no longer shows the KEK.

## Rotate on schedule

Do it once a year, or when `keys status` warns. No downtime, no restart.

1. Add a key: `koinon-api keys add`. It prints the new ID and does not activate it. It reads the database (`--database-url`) to refuse an ID that stored values still carry. Back up the Secret.
2. Activate it: `koinon-api keys activate <new-id>`. New values are wrapped by it once the pods have the
   changed Secret (kubelet takes up to about a minute; both pods reload their keys every minute).
3. Wait two minutes, then re-wrap the old values: `koinon-api keys rewrap`. This wraps every stored
   DEK under the active KEK. It never decrypts a payload and can be run again.
4. Check `keys status`: no value should use the old KEK. If one does (a pod wrote a value with the old KEK
   before it saw the change), run `keys rewrap` again.
5. Retire the old key: `koinon-api keys retire <old-id>`. It refuses while any value uses the KEK, and
   until the active KEK has been active for five minutes (so every pod wraps with it). Keep a copy of the
   old key in your backup escrow.

DEKs need no schedule: every write makes a new one.

### Interactive form

In a terminal (`kubectl exec -it`), `keys activate`, `keys retire`, `keys rewrap` and `keys reencrypt`
can ask instead of taking everything on the command line. The flag forms above work the same; the
prompts only fill in what you left out and confirm what is hard to undo.

```sh
kubectl -n koinon exec -it deploy/koinon-api -- koinon-api keys activate
kubectl -n koinon exec -it deploy/koinon-api -- koinon-api keys rewrap
kubectl -n koinon exec -it deploy/koinon-api -- koinon-api keys retire
```

- `keys activate` without an ID lists the inactive KEKs (age, and the values each wraps) and asks
  "Activate `<id>`?" before it changes the Secret.
- `keys retire` without an ID lists the KEKs; the ones that cannot be retired now (the active one, one that
  values still use, or any while the active KEK is under five minutes old) are shown with the reason and
  cannot be picked. It asks again, with the escrow reminder, before it removes the key.
- `keys rewrap` asks with the number of values it will change.
- `keys reencrypt` asks you to type the active KEK's ID, because it decrypts every payload.
- `keys forget` asks you to type the lost KEK's ID, because it cannot be undone.
- `keys new-repositories` asks you to type `replace`, because the agents' older checkpoints are deleted.
- `--yes` skips the questions you would be asked (it never picks a KEK for you).

Without a terminal (no `-t`, a script, CI, a pod's own command), or with `--no-input`
(`KOINON_NO_INPUT=true`), nothing is ever asked: a missing ID is the usual error
(`accepts 1 arg(s), received 0`) and nothing needs `--yes`, except `keys forget` and
`keys new-repositories`: they cannot be undone, so without a terminal they refuse unless you pass
`--yes`. `--no-input` is a flag of every Koinon
binary; `kubectl exec -it ... keys retire --no-input <id>` runs the plain form on a terminal. The chart
does not set it. With `TERM=dumb` the questions are plain lines, one per answer.

## Rotate the OTLP receiver token

koinon-api's internal OTLP listener (the gateway's access log, agentd's run spans) requires a bearer
token: the Secret `koinon-otlp-token`, key `token` (or the Secret named by `api.otlpToken.existingSecret`).
The ensure-secrets Job creates it and never changes it. Rotation has no downtime, because the listener
also accepts the key `previous`:

1. Put the current `token` value in a new key `previous` and a new value in `token` (`openssl rand -hex 32`;
   at least 32 characters of A-Z a-z 0-9 . _ ~ + / = -). Keep the values off the command line, where they
   would land in shell history and the process list: write a patch file only you can read, apply it, and
   delete it.

   ```sh
   umask 077; p=$(mktemp)
   old=$(kubectl -n koinon get secret koinon-otlp-token -o jsonpath='{.data.token}')   # already base64
   printf '{"data":{"previous":"%s","token":"%s"}}' "$old" "$(openssl rand -hex 32 | base64 | tr -d '\n')" >"$p"
   kubectl -n koinon patch secret koinon-otlp-token --type merge --patch-file "$p"; rm -f "$p"
   ```

   (`kubectl -n koinon edit secret koinon-otlp-token` works too.)
2. Wait for kubelet to sync the mounted Secret (up to about a minute). Nothing is restarted: koinon-api and
   koinon-agentd re-read their files every few seconds, and agentgateway watches the token file and
   reloads it, so its next export carries the new token. In kubernetes mode the controller reads the Secret
   and pushes the value to the proxy.
3. Once every sender sends the new token, check `koinon_api_otlp_auth_rejected_total` stays flat, then
   remove `previous`. A `previous` that is empty or not a valid token is ignored, and never blocks a new
   `token`.

If the token leaked, do the same and remove `previous` straight away: the old token is refused at once.
Exports in the window between that and the gateway's reload (kubelet sync plus the reload) are rejected
with a 401, and agentgateway does not retry them, so the spend ledger misses those records.

## Suspected compromise

A KEK compromise means someone may hold a KEK and the database. Rotating the KEK is not enough: they
can already open every DEK they copied. Do this, in this order:

1. **Stop wrapping with the key.** `keys add`, then `keys activate <new-id>`.
2. **Re-encrypt with new DEKs.** `koinon-api keys reencrypt`. It decrypts each value in memory and
   seals it again under a fresh DEK and the active KEK. Then `keys retire <compromised-id>`. Do not
   escrow the compromised key with your normal backups; keep it only for backups you must still read,
   in a place the attacker cannot reach.

   **2b. Replace the agents' home repositories.** The KEK opens each agent's kopia repository
   password, and kopia derives the repository's master key from it once: re-sealing the password
   (step 2) or changing it in kopia does not revoke what was copied, and with the checkpoint bucket's
   S3 key whoever holds it reads every saved home. `keys reencrypt` keeps every
   repository password the same; Koinon has no command that changes a repository's password in place.
   The only way to change it is `keys new-repositories`: a new repository under a new random password
   (step 2), with the newest checkpoint copied over. So each agent gets a new repository, and the
   bucket a new key, in this order:

   1. Issue a new access key for the checkpoint bucket at your S3 provider. Put it in the Secret
      named by `checkpoints.s3.existingSecret` (keys `AWS_ACCESS_KEY_ID` and
      `AWS_SECRET_ACCESS_KEY`), then restart koinon-agentd, which reads it at start and is the only
      component that holds it:

      ```sh
      kubectl -n koinon rollout restart deploy/koinon-agentd
      kubectl -n koinon rollout status deploy/koinon-agentd
      kubectl -n koinon logs -l app.kubernetes.io/component=agentd --prefix | grep 'checkpoint storage'
      ```

      Each replica logs `access_key=sha256:…`, the fingerprint of the key's ID (never the key):
      check that every replica shows the new one. Koinon does not issue or rotate S3 keys itself.
   2. Start a new home repository for every agent:

      ```sh
      kubectl -n koinon exec -it deploy/koinon-api -- koinon-api keys new-repositories --all
      ```

      (`--agent <id>`, repeated or comma-separated, for only some agents.) Each agent gets a new
      repository epoch with a random password that nobody has seen, sealed with the active KEK; the
      old repository is retired with a deadline of now, and so is every older retired repository of
      the agent still kept for its retention (as `keys forget` leaves one): it may be under an
      exposed password too. Each agent gets one
      `home.repository_replaced` audit event (actor `cli`). An agent without a repository is skipped:
      its first one is made under the active KEK.
   3. koinon-agentd copies each agent's newest checkpoint into its new repository (a restore piped
      into a backup, falling back to older checkpoints when one does not copy) and records the copy
      with the source's times (audit `home.repository_seeded`, actor `system`). The command does
      not wait for it: `koinon_agentd_home_repository_seeds{state="pending"}` falls to 0 when every
      seed is done. The sweeper then deletes each old repository, once the new one holds a
      checkpoint at least as new as the old one's newest and no checkpoint of the agent is running
      (one whose writer died counts only within its lease). Only the copied checkpoint survives:
      the older history is dropped with the old repositories. Seeds use at most all but one of a
      replica's backup writers, so stop and hourly checkpoints go on meanwhile.
   4. A seed is tried three times, with a growing delay, like a checkpoint. One that keeps failing is given up
      (audit `home.repository_seed_failed`, `koinon_agentd_home_repository_seeds{state="failed"}`
      counts it) and its old repository is kept, since nothing newer holds that home; the agent's
      next backup goes to the new repository and releases the old one, and the seed then no longer
      counts as failed. Fix the cause (the log and
      the audit event give the error), then run `keys new-repositories --agent <id>` again for a new
      seed.
   5. Revoke the old S3 key at the provider once every replica logs the new fingerprint and a
      checkpoint has completed with it.

   The agents' sandboxes, homes and generations are not touched; their next backups go to the new
   repositories. Without a terminal `keys new-repositories` refuses unless you pass `--yes`, as
   `keys forget` does, because the older checkpoints are deleted. Running it again replaces the
   repositories again (a seed not yet done moves on to the newest epoch). These choices are recorded
   in [ADR 0085](../adr/0085-upgrade-compatibility-and-recovery-policy.md) decision 14 and #86.

   **After a database restore, run `koinon-api checkpoints resync --all` before `keys forget` or
   `keys new-repositories`.** A repository started after the dump is in the bucket but not in the
   restored database, and its location follows from the agent and the repository number, so the
   restored database's next number can point at it. The resync lists the agent's repository folders
   in the bucket and records each location it finds taken by a repository the database has no
   password for, and the next free number past every folder that holds anything (its summary counts
   the taken locations; audit
   `home.repository_prefix_taken`, actor `system`, with `prefix`, `repositoryEpoch`,
   `knownToDatabase` and `nextEpochAtLeast`). Both commands then start the agent's next repository
   at that number. A backup or seed that reaches a taken location fails at once with an error that
   names it and says to run `keys new-repositories --agent <id>`; it records the same, so that run
   picks the free number. The sweeper never deletes the blobs at a taken location: they belong to
   the lost database's repository, and you delete them by hand once nothing in them is needed (#262).
   A repository whose complete checkpoints the database records but that does not open with their
   password is not taken: the backup fails with "does not open with the password that wrote its …
   checkpoint(s)", and nothing is recorded. koinon-agentd uses the wrong bucket, or the repository's
   `kopia.repository` is damaged; fix that, and do not run `keys new-repositories` for the agent,
   which would retire the repository and let the sweeper delete its homes.

   If only the **bucket's S3 key** leaked, not a KEK, the repository passwords are safe: do step 2b.1
   and 2b.5 only.
3. **Rotate the real credentials at their providers.** Re-encrypting does not undo a leak. Issue new
   OpenRouter, OpenAI or other provider keys (and cost keys), a new GitHub App private key, new remote
   MCP server credentials and new trace destination credentials (the trace backend's API keys, or a
   generic destination's header values), and replace them in Koinon (the admin API, or by editing the
   declarative Secrets). Revoke the old ones where they were issued.
4. Rotate the admin API tokens: revoke every token and create new ones.
5. Look at the audit log for changes made while the key was exposed.

If a **database backup** leaked but not the KEKs, nothing opens, but plan a normal rotation anyway.

## Declarative Secrets

Providers and connections declared by labelled Secrets (see the Models guide) carry their credentials
in the Secret's `stringData`, as in Argo CD's declarative setup. Koinon copies them into the store and
leaves the Secret as it is. Kubernetes stores Secrets unencrypted in etcd unless you turn on
[encryption at rest](https://kubernetes.io/docs/tasks/administer-cluster/encrypt-data/), so do that, and
do not keep the manifests in git in clear text: use External Secrets Operator or Sealed Secrets to
produce the labelled Secret from a store you trust. Treat write and read access to Koinon's namespace as
platform-admin level.

When such a Secret is deleted, Koinon does not delete what it declared. A provider (and its models) or
connection is marked removed: it is no longer served or usable, agents bound to a removed connection
report why, runs that need it fail with that reason, and creating the Secret again restores it. An org
admin can delete a removed entry for good.

## Append-only audit log with an external database

`audit_events` refuses UPDATE and DELETE for every role through a trigger. The migration also revokes
UPDATE, DELETE and TRUNCATE on it from the role `koinon_app`, which the chart's CloudNativePG setup
creates. With `postgres.mode: external` the roles are yours: the migration cannot know their names, so
run this once as the schema owner, for the role `koinon-api`, `koinon-reconciler` and `koinon-agentd`
log in as (or the group role they share):

```sql
REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM <app role>;
```

and make sure that role is not the table owner.

### The install audit chain (ADR 0100)

Actions that belong to no org (platform prices first) go to a second chain, `install_audit_events`.
Only `koinon-api` writes it. The migration takes every write privilege on both install tables away
from `koinon_app`; `koinon-api migrate` then grants INSERT to the group role
`koinon_install_audit_writer` after the migrations, on every run, if the role exists. It never creates
the role (the migrate role has no CREATEROLE): when it is missing the Job logs a warning and grants
nothing, or, with `--require-install-writer` (the chart's external mode, below), fails. The chart's CloudNativePG setup creates the role in `postInitApplicationSQL` (new clusters)
and as a managed role (clusters created before this release), and makes only the `koinon_api`
managed role a member. On an upgrade of an older cluster the operator creates that managed role
after the `pre-upgrade` migrate Job, so the chart also runs `koinon-api migrate --grants-only` as a
`post-upgrade` hook (`<release>-migrate-grants`; Argo CD: `PostSync`): it applies no migration,
waits up to `migrations.wait` for the role and the tables, grants, and fails the upgrade if the role
never appears. One `helm upgrade` is enough. Until the grant lands `koinon-api` serves anyway: it
logs an error for each startup install event the database refuses
(`koinon_audit_writes_total{chain="install",result="error"}` counts them) and retries it every
30 seconds, and platform price changes fail with a permission error. The scheduled price refresh
keeps the setting already recorded (mode, interval and auto-apply bounds) wherever it does less,
until its change is recorded. Do not let the api, reconciler and
agentd share one login: the reconciler would then be able to write this chain. `koinon-reconciler`
and `koinon-agentd` log a warning at startup when their login can write it.

`koinon-api` checks its own login against the role, at startup and every 30 seconds:
`SELECT current_user, pg_has_role(current_user, 'koinon_install_audit_writer', 'USAGE')`. Only it
knows the login it connects as, so the check runs there and not in the migrate Job or from a chart
value (ADR 0100 decision 1.1a). When the role is missing, or the login is not a member or does not
inherit it, it logs one error per change of state with the statement to run for that login, for
example `GRANT koinon_install_audit_writer TO "koinon_api"`, and keeps serving. The gauge
`koinon_install_audit_writer_ok` (no labels) is 1 while the login can write the chain and 0
otherwise, so the gap shows before anything tries to write.

koinon-api anchors every org chain in the install chain hourly (`--audit-anchor-interval`, ADR 0100
decision 4.1; `koinon-api audit anchor` does it now). The gauge `koinon_audit_anchor_age_seconds` (no
labels) is the age of the newest anchor; alert when it stays above a few intervals, for example
`max(koinon_audit_anchor_age_seconds) > 3 * 3600` for 15 minutes with the default interval. While there is no anchor yet the gauge counts the seconds since the process started.

The interval is at least 5 minutes: every anchor stores one row per org (`install_audit_anchor_heads`),
so 100 orgs hourly is about 880 thousand rows a year. The age gauge keeps its last value when the read of
the newest anchor fails, so with the database down it stops moving and can look healthy or stale
depending on when the outage started: alert on the database being unreachable next to it, for example
on `up` or your database exporter, instead of relying on the gauge alone.

Before each anchor koinon-api checks every org's chain against the previous anchor (the rule of C2SP
tlog-checkpoint: never sign a checkpoint inconsistent with an earlier one). A mismatch is logged once per
tick, naming the org, and counted in `koinon_audit_anchor_inconsistent_total{reason}` (`hash_changed`,
`seq_regressed`; no org label). The anchor is written anyway. Alert on any increase, for example
`increase(koinon_audit_anchor_inconsistent_total[1h]) > 0`, then run `koinon-api audit verify`.

A missing grant or membership that lasts longer than an upgrade, or a refused install write, needs
an operator. An alert for both:

```yaml
- alert: KoinonInstallAuditWritesFailing
  expr: >-
    min(koinon_install_audit_writer_ok) == 0
    or sum(increase(koinon_audit_writes_total{chain="install",result="error"}[15m])) > 0
  for: 30m
  annotations:
    summary: koinon-api cannot write the install audit chain
    description: >-
      koinon-api's log names the GRANT to run for its login. Check that koinon_install_audit_writer
      exists and that koinon-api's login role is a member, and run koinon-api migrate --grants-only
      (docs/ops/secret-keys.md, The install audit chain).
```

With `postgres.mode: external` the roles are yours. Run this once as an administrator **before the
first migration** (or before the upgrade that adds the chain; created later, run `koinon-api migrate`
or `koinon-api migrate --grants-only` again), with `<api role>` the role `koinon-api` logs in as:

```sql
CREATE ROLE koinon_install_audit_writer NOLOGIN;
GRANT koinon_install_audit_writer TO <api role>;
```

In this mode the chart runs the migrate Job with `--require-install-writer`: without the role the Job
fails (`the role koinon_install_audit_writer does not exist`) and so does the install or upgrade, since
no hook would create the role later. The check runs before any migration, so a failed Job leaves the
schema as it was. Create the role as above and run the same `helm upgrade` again.

The Job does not check the membership: it connects as the migration role and cannot know the login
`koinon-api` uses (with a cloud group login or a pooler that forces the user it is not the name in the
api URI, and under workload identity the Job is another identity). `koinon-api` checks its own (above).

And, if the migration role is not the one that owns the tables or `koinon_app` does not exist, after
it:

```sql
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON install_audit_events, install_audit_chain_head, install_audit_anchor_heads FROM <reconciler role>, <agentd role>;
GRANT SELECT ON install_audit_events, install_audit_chain_head, install_audit_anchor_heads TO <reconciler role>, <agentd role>;
```

**Do not let `koinon-reconciler` or `koinon-agentd` share the `koinon-api` login role.** The
privilege is the role's: a process that connects as the writer role can append install events
under any actor name (the chain's hash stops edits, not a forged first write), and the only
separation between the components is that their roles differ. The same holds for the table owner
and any superuser, which can also rewrite the chain; signed checkpoints (ADR 0100) will close that
gap later. `koinon-api audit verify` walks the install chain and every org chain and exits non-zero
at the first broken link (`--install` for the install chain alone).

### Key events in the install audit chain

Every change to the key set is written to the install audit chain (ADR 0100, "Keys: not atomic,
so checked on start"). A Secret update is not a database write, so the event follows the update; if
it cannot be written, the change stands (`koinon-api keys` makes the change and returns an error that
it was not recorded) and the next `koinon-api` start records the key set as `keys.observed`.

A key command (`add`, `activate`, `retire`, and every change the key-admin Job makes) takes the key-set
lock in the database before it reads the key set, and holds it through the Secret update and its
event. A `koinon-api` start or keyring check in that window waits for the lock and then sees the
change recorded, so a key change shows once, never also as `keys.observed`. The keys `koinon-api`
generates itself (the KEK when `kekCreate` is on, and `koinon-saved-work`) are made the same way: it
takes the lock before it creates the Secret and writes `keys.generated` before it releases it. If the
lock is not free within 10 s, it creates the Secret anyway and writes the event afterwards. `activate` and `retire`
check the chosen KEK again on the key set read under the lock: on a terminal you pick and confirm
first, and a KEK that changed meanwhile is refused then.

| Action | Actor | When |
| --- | --- | --- |
| `keys.add`, `keys.activate`, `keys.retire` | `cli`, `keys add` / `keys activate` / `keys retire` | right after the Secret update |
| `keys.rewrap`, `keys.reencrypt` | `cli`, `keys rewrap` / `keys reencrypt` | once per run, after the loop: rows done and failed, failures by kind (`unknown_kek`, `open`, `envelope`, `wrap`, `database`), the KEK usage before and after. A run that changes no row and fails none writes nothing; a run that wrote a `.started`, or failed a row, always writes it |
| `keys.rewrap.started`, `keys.reencrypt.started` | as above | only with `--record-started`, before the loop, when there is a row to change |
| `keys.generated` | `system koinon-api:keys` | `koinon-api` made the first KEK Secret or the saved-work key |
| `keys.secret_created`, `keys.add`, `keys.activate`, `keys.bound` | `system koinon-api:key-admin` | the key-admin Job created the KEK Secret, added a KEK, recovered a lost active KEK, or bound a KEK to the install |
| `keys.observed` | `system koinon-api:startup`, `system koinon-api:keyring-watch`, or the actor of the key command that found the gap | a start found the key set different from the last recorded key event (the first start always writes it, even with no KEK and no generated Secret: maintainer decision 2026-11-08); the running `koinon-api` saw the key set change and found it different; or a key command (`add`, `activate`, `retire`, the key-admin Job) found the live KEKs different from it and recorded that before its own event, under its own actor |

The failure kinds of `keys.rewrap` and `keys.reencrypt` are a fixed set: `unknown_kek` (the envelope
names a KEK the key set does not hold), `open` (the envelope did not authenticate: wrong key or
modified data), `envelope` (the stored value is not a readable envelope), `wrap` (the new envelope
could not be made: the data key could not be wrapped, for example the KMS did not answer, or the value
could not be sealed) and `database` (any other error, the row's transaction or the row listing). A
format or algorithm the keyring does not know counts as `envelope`.

If the rows of a column cannot be listed, that counts as one `database` failure, however many rows the
column has, and the run stops there: the columns after it are not tried. The closing event records the
rows done so far and the failure, and the command fails; run it again once the database is back.

`--record-started` writes the `.started` event before the first row changes. If the chain cannot take
it, the run is refused and says "nothing was changed": a run that could die half way is never started
without its marker (maintainer decision 2026-11-08).

The key-admin Job waits for the chain at most `--wait` (5 minutes by default) in total, one budget per
Job run, not per event. If it cannot read or write the chain, it prints a warning and succeeds; the
Secret change stands and the next start records `keys.observed`. The CLI instead returns the
not-recorded error. Both are maintainer decisions of 2026-11-08.

`koinon-api` does not know its Helm release name, so it lists the generated Secrets by the
`koinon.dev/generated` label alone. Two releases in one namespace would see each other's generated
Secrets in `keys.observed`, so one Koinon release per namespace stays the rule until release identity
lands (#375, v0.2; maintainer decision
2026-11-08).

The summary of a key event holds `before` and `after`, each `{keks: [{id, fingerprint, wrapped_by,
binding}], active, retired, generated: [{secret_name, uid, created_at}]}`. It names KEKs by ID and
fingerprint, as `keys status` does, and Secrets by `secret_name`, UID and creation time. It never
holds a key, a value or a hash of one: Secrets are read by metadata only. A generated Secret that was
deleted and created again has a new UID, so the start after it writes `keys.observed`.

`keys.observed` also appears on the first start after a KEK came from `secrets.kek.existingSecret`
or from the ensure-secrets Job, since no command wrote it; those keys are never recorded as
`keys.generated`, which is only for a key `koinon-api` made itself (maintainer decision 2026-11-08).
The lost-change `keys.observed` a key command writes carries that command's actor, not
`koinon-api:startup` (maintainer decision 2026-11-08). `keys activate` now needs `--database-url`
(the `DATABASE_URL` alias still works), like `add`, `retire` and `rewrap`, so that it can record its
event (maintainer decision 2026-11-08).

### Platform settings in the install audit chain

On every start `koinon-api` records the install-wide settings that Helm sets through its flags as
`platform.settings.observed` (actor `system koinon-api:startup`). In one transaction under an advisory
lock it compares them with the settings recorded so far, and writes one event with only the keys that
changed in `before` and `after`. The first start writes every setting; a restart with the same settings
writes nothing; two replicas starting together write one event. Like the other start-up writes it is
retried in the background every 30 seconds while the database refuses the install audit writer role.

The list is the Go table `recordedSettings` in `internal/cmd/koinonapi/settings.go`: the auth and
session flags, the network and gateway flags, the key and wrapping flags (`kek-*`, `openbao-*`,
`awskms-*`, the saved-work flags), `otlp-addr`, `otlp-token-dir`, `durable`, `run-cap-sweep-interval`,
`chart-version` (the chart passes `.Chart.Version`) and `image-version`. A flag that names a Secret, a
file or a directory is recorded by name only: `koinon-api` never opens it, reads its content or hashes
it. A URL is recorded as `scheme://host[:port]` only: user info, path, query and fragment are dropped,
because any of them could carry a credential. The one exception is `oidc-issuer-url`, whose path
identifies the issuer and is kept; it too loses user info, query and fragment. Five keys differ from their flag, because the audit
scrubber refuses a key that looks like a secret value: `bootstrap-password-file` is
`bootstrap-admin-pw-file`, `kek-secret` is `kek-object`, `saved-work-secret` is `saved-work-object`,
`otlp-token-dir` is `otlp-auth-dir` and `openbao-token-file` is `openbao-auth-file`. The price refresh
flags are not on the list: they are `price.refresh.settings_changed` (ADR 0098). Settings an admin
changes through the API are audited by that API call.

During a rolling upgrade, each pod start that sees a different image version, chart version or
settings table than the recorded ones writes an event. Old and new pods can therefore alternate
events until the rollout ends; this is expected, and the last event after the rollout holds the new
state.

## Admin API tokens

A token made from a session starts a chain: 30 days by default, at most 90. A token may create tokens
only with the same or a lower role and an expiry no later than its own, so a stolen token cannot extend
itself. Every token carries the root user of its chain. Authentication checks that this user still holds
at least the token's role in the org (or is a platform admin); when they are demoted or removed, the
whole chain stops working. Revoking a token revokes every token below it. A token can revoke tokens
of its own role or lower.

Failed use is not written to an org's audit trail (anyone can send a bad token). It goes to the
`koinon-api` log as a structured line with the client IP and the token's public prefix, never its secret,
and the client IP is slowed down after five failures, with growing backoff up to 15 minutes. When the
prefix belongs to a real token, "failed use of token" is also written to that org's audit log. The client
IP is the connection's peer; behind an ingress that is the ingress.

## Provider URLs

Providers may not name loopback, link-local, private (RFC 1918, fc00::/7) or shared (100.64/10)
addresses, the cloud metadata addresses, names without a dot, or hosts in Koinon's own namespace,
because the gateway would send requests there with the provider's key. Private addresses include the
ClusterIPs of Services, so even `koinon-api` is out of reach.

Hosts are read the way agentgateway's URL parser reads them: `0177.0.0.1`, `127.1`, `0x7f.0.0.1` and
`2130706433` are all 127.0.0.1 and are refused, and so is a host that ends in a number but is no valid
address. Names are resolved when a provider is saved (a name that does not resolve yet is accepted) and
again each time the reconciler renders it (a name that cannot be resolved, or resolves to a denied
address, is not served; the render check fails closed). DNS rebinding between the checks is not closed,
because the gateway resolves the name itself; the egress NetworkPolicy below is the hard limit.

A platform admin allows internal upstreams in the chart value `egress.allowedInternalHosts` (the old name `providers.allowedInternalHosts` still works for one more release and is joined with it; the list covers model providers and remote MCP servers): names
(optionally with a port), IPs or CIDRs, for example `["ollama.lab.example.com:11434", "10.7.0.0/24"]`.

**Gateway egress policy.** With `networkPolicy.enabled` the chart restricts agentgateway's egress with a
standard `networking.k8s.io/v1` NetworkPolicy (no CNI-specific resources): DNS, `koinon-api`'s OTLP and
tools ports, and the public internet with the private, shared, link-local and loopback ranges carved
out. Allow an internal upstream there too, by address, with `networkPolicy.gatewayEgressTo`:

```yaml
networkPolicy:
  gatewayEgressTo:
    - to: [{ipBlock: {cidr: 10.7.0.0/24}}]
      ports: [{port: 11434}]
```

An MCP target other than `koinon-api`'s tools listener (a future remote MCP server, or any other
upstream the gateway is configured with) is blocked the same way when it is on a private address: add
it to `networkPolicy.gatewayEgressTo` too. The IPv6 carve-outs include NAT64 (`64:ff9b::/96`) and 6to4
(`2002::/16`), so an IPv6-only cluster that reaches the internet through NAT64 must add its own rule.

Limits: the carve-outs are the standard private ranges, so a cluster that numbers pods or Services from
public addresses needs its own rules; the policy matches addresses, not names, so keep it and
`egress.allowedInternalHosts` in agreement; and a CNI that does not enforce egress policy ignores
it (the default provider URL check still applies).

## Wrapping the KEKs with a KMS (optional)

A KMS is an option, not a mandate. The default keeps the KEKs in the Kubernetes Secret, as described
above; it is fully supported and nothing here asks you to move. An install can opt in to have a KMS
(OpenBao/Vault transit, old issue 416, or AWS KMS, old issue 417) wrap each KEK, so the Secret holds only ciphertext
that is useless without the KMS. Both are set with `secrets.kek.kms` (below).

What changes once you opt in (ADR 0085 decision 12):

- The KMS wraps the KEKs, not every value. Pods unwrap the key set when they load it (on start and
  every minute), so values are read and written locally and a short KMS outage does not stop reads.
- Every pod signs in to the KMS with its own pod identity (Kubernetes auth for OpenBao, IRSA for
  AWS). No KMS credential is in Helm values or env.
- Point the OpenBao address at the active node (or a load balancer that forwards to it). Koinon
  never follows a redirect, since that would send its token to wherever the redirect points; a
  standby that redirects is reported as "redirect refused: set the active node's address".
- koinon-api and the reconciler may only decrypt (agentd holds no keyring). A separate key-admin Job, with its own
  service account and the only encrypt right, wraps new KEKs: it runs on first install and on demand
  when you add or rotate a KEK. `koinon-api keys add` (and koinon-api's first start without a key)
  then fail with "new KEKs are wrapped by the key-admin Job" instead of calling the KMS. `keys
  activate`, `rewrap`, `retire`, `status` and `export` still work from koinon-api.
- Moving to the KMS uses the rotation steps above: the key-admin Job adds a KMS-wrapped KEK, then
  `keys activate`, `keys rewrap` and `keys retire` on the local ones. `keys status` shows WRAPPED BY
  for each KEK. A KEK's fingerprint does not depend on its wrapping.
- koinon-api records each KEK's fingerprint in the database (`kek_fingerprints`) when it loads the key
  set and `keys activate` records it too. `keys status` warns when a loaded KEK's fingerprint differs
  from the recorded one for its ID, which means a restored or wrong KEK Secret. Rows of retired KEKs
  stay: IDs are never reused, and the history is what a later `keys forget` needs.
- `keys status` also says whether the active KEK's wrapping key answers, and warns when the active KEK
  is a local one on an install that has a KMS configured. `keys activate` refuses a local KEK while a
  KMS is the configured primary (procoduck, 2026-11-03); to roll back, set the configured wrapping to
  local first, then activate. koinon-api and the reconciler export
  `koinon_kek_active_reachable{backend}` (1 or 0), probed on every reload. A local install always
  reads 1 and logs nothing. If the KMS is down when a pod starts, the pod does not start (it cannot
  unwrap the KEKs: fail closed), so the gauge never shows 0 then; alert on the pod not being ready.

**Losing the KMS key loses the KEKs it wraps.** A transit key deleted in OpenBao (or a disabled transit
mount, which deletes every key in it), or an AWS KMS key whose deletion completed, cannot unwrap the
KEKs any more, and a KEK Secret backup holds only their ciphertext. Every KEK wrapped by that key is
then lost, with the same consequences as [A lost KEK](#a-lost-kek), home checkpoints and trace
destination credentials included.

- Protect the KMS key as you protect the KEK backup. OpenBao: leave `deletion_allowed` off on the
  transit key, do not disable the transit mount, and back up OpenBao itself (a raft snapshot, or your
  storage backend's backup, with the unseal or recovery keys). A transit key backup is not the way: it
  needs `exportable` and `allow_plaintext_backup`, which cannot be turned off again and let the key
  leave the KMS. AWS: a key policy that denies `kms:ScheduleKeyDeletion`.
- AWS deletion waits 7 to 30 days. While it is pending, pods fail to start (see the bullet on
  disabling the key above); `aws kms cancel-key-deletion` and then `aws kms enable-key` bring the key
  back and nothing is lost.
- Before you retire a KMS key, move the KEKs to a new key (add with the new key, activate, rewrap,
  retire) and keep the old key until `keys status` lists no KEK wrapped by it.
- Once the key is gone for good, koinon-api cannot load the key set: it fails when any one KEK does
  not unwrap, so it does not start and `keys forget` cannot run. See
  [Recovering from a lost KMS key](#recovering-from-a-lost-kms-key).

A local install has none of this: no key-admin Job, and koinon-api creates and adds KEKs exactly as
before.

### Recovering from a lost KMS key

Use this when the KMS key is gone for good and no copy of the KEKs it wrapped exists anywhere (a
KEK Secret backup holds only their ciphertext, so it does not help). If the key can still be brought
back (an AWS deletion that is only pending, a transit key restored from an OpenBao snapshot), do that
instead: nothing is lost. What you lose is what [A lost KEK](#a-lost-kek) lists: provider, connection,
MCP server and trace credentials (people enter them again), saved work, and every home checkpoint
whose kopia repository password the KEK sealed (the password exists nowhere else).

The hard case is a lost key that wrapped the **active** KEK. koinon-api needs an active KEK to start,
and once the dead entries are out of the Secret `active` names a KEK that is not there, so the key set
does not load and `keys add`, `keys activate` and `keys forget` all fail on it. The key-admin Job is
the identity that may encrypt with the KMS, so it adds the new KEK and makes it active:

1. **Keep a copy of the Secret**, then find which KEKs the lost key wrapped. This prints each
   wrapped KEK's ID and its wrapping key, never the key material (a local KEK is not shown):

   ```sh
   kubectl -n koinon get secret koinon-kek -o yaml > koinon-kek-before-recovery.yaml
   kubectl -n koinon get secret koinon-kek -o json | jq -r '.data | to_entries[] | select(.key != "active")
     | .key as $id | (.value | @base64d) | select(startswith("{")) | fromjson | "\($id) \(.backend):\(.key)"'
   ```

2. **Remove the entries of those KEKs** from the Secret, and leave `active` as it is. Every KEK that
   stays must still unwrap (a local KEK, or one wrapped by a key that answers): the Job stops with the
   unwrap error otherwise.

   ```sh
   kubectl -n koinon patch secret koinon-kek --type=json \
     -p '[{"op":"remove","path":"/data/<kek-id>"}]'
   ```

3. **Point Koinon at a working KMS key** (a new transit key, or a new AWS key, set up as in the
   sections below; the chart settings of the new key replace the old ones) and run the Job with a new
   KEK ID (`kek-YYYYMMDD-xxxx`) and recovery on:

   ```yaml
   secrets:
     kek:
       kms:
         keyAdmin:
           addKey: kek-20261106-a1b2   # a new ID: IDs are never reused
           activateIfNoActive: true
   ```

   The Job adds the KEK, wrapped by the new key and bound to the install, and makes it active in the
   same write, but only when `active` is missing or names a KEK the Secret does not hold. If the
   Secret has a working active KEK it adds the KEK inactive, as without the flag, and changes nothing
   else. It is safe to run twice. Set `activateIfNoActive` back to `false` afterwards. Every add refuses an ID that the database
   still carries in any envelope, whether the Job recovers or only adds, and so does `keys add --id`
   (the lost active KEK, or any other KEK you removed from the Secret; `keys status` shows them as
   MISSING with a count): IDs are never reused, so pick another. If the database cannot be asked (it
   is down, or not fully migrated yet), the add stops without changing anything; rerun it once the
   database is ready.
4. **Wait for koinon-api and the reconciler to start** (they reload the Secret at least once a minute),
   then back it up: `keys export`. `keys status` now lists the removed KEKs as MISSING and the new
   one as active.
5. **Give up the lost KEKs**, one command each, as in [A lost KEK](#a-lost-kek):

   ```sh
   kubectl -n koinon exec -it deploy/koinon-api -- koinon-api keys forget <kek-id>
   ```

   New home repositories and sealed values use the new active KEK from here on.

On a **local** install (no KMS, no key-admin Job) there is no such Job. If the active KEK's entry was
lost from the Secret and no copy exists, add a new local KEK and set `active` to it in one patch,
then run `keys forget` for the lost one. A local KEK's stored form is already base64 text of 32
random bytes, so write it through `stringData`: the API server base64-encodes it once into `data`
(a value put straight into `/data` would have to be base64 twice). Use a new ID like
`kek-20261106-a1b2`:

```sh
ID=kek-20261106-a1b2
kubectl -n koinon patch secret koinon-kek --type=merge \
  -p "{\"stringData\":{\"$ID\":\"$(head -c 32 /dev/urandom | base64)\",\"active\":\"$ID\"}}"
```

Back the Secret up (`keys export`) as soon as the pods are running again.

### The install binding (old issue 615)

Every KEK the key-admin Job wraps is bound to the install: the KMS call carries the install ID as
part of its authenticated data, so the wrapped KEK opens only next to the database it was made for.
A KEK Secret copied to another install (one with its own database, even on the same KMS key), or a
database restored into the wrong install, then fails to load with "cannot open envelope …
it was wrapped for install A and this is install B", the same error as a wrong key, never as a KMS
outage.

**Where the install ID comes from.** The first migration of a release with old issue 615 writes a random
UUID into the table `install_identity` of the database. Triggers refuse UPDATE, DELETE and TRUNCATE
of it; they guard against mistakes, not against the database owner: the table's owner can DROP the
table or `ALTER TABLE … DISABLE TRIGGER`, and `pg_restore --disable-triggers` and
`session_replication_role = replica` skip the triggers. A changed ID only makes the bound KEKs stop
opening (fail closed); it opens nothing. `keys status` prints it ("The install ID is …"). It is not secret. The database is the
one place that is born with the data and travels with it, so the ID stays the same through:

| Event | Install ID |
| --- | --- |
| Upgrade (any number of releases) | Unchanged: the row is written once. |
| Restore of a database backup (`pg_dump`, CNPG backup, a snapshot) | The backup's, which is the install that wrote the values. |
| Restore of the KEK Secret from its backup | Unchanged: the ID is not in the Secret (each KEK records it only as a label). |
| `helm uninstall` and reinstall that keeps the database (external Postgres, a retained PVC) | Unchanged. |
| A move to another namespace or cluster with the same database | Unchanged. |
| A new, empty database | New: it is a new install, and a KEK Secret left over from the old one does not open. Delete that Secret (or restore the old database). |

**A clone is the same install.** A copy of the database (a restored backup, a CNPG clone, a
snapshot) keeps the install ID, so the production KEKs open next to it: a staging environment made
from a copy of production is, to the binding, production. For a staging copy that must **not** open
the production KEKs:

- give it its own KMS key (its own transit key, or its own AWS KMS key) and never grant its
  service accounts decrypt on the production key; this is what keeps it out, the install ID does not;
- never copy the production KEK Secret into it; it starts with no KEK Secret and its key-admin Job
  creates a new one only while the database holds no encrypted values, so remove the copied values
  first (or start from an empty database, which is also a new install ID) and enter the staging
  credentials again.

**A data-only restore onto a migrated database fails.** `pg_restore --data-only` (or a `COPY` of
the tables) onto a database that already ran the migrations stops on `install_identity`: the target
already holds its own row and the table has one row only (`duplicate key value violates unique
constraint "install_identity_pkey"`). Restore schema and data into an empty database instead (a
plain `pg_restore`, or drop and recreate the target first), so the backup's install ID, the one its
KEKs are bound to, is the one that comes back. Do not delete the target's row to make room: its
triggers refuse that, and an ID that is not the backup's leaves the bound KEKs closed.

**Restore order does not matter.** Restore the database and the KEK Secret in either order; until
both are back, the pods do not start (fail closed) and start on their own once both match. A running
pod reads the install ID once, so restart koinon-api and the reconciler after a database restore,
as after any restore under running pods. If they
do not match, `keys status` and the pod log name both install IDs: restore the KEK Secret backup that
belongs to this database, or the database that belongs to this Secret.

Rejected sources, and why: a chart value is lost on a reinstall that forgets it and is not born with
the data; a generated Secret would be backed up and restored with the KEK Secret, which defeats the
binding; the namespace or cluster UID changes on a move or a reinstall that keeps the data.

**KEKs wrapped before old issue 615 keep working.** They are stored in the old form (`"fmt":1`) and open as
before; `keys status` shows them as `…, unbound` in WRAPPED BY and lists them. koinon-api and the
reconciler export `koinon_kek_unbound` (no labels), the number of KMS-wrapped KEKs in the loaded
key set that are not bound, set on every load (at start and about once a minute); local KEKs are
not counted. While it is above 0 each process logs a warning with the count and the remedy (at most
every 10 minutes). Alert on `koinon_kek_unbound > 0` if you want every KEK bound. Bind them either by
rotating (add, activate, rewrap, retire, as above), or by setting
`secrets.kek.kms.keyAdmin.rewrapUnbound: true` and upgrading: the key-admin Job wraps each unbound KEK
of the primary wrapping key again, bound (the KEK itself is unchanged, so no value needs a rewrap).
The run is idempotent; a KEK wrapped by another KMS key stays unbound and the Job says so. Back up
the KEK Secret afterwards. A local KEK is never bound (it is not wrapped).

**The bound-only marker.** Binding wraps the same KEKs again: an old backup of the KEK Secret still
holds them unbound, and an unbound KEK opens next to any database that can reach the KMS key. So once
every KEK is bound, the install records it: a row in the table `kek_bound_only`. From then on every
Koinon process (koinon-api, the reconciler, the key-admin Job, `keys` commands) refuses a KMS-wrapped
KEK stored unbound, with an error that names the KEK, when the marker was set and by what ("…the
bound-only marker of this install was set at 2026-11-03T12:00:00Z, when the key-admin Job
(--rewrap-unbound) bound the last unbound KEK"). It is set

- on a new install, by the key-admin Job (or koinon-api, for local KEKs) when it creates the first KEK,
  before the KEK Secret exists: a new install never holds an unbound KMS KEK;
- by the key-admin Job with `rewrapUnbound: true`, once a run leaves no KMS-wrapped KEK unbound. A run
  that leaves one unbound (another KMS key) does not set it and names the KEKs: rotate them out and run
  the Job again.

`keys status` prints it ("Bound only: set at …" or "Bound only: not set"). Koinon only ever sets it;
nothing in Koinon clears it, and triggers refuse UPDATE, DELETE and TRUNCATE on the table. Those guard
against mistakes, not against a determined database owner: the table's owner can drop the table or
disable the triggers, and `pg_restore --disable-triggers` and `session_replication_role = replica`
skip them.

A process reads the marker each time it loads the key set while the marker is unset and the set holds
an unbound KMS KEK (at start and on every reload, about once a minute), and keeps it once it reads it
set. A process that started before the marker was set refuses unbound KEKs from its next load on; a
failed reload keeps the key set it already has, which after the rewrap holds the bound forms. If the
marker cannot be read (the database is down), an unbound KEK is refused (fail closed); bound and local
KEKs do not need it. Every process that opens KEKs reads the marker; there is no mode that skips it. The
`keys` commands run without `--database-url` (or `DATABASE_URL`) refuse an unbound KMS KEK for the
same reason. A read of the marker is bounded like a KMS call (10 seconds): a hung database fails that
load and logs the timeout, instead of blocking every load after it.

**When a KEK is refused as unbound.** The pods log the refusal (and the `koinon_kek_unbound` gauge
stays above 0), and `keys status` lists every refused KEK ("REFUSED: KEK … is stored unbound").
Binding cannot help here: `rewrapUnbound` has to open a KEK to bind it. Instead:

- Restore a KEK Secret backup taken after the KEKs were bound.
- On an install whose database holds no encrypted values yet, delete the KEK Secret and rerun the
  key-admin Job, which creates a new, bound one: no values are lost. This is the usual case when
  GitOps or a backup restore (Velero, for example) puts an old KEK Secret in place on a new install,
  whose key-admin Job set the marker before it found the Secret already there.

**Rotate as well if an old backup may have leaked.** The marker stops an old KEK Secret backup only
where this database is: an install whose database lacks the marker (a clone taken before it was set, a
rollback below old issue 615, another install on the same KMS key) still opens that backup's unbound KEKs. If
a KEK Secret backup from before the rewrap may be in the wrong hands, rotate the KEKs as well (add,
activate, rewrap, retire, as above): only rotation makes an old backup useless everywhere.

**Rolling back below old issue 615.** A bound KEK is stored as `"fmt":2`, which a release before old issue 615
refuses ("unsupported format 2"): its pods fail closed and never misread it. Every new KMS-wrapped
KEK is bound, so before rolling back below old issue 615 use the steps in "Rolling back below the release that
added key wrapping" below (back to local KEKs), or roll back to a key set that held only unbound
KEKs. Leave `rewrapUnbound` off until you will not roll back that far: once it binds the last KEK the
marker is set, the Job says so, and a rollback below old issue 615 no longer works with this key set (those
releases cannot read bound KEKs, and the unbound backups no longer open on this database once you
come forward again).

### Setting up OpenBao (or Vault) transit

On the OpenBao side (the release in namespace `koinon`, chart defaults for the names):

```sh
bao secrets enable transit
bao write -f transit/keys/koinon-kek
# koinon-api and the reconciler: decrypt only.
bao policy write koinon-decrypt - <<'HCL'
path "transit/decrypt/koinon-kek" { capabilities = ["update"] }
HCL
# The key-admin Job: encrypt and decrypt. Grant update, never create: with create, encrypt makes a
# missing key on the spot, so a typo in the key name would wrap the KEK under a new key.
bao policy write koinon-key-admin - <<'HCL'
path "transit/encrypt/koinon-kek" { capabilities = ["update"] }
path "transit/decrypt/koinon-kek" { capabilities = ["update"] }
HCL
bao auth enable kubernetes
bao write auth/kubernetes/config kubernetes_host=https://<api server>:443 \
  kubernetes_ca_cert=@ca.crt token_reviewer_jwt=@reviewer.jwt
for r in api:koinon-decrypt reconciler:koinon-decrypt key-admin:koinon-key-admin; do
  bao write auth/kubernetes/role/koinon-${r%%:*} bound_service_account_names=koinon-${r%%:*} \
    bound_service_account_namespaces=koinon token_policies=${r#*:} token_ttl=1h
done
```

Each role binds one service account: `koinon-api`, `koinon-reconciler` and `koinon-key-admin`
(`<release fullname>-<component>`). OpenBao checks the pods' tokens with the TokenReview API; an
OpenBao outside the cluster needs a reviewer token (`reviewer.jwt` above) of a service account bound
to the `system:auth-delegator` ClusterRole. An OpenBao in the cluster can leave
`token_reviewer_jwt` out and use its own.

In the chart values:

```yaml
secrets:
  kek:
    kms:
      backend: openbao
      openbao:
        address: https://openbao.openbao.svc:8200   # the active node; redirects are refused
        caConfigMap: openbao-ca                     # a ConfigMap with ca.crt, when the CA is private
        key: koinon-kek
        # transitMount, authMount, namespace, timeout and roles.{api,reconciler,keyAdmin}
        # default to the names above.
```

On install the key-admin Job (`koinon-key-admin-<hash>`) creates the KEK Secret with one KEK wrapped
by OpenBao, and koinon-api restarts until the Secret is there. To add a KEK, for a rotation or the
move from local KEKs, set `secrets.kek.kms.keyAdmin.addKey` to a new ID (for example
`kek-20271103-a1b2`) and upgrade: the Job runs once for every change of its inputs and adds that KEK,
inactive. Back up the KEK Secret, then `koinon-api keys activate <id>` and `keys rewrap` as in the
rotation steps. With `secrets.kek.existingSecret` the Job adds to your Secret and never creates it.

#### When the key-admin Job fails for good

The Job retries a few times, then stays failed (its log says why: OpenBao unreachable, a role or
policy missing, the transit key missing). A failed Job does not run again on a plain upgrade with the
same inputs, because its name is the hash of those inputs. Fix the cause, then:

```sh
kubectl -n koinon logs job/<the koinon-key-admin-… Job>      # why it failed
kubectl -n koinon delete job <the koinon-key-admin-… Job>
helm upgrade koinon <chart> -n koinon -f values.yaml          # renders the Job again; it runs once more
```

`helm install --wait` (and `helm upgrade --wait`) times out while the Job keeps failing on install,
since koinon-api waits for the KEK Secret. A plain `helm upgrade` does not wait for the Job at all;
only `--wait-for-jobs` does, so check the Job yourself after an upgrade that adds a KEK.

### Setting up AWS KMS

Koinon wraps with one symmetric encryption key (`SYMMETRIC_DEFAULT`, usage `ENCRYPT_DECRYPT`) that
you create; it never creates, enables or schedules keys. Every request names that key and carries
the encryption context `koinon=kek-wrap/v1`. The pods sign in with their pod identity only: IRSA (an
IAM role on the ServiceAccount) or EKS Pod Identity. Koinon never reads an access key, the shared
config files or the node's instance metadata; static `AWS_ACCESS_KEY_ID` and friends in the env are
ignored with a warning, so the pod cannot fall back to the node's role or to a key someone left in
the env.

One IAM role per identity (the release in namespace `koinon`, chart default names):

| Role (example name)          | ServiceAccount        | Actions on the key           |
|------------------------------|-----------------------|------------------------------|
| `koinon-api-decrypt`         | `koinon-api`          | `kms:Decrypt`                |
| `koinon-reconciler-decrypt`  | `koinon-reconciler`   | `kms:Decrypt`                |
| `koinon-key-admin-encrypt`   | `koinon-key-admin`    | `kms:Encrypt`, `kms:Decrypt` |

The identity policy of the decrypt roles (the key-admin role adds `kms:Encrypt`):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["kms:Decrypt"],
    "Resource": "arn:aws:kms:eu-central-1:111122223333:key/<key id>",
    "Condition": {"StringEquals": {"kms:EncryptionContext:koinon": "kek-wrap/v1"}}
  }]
}
```

The condition is optional; it keeps the roles from decrypting anything that was not wrapped by
Koinon. The key policy must let IAM policies grant access (the default key policy's account
statement does), or name the three roles itself. Grant nothing else: no `kms:CreateKey`,
`kms:GenerateDataKey` or `kms:ReEncrypt*`.

With IRSA, each role trusts the cluster's OIDC provider for its one ServiceAccount
(`system:serviceaccount:koinon:koinon-api`, and so on, audience `sts.amazonaws.com`), and the chart
annotates the ServiceAccounts from `roleArns`. With EKS Pod Identity, leave `roleArns` empty and
create one association per ServiceAccount (`aws eks create-pod-identity-association --namespace
koinon --service-account koinon-api --role-arn ...`); the roles then trust `pods.eks.amazonaws.com`.
agentd gets no role: it holds no keyring.

In the chart values:

```yaml
secrets:
  kek:
    kms:
      backend: awskms
      awskms:
        region: eu-central-1
        keyId: arn:aws:kms:eu-central-1:111122223333:key/<key id>   # the key ARN (recommended)
        roleArns:                                                 # IRSA; empty with EKS Pod Identity
          api: arn:aws:iam::111122223333:role/koinon-api-decrypt
          reconciler: arn:aws:iam::111122223333:role/koinon-reconciler-decrypt
          keyAdmin: arn:aws:iam::111122223333:role/koinon-key-admin-encrypt
        # endpoint, stsEndpoint: https://... of VPC endpoints with their own DNS names
        # caConfigMap: a ConfigMap with ca.crt, for a TLS-inspecting egress proxy
        # timeout: bound on one KMS call (default 5s)
```

Notes:

- Name the key by its key ARN. Every wrapped KEK records the key as written in `keyId` and is read
  with that key, so keep the spelling unchanged. An alias works, but if the alias is later pointed at
  another key, the KEKs wrapped before refuse to unwrap with the wrong-key error ("cannot open
  envelope"): aliases are for keys that never move.
- Calls go to `https://kms.<region>.amazonaws.com` and `https://sts.<region>.amazonaws.com` (with
  IRSA) unless you set the endpoints. Koinon honours `HTTPS_PROXY` and `NO_PROXY` for them; the EKS
  Pod Identity agent (a link-local address) is always called directly. Redirects are never followed.
- Koinon checks the key on every reload with a dry-run Decrypt (`DryRun` with the modifier
  `IGNORE_CIPHERTEXT`): KMS checks the identity and the `kms:Decrypt` right on the key, encryption
  context condition included, without decrypting anything. Healthy is exactly KMS's
  `DryRunOperationException`, its answer to a dry run that would have been allowed. Any other answer
  (`AccessDeniedException` or another KMS error), an identity that cannot sign in and a network error
  read as unreachable (`koinon_kek_active_reachable` 0), never as a wrong key.
- An endpoint that does not support the dry run (an emulator such as LocalStack, which ignores it,
  or a KMS-compatible service that rejects the parameters) reads as unreachable too, with the reason
  "the endpoint ignored or rejected DryRun; the decrypt right is unverified". There is no weaker
  fallback: a plain Decrypt of a junk ciphertext cannot prove the right, since KMS may refuse the
  junk before it checks it. Wrapping and unwrapping still work against such an endpoint; only the
  health check stays at 0. AWS KMS itself supports the dry run.
- Disabling the key or scheduling its deletion stops every pod from loading the key set on its next
  start. Before you retire the key, move the KEKs to a new key (the steps above: add with the new
  `keyId`, activate, rewrap, retire), and keep the old key enabled until `keys status` lists no KEK
  wrapped by it.
- The key-admin Job, its install and its retries work as with OpenBao. When it fails for good (a
  role, trust policy or key policy missing, the key missing or disabled), follow "When the key-admin
  Job fails for good" above.

### Rolling back below the release that added key wrapping

Koinon releases before key wrapping read only local KEKs. While the Secret holds any KMS-wrapped KEK,
such a release cannot read the key set: koinon-api does not start and the reconciler keeps waiting
for keys. Before you roll back that far, switch the wrapping back to local while the KMS stays
configured for reading, add a local KEK, `keys activate` it, `keys rewrap`, and
`keys retire` every KMS-wrapped KEK, so `keys status` shows `local` for every KEK left. A local
install's Secret is unchanged by key wrapping and needs nothing.

## Limits and notes

- AES-GCM with random nonces is safe for 2^32 invocations per key. A KEK is invoked once per write and
  once per rewrap, so a KEK would need billions of values. The yearly period is for hygiene, not for
  this limit.
- A KMS is optional (see above); without one, the KEKs are protected only by the Secret's access
  rules and by etcd encryption at rest, if the cluster has it on.
- `koinon-api keys` changes the Secret with the `update` permission the chart grants `koinon-api` on
  that one Secret.
