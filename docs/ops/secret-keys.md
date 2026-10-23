# Secret keys: rotation, backup and compromise

Koinon stores provider API keys and connection secrets (a GitHub App's private key) in Postgres as
envelopes: each value has its own data key (DEK), wrapped by a key-encryption key (KEK) that lives
in a Kubernetes Secret. This runbook says how to look after the KEKs. The design is in
[ADR 0056](../adr/0056-model-providers-pricing-and-budgets-per-org.md); the sources for the
numbers and the procedure are in [the research](../research/kek-rotation.md).

## What is where

| What | Where | Who can read it |
| --- | --- | --- |
| KEKs | Secret `koinon-kek` (or `secrets.kek.existingSecret`): one file per KEK, named by its ID, plus `active` | `koinon-api` and `koinon-reconciler` mount it. Nothing else does. |
| Wrapped DEKs and ciphertext | `providers.api_key`, `connections.secret_envelope` in Postgres | Anyone with the database, but they need the KEK to open them. |
| Opened provider keys | Secret `koinon-gateway-providers` | Only the gateway pod mounts it. |

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

## Back up the KEK Secret

Do this at install and after every `keys add`:

```sh
kubectl -n koinon get secret koinon-kek -o yaml > koinon-kek-backup.yaml
```

Keep the file with your other secrets, apart from the database backups. **A database backup needs the
KEKs that wrapped it.** Keep every KEK, including retired ones, for as long as a database backup that
used it exists. Retiring a KEK removes it from the Secret, not from your escrow. If every copy of a KEK
is lost, the values wrapped by it are lost: add the providers and connections again.

The chart marks the generated Secret `helm.sh/resource-policy: keep`, so `helm uninstall` leaves it.

## Rotate on schedule

Do it once a year, or when `keys status` warns. No downtime, no restart.

1. Add a key: `koinon-api keys add`. It prints the new ID and does not activate it. Back up the Secret.
2. Activate it: `koinon-api keys activate <new-id>`. New values are wrapped by it once the pods have the
   changed Secret (kubelet takes up to about a minute; both pods reload their keys every minute).
3. Wait two minutes, then re-wrap the old values: `koinon-api keys rewrap`. This wraps every stored
   DEK under the active KEK. It never decrypts a payload and can be run again.
4. Check `keys status`: no value should use the old KEK. If one does (a pod wrote a value with the old KEK
   before it saw the change), run `keys rewrap` again.
5. Retire the old key: `koinon-api keys retire <old-id>`. It refuses while any value uses the KEK.
   Keep a copy of the old key in your backup escrow.

DEKs need no schedule: every write makes a new one.

## Suspected compromise

A KEK compromise means someone may hold a KEK and the database. Rotating the KEK is not enough: they
can already open every DEK they copied. Do this, in this order:

1. **Stop wrapping with the key.** `keys add`, then `keys activate <new-id>`.
2. **Re-encrypt with new DEKs.** `koinon-api keys reencrypt`. It decrypts each value in memory and
   seals it again under a fresh DEK and the active KEK. Then `keys retire <compromised-id>`. Do not
   escrow the compromised key with your normal backups; keep it only for backups you must still read,
   in a place the attacker cannot reach.
3. **Rotate the real credentials at their providers.** Re-encrypting does not undo a leak. Issue new
   OpenRouter, OpenAI or other provider keys and a new GitHub App private key, and replace them in
   Koinon (the admin API, or by editing the declarative Secrets). Revoke the old ones at the provider.
4. Rotate the admin API tokens: revoke every token and create new ones.
5. Look at the audit log for changes made while the key was exposed.

If a **database backup** leaked but not the KEKs, nothing opens, but plan a normal rotation anyway.

## Declarative Secrets

Providers and connections declared by labelled Secrets (see the Models guide) carry their credentials
in the Secret's `stringData`, which Kubernetes stores unencrypted unless you enable encryption at rest
for Secrets. Koinon copies them into the store and does not clear the Secret. Treat write and read access
to Koinon's namespace as platform-admin level.

## Limits and notes

- AES-GCM with random nonces is safe for 2^32 invocations per key. A KEK is invoked once per write and
  once per rewrap, so a KEK would need billions of values. The yearly period is for hygiene, not for
  this limit.
- Koinon does not use a KMS. The envelope has a `kek_id` so a KMS adapter can follow.
- `koinon-api keys` changes the Secret with the `update` permission the chart grants `koinon-api` on
  that one Secret.
