<!-- SPDX-License-Identifier: Apache-2.0 -->

# The gateway recipient key

- Status: Accepted, with [ADR 0097](../adr/0097-koinon-owned-gateway.md) decision 12 (the reasons; the ADR wins if the two disagree)
- Area: security-audit
- Tags: secrets

koinon-gateway holds the private half of a key pair. koinon-api seals each stored MCP credential to the
public half (envelope v2), and only the gateway can open it. This page says how an operator runs the
key and what the install reports. It covers issue #1151, slice 2: the key lifecycle. No caller seals v2
yet; that is slice 3.

## Use this page when

You turn on `gateway.mcp.driver=koinon`. The key is on by default with that driver
(`gateway.mcp.koinon.recipientKey.enabled`).

## What runs

- The Job `<release>-gateway-key-admin-<hash>` runs `koinon-gateway keys <action>`. Its action is
  `gateway.mcp.koinon.recipientKey.keyAdmin.action`: `init` (the default), `rotate` or `retire`.
  Change `rotationId` to run an action again. Each run is idempotent.
- `init` makes the first key and activates it after every gateway pod loaded it. The pods are ready with
  no key, so the Job and the pods start together. A pod that cannot unwrap a key a row names becomes not
  ready.
- `rotate` adds a pending key. The gateway pods load it and report. The Job then makes it active and
  the old key `retiring`. Credentials sealed to the old key stay readable.
- `retire` retires a `retiring` key after a grace wait, and only when no stored credential names it.
  It then deletes the wrapped private key.
- `koinon-gateway keys status` prints every key and which gateway pods loaded it.

## Where the keys are

- Public keys: the table `gateway_recipient_keys`. Rows are never deleted. The chart's database roles
  decide who can write: the key-admin login (`koinon_gateway_key_admin`) inserts a key and moves its
  state; the gateway login reads the rows and reports its load; koinon-api only reads.
- Private keys, wrapped: the Secret `<release>-gateway-recipient-key`. Only the gateway Deployment mounts
  it. A native ValidatingAdmissionPolicy (`gateway-key-guard`) lets only the key-admin service account
  write it.

## Custody

The wrapping key is a KMS key of its own. It is never the KEK's key. The chart refuses a render where
the two are equal.

| Backend | Who can unwrap | What it means |
|---|---|---|
| `local` | koinon-api and the gateway | The Secret holds the private key as it is. koinon-api can read the Secrets of its namespace. Nothing keeps the key from koinon-api. |
| `openbao`, `awskms` | The gateway only | koinon-api and the reconciler have no right on the wrapping key. |

The install reports the level: the NOTES text, and the metric `koinon_gateway_recipient_key_custody`
with the label `custody` (`kms` or `local`). Do not state that koinon-api cannot decrypt on `local`.

## Move the credentials to v2

New credentials are sealed to the gateway key when a key is active and the write gate is open. Old ones stay
v1 until you move them:

1. `koinon-api keys seal-mcp --dry-run` opens each v1 credential that would move and prints the counts.
   It writes nothing. Add `--org <uuid>` to limit it to one org.
2. `koinon-api keys seal-mcp` moves them. It is safe to run again: it skips v2 values, and a row that
   changed while the command ran is skipped and taken by the next run. It refuses, and changes nothing,
   while an expected pod cannot read v2.
3. `koinon-api keys verify` must print 0 MCP credentials in v1. `keys status` shows the v2 count for each key ID.

The report names counts, orgs and row IDs. It never prints a credential value.

`keys seal-mcp --to-v1` is the reverse move. It needs the private gateway key, so the `koinon-api` command
refuses it and says why. Do not plan a rollback that relies on it before the gateway side has a command for it.

The migration Job can refuse an upgrade while a v1 credential remains (`koinon-api migrate --require-mcp-v2`).
The message names the commands above.

## Re-seal after a rotation

After a rotation the new key is active. Credentials sealed to the old key stay readable, but the old key
cannot retire while one names it. To move them, set
`gateway.mcp.koinon.recipientKey.reseal.enabled=true` and a new `reseal.rotationId`. The chart then runs the
Job `<release>-reseal-<hash>`: `koinon-gateway keys reseal`. It runs as the gateway service account, because
the gateway identity holds the right to unwrap the old private key. (On `local`, koinon-api can read the key Secret too.) Its database login is `koinon_reseal`, which can read the key
table and write only the credential column of `mcp_servers`. It writes with a compare-and-swap, so a row that
changed meanwhile is skipped and the next run takes it. Use `reseal.dryRun=true` to count first, and
`reseal.org` to limit it to one org. When the Job reports no credential left on the old key, run the
`retire` key action.

Run the re-seal in a later upgrade than the rotation. The Job must see the new key as active. If both Jobs
run in one upgrade, the re-seal can find the old key still active, move nothing and end. Then change
`rotationId` to run it again. The Job has no egress except DNS, the database and the KMS.

## OpenBao policy

Use a transit key `gateway.mcp.koinon.recipientKey.kms.openbao.key` (default `koinon-gateway`) that
differs from the KEK's key. Make two Kubernetes auth roles:

- The gateway role (`koinon-gateway`), bound to the service account `<release>-mcp-gateway`: `update` on
  `transit/decrypt/koinon-gateway`.
- The key-admin role (`koinon-gateway-key-admin`), bound to the service account
  `<release>-gateway-key-admin`: `update` on `transit/encrypt/koinon-gateway` and
  `transit/decrypt/koinon-gateway`.

Do not grant `create` on the key path: a mistyped key name would make a new key. The gateway pod mounts
no automatic service account token. It reads a projected token with the audience
`...openbao.audience`. The roles `koinon-api` and `koinon-reconciler` get no policy on this key.

## AWS KMS policy

Use a symmetric key `gateway.mcp.koinon.recipientKey.kms.awskms.keyId` that differs from the KEK's key.

- The gateway role (`roleArns.gateway`, trusted by `<release>-mcp-gateway`): `kms:Decrypt` on it.
- The key-admin role (`roleArns.keyAdmin`, trusted by `<release>-gateway-key-admin`): `kms:Encrypt` and
  `kms:Decrypt` on it.
- No other role of the release gets a statement on this key.

## Database logins

`koinon-api migrate` (and `--grants-only`) grants two group roles: `koinon_gateway_key_admin` and
`koinon_gateway_reseal`. In cnpg mode the chart creates them and the logins `koinon_gwkeyadmin` and
`koinon_reseal`, each a member of its own group role only.

With `postgres.mode=external`, create the same group roles and logins and put each URI in the Secret under
`postgres.external.keys.gwkeyadmin` and `postgres.external.keys.reseal`. If every component shares one
database role, the grants separate nothing: koinon-api could then insert a public key. Use a separate
role for the key-admin login, or accept that the database does not enforce this boundary. The table
triggers only stop mistakes. They do not stop the table owner.
