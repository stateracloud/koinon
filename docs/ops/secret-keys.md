# Secret keys: rotation, backup and compromise

Koinon stores provider API keys and connection secrets (a GitHub App's private key) in Postgres as
envelopes: each value has its own data key (DEK), wrapped by a key-encryption key (KEK) that lives
in a Kubernetes Secret. This runbook says how to look after the KEKs. The design is in
[ADR 0056](../adr/0056-model-providers-pricing-and-budgets-per-org.md); the sources for the
numbers and the procedure are in [the research](../research/kek-rotation.md).

## What is where

| What | Where | Who can read it |
| --- | --- | --- |
| KEKs | Secret `koinon-kek` (or `secrets.kek.existingSecret`): one file per KEK, named by its ID, plus `active` | `koinon-api` reads it through the Kubernetes API; `koinon-reconciler` mounts it. Nothing else does. |
| Wrapped DEKs and ciphertext | `providers.api_key`, `connections.secret_envelope` in Postgres | Anyone with the database, but they need the KEK to open them. |
| Opened provider keys | Secret `koinon-gateway-providers` | Only the gateway pod mounts it. |

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

## Back up the KEK Secret

Do this once the install is up (the Secret exists after `koinon-api` has started) and after every
`keys add`:

```sh
kubectl -n koinon get secret koinon-kek -o yaml > koinon-kek-backup.yaml
```

Keep the file with your other secrets, apart from the database backups. **A database backup needs the
KEKs that wrapped it.** Keep every KEK, including retired ones, for as long as a database backup that
used it exists. Retiring a KEK removes it from the Secret, not from your escrow. If every copy of a KEK
is lost, the values wrapped by it are lost: add the providers and connections again.

The Secret is not part of the Helm release, so `helm uninstall` leaves it, and so does a GitOps prune.
Delete it yourself when you delete the database.

## Rotate on schedule

Do it once a year, or when `keys status` warns. No downtime, no restart.

1. Add a key: `koinon-api keys add`. It prints the new ID and does not activate it. Back up the Secret.
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

A platform admin allows internal upstreams in the chart value `providers.allowedInternalHosts`: names
(optionally with a port), IPs or CIDRs, for example `["ollama.lab.example.com:11434", "10.20.0.0/24"]`.

**Gateway egress policy.** With `networkPolicy.enabled` the chart restricts agentgateway's egress with a
standard `networking.k8s.io/v1` NetworkPolicy (no CNI-specific resources): DNS, `koinon-api`'s OTLP and
tools ports, and the public internet with the private, shared, link-local and loopback ranges carved
out. Allow an internal upstream there too, by address, with `networkPolicy.gatewayEgressTo`:

```yaml
networkPolicy:
  gatewayEgressTo:
    - to: [{ipBlock: {cidr: 10.20.0.0/24}}]
      ports: [{port: 11434}]
```

An MCP target other than `koinon-api`'s tools listener (a future remote MCP server, or any other
upstream the gateway is configured with) is blocked the same way when it is on a private address: add
it to `networkPolicy.gatewayEgressTo` too. The IPv6 carve-outs include NAT64 (`64:ff9b::/96`) and 6to4
(`2002::/16`), so an IPv6-only cluster that reaches the internet through NAT64 must add its own rule.

Limits: the carve-outs are the standard private ranges, so a cluster that numbers pods or Services from
public addresses needs its own rules; the policy matches addresses, not names, so keep it and
`providers.allowedInternalHosts` in agreement; and a CNI that does not enforce egress policy ignores
it (the default provider URL check still applies).

## Limits and notes

- AES-GCM with random nonces is safe for 2^32 invocations per key. A KEK is invoked once per write and
  once per rewrap, so a KEK would need billions of values. The yearly period is for hygiene, not for
  this limit.
- Koinon does not use a KMS. The envelope has a `kek_id` so a KMS adapter can follow.
- `koinon-api keys` changes the Secret with the `update` permission the chart grants `koinon-api` on
  that one Secret.
