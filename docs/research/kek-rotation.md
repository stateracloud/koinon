# KEK and DEK rotation for Koinon's per-org secrets

- Status: Complete
- Date: 2026-10-23
- Area: security-audit
- Tags: encryption, secrets

Researched 2026-10-23. Primary sources only. Each claim is tagged:

- **[E]** established: the cited source says it.
- **[I]** inference: my reasoning from the sources, applied to Koinon.

Source keys like [S1 §5.3.6] refer to the Sources list at the end.

## Short answer

- Use a fresh random DEK for every write of every secret value. Then DEKs never need scheduled rotation. [E, S9 "DEK best practices"]
- Rotating a KEK means making a new key version the one used for wrapping. Old versions stay for unwrapping. Existing data is not re-encrypted on its own. [E, S3, S6, S10, S12]
- After rotation, rewrap the stored DEKs under the new KEK. The payload ciphertext does not change. Then retire the old KEK once no row uses it. [E, S5, S11, S13; I for the Koinon steps]
- Compromise is different. Stop using the key, re-encrypt payloads under new DEKs, and rotate the underlying credentials themselves. [E, S1 §5.5; S7; I for the credential step]
- Default KEK cryptoperiod for Koinon: one year of active use, configurable. Keep retired KEKs for as long as backups that need them exist. [E for the source numbers; I for the choice]

## 1. Cryptoperiods and what "rotation" means

### What the sources recommend

- NIST Table 1 suggests an originator-usage period (time a key may apply protection) of under 2 years for both symmetric data-encryption keys and symmetric key-wrapping keys. The recipient-usage period (time it may still decrypt or unwrap) is under OUP + 3 years. [E, S1 §5.3.6 Table 1]
- NIST says the numbers are "only rough order-of-magnitude guidelines". Longer periods need serious thought about the risks. [E, S1 §5.3.6]
- A key-wrapping key that wraps very many keys in a short time should get a day or a week. One that wraps relatively few keys can go up to two years. [E, S1 §5.3.6 item 7b]
- A wrapping key may be kept so that stored data can be recovered later. Then the recipient period can be much longer than the originator period. [E, S1 §5.3.6 item 7a]
- A key used after its originator period must not encrypt or wrap new data. It may still decrypt. [E, S1 §5.3.6 items 6a, 7a]
- Stored data often gets longer cryptoperiods, because re-encrypting it is costly. [E, S1 §5.3.3.1]
- Short cryptoperiods can backfire when re-keying is error-prone and availability matters most. [E, S1 §5.3.2]
- AWS KMS rotates yearly by default (365 days). The period is configurable. [E, S3 "Rotation period"]
- Azure Key Vault: "rotate encryption keys at least every two years". [E, S12 intro]
- Kubernetes KMS v2 recommends rotating the KEK at least every 90 days, because "you don't control the number of writes performed with the DEK". [E, S14 "Understanding key_id and Key Rotation"]
- Tink suggests 30- to 90-day keys for symmetric encryption in a KMS. [E, S16 "Key rotation"]
- Google Cloud KMS recommends automatic rotation on a schedule. It says not to rely on manual or irregular rotation as a main security control. [E, S6]

### What rotation means for each key

- **KEK rotation means a new key version.** New wraps use the new version. Old versions stay to unwrap. No stored data changes. [E, S3 "Using key material"; S6; S12 "Important"; S10 "Key rotation"]
- AWS states it plainly: rotation "does not rotate the data keys … or re-encrypt any data". It "will not mitigate the effect of a compromised data key". [E, S3 Note]
- Azure says rotation "re-wraps data encryption keys (DEKs) with the new key version — it does not re-encrypt the underlying data". Both versions must stay enabled until rewrapping is done. [E, S12 "Important"] Here "rotation" means the service's own rewrap, not Key Vault creating a version. [I]
- **DEK rotation is not needed if each write gets a new DEK.** Google: "Generate a new DEK every time you write the data. This means you don't need to rotate the DEKs." [E, S9]
- Google also says not to share a DEK between two users. [E, S9]
- AWS: data keys are best used "once, or just a few times". [E, S3 "Why rotate KMS keys?"]
- **Re-encrypting payloads** is a separate act. It decrypts the data and encrypts it under a new key. Google ties it to suspected compromise and to removing reliance on old versions. [E, S7; S6]

## 2. Rotation mechanics, system by system

### The common pattern

All of the systems below do the same five things. [I, synthesis of S3, S5, S6, S10, S13, S15]

1. Store a key identifier or version with every ciphertext.
2. Encrypt or wrap with one primary key.
3. Keep older keys only to decrypt or unwrap.
4. Rewrap or re-encrypt old ciphertexts under the primary key.
5. Retire old keys after a check that nothing still needs them.

### AWS KMS

- Automatic or on-demand rotation adds new key material. The key ID stays the same. [E, S3]
- Decrypt picks the right material automatically. You cannot choose it. [E, S3 "Using key material"]
- AWS keeps all old material for AWS-generated keys until the key is deleted. [E, S3 "Managing key material"]
- `ReEncrypt` decrypts and re-encrypts "entirely within AWS KMS". It can move ciphertext to a new KMS key or change the encryption context. [E, S4]
- Symmetric KMS ciphertext carries metadata that names the key. AWS still recommends passing `SourceKeyId`. [E, S4]
- The response reports source and destination key material IDs. [E, S4 "Response Elements"]
- `GenerateDataKey` returns a plaintext DEK and a wrapped copy. Erase the plaintext from memory soon. Store the wrapped copy with the data. [E, S5]
- `GenerateDataKeyWithoutPlaintext` returns only the wrapped copy. [E, S5]
- Rotation writes a `RotateKey` event to CloudTrail. [E, S3 "Monitoring key rotation"]

### Google Cloud KMS

- Rotation creates a new primary version. It "doesn't re-encrypt your data". [E, S6]
- Decryption uses the right version automatically, if it is still enabled. Encryption uses the new primary. [E, S7]
- After a suspected incident, re-encrypt the data, then disable the old version or schedule it for destruction. [E, S7]
- Destruction is delayed and can be cancelled during that window. After the window, data under that version cannot be decrypted. [E, S7]
- "Make sure that a key version is no longer in use before destroying the key version." [E, S6]
- Envelope guidance: generate DEKs locally, keep them encrypted at rest, store them near the data, never store a plaintext DEK, use AES-256-GCM. KEKs are stored centrally and rotated "regularly, and also after a suspected incident". [E, S9]

### Azure Key Vault

- A rotation policy creates a new key version on a schedule. The rotation time must be at least seven days. [E, S12 "Key rotation policy"]
- Encrypt with the versionless key URI to follow the latest version. Store the versioned URI with the data, so unwrap uses the same material. [E, S12 "Important"]
- Some services take an hour to 24 hours or more to pick up a new version. Check before disabling the old one. [E, S12 Note]

### HashiCorp Vault Transit

- Ciphertext looks like `vault:v1:…`. `v1` is the key version used. The rest is base64 of the IV plus the ciphertext. [E, S10 "Usage"]
- After rotation, new encryptions use the new version. Old data still decrypts from the key ring. [E, S10]
- `rewrap` moves ciphertext to the newest version. It "never returns plaintext", so you can hand it to untrusted scripts. [E, S11 "Rewrap Data"]
- `min_decryption_version` sets the oldest version allowed to decrypt. Older versions get archived. In an emergency you can lower it again. [E, S10 "Working set management"; S11]
- `min_encryption_version` sets the oldest version allowed to encrypt. [E, S11]
- `auto_rotate_period` rotates on a schedule. `0` turns it off. [E, S11]
- `trim` deletes versions below `min_available_version` for good. "Once trimmed, previous versions of the key cannot be recovered." [E, S11 "Trim key"]
- The `datakey` endpoint returns a new DEK. The `plaintext` type returns both copies. The `wrapped` type returns only the wrapped copy. [E, S11 "Generate data key"]
- Transit supports associated data for its AEAD key types (`aes128-gcm96`, `aes256-gcm`, `chacha20-poly1305`). [E, S11]
- Vault says AES-GCM keys should rotate "before approximately 2^32 encryptions have been performed by a key version". [E, S10 "NIST rotation guidance"]
- Convergent (deterministic) encryption is not relevant for secrets. Koinon does not need equality search on secret values. [I]

### Kubernetes encryption at rest

- `providers` is an ordered list. The first provider encrypts. On read, matching providers are tried in order. Within a provider, keys are tried in order for decryption. The first key of the first provider encrypts. [E, S13 "Understanding the encryption at rest configuration"]
- Stored values carry a prefix that names the provider and key, for example `k8s:enc:aescbc:v1:key1`. [E, S13 "Verify that newly written data is encrypted"]
- Zero-downtime rotation with several API servers ("Rotate a decryption key"): [E, S13]
  1. Add the new key as the **second** entry on all control plane nodes.
  2. Restart all API servers, so each can decrypt with the new key.
  3. Back up the new key securely.
  4. Make the new key the **first** entry.
  5. Restart all API servers again.
  6. Rewrite every Secret: `kubectl get secrets --all-namespaces -o json | kubectl replace -f -`. You can run it more than once safely.
  7. Remove the old key only after every Secret is rewritten and the new key is backed up.
- If you lose every copy of a key, you must delete the resources encrypted under it. [E, S13 step 3]
- The local `aesgcm` provider uses a random nonce. It "must be rotated every 200,000 writes" and is not recommended without automated rotation. [E, S13 "Providers" table]
- KMS v2 makes a new DEK per encryption. It derives single-use DEKs from a secret seed with a KDF. The seed changes when the KEK changes. [E, S14]
- KMS v2 tracks a public, non-secret `key_id` for the current KEK. A new `key_id` marks old data stale. Never reuse a `key_id`. [E, S14 "Understanding key_id and Key Rotation"]
- Waiting for natural rewrites is allowed, but a migration should still be scheduled. [E, S14]

### Google Tink

- A keyset is "a non-empty list of keys in which one key is designated primary". The primary key encrypts new plaintexts. [E, S15]
- Keys have unique IDs. Ciphertexts can start with a 5-byte prefix derived from the ID, so decrypt tries only one key. [E, S15]
- The prefix is not authenticated. Treat it as a hint only. [E, S15]
- Rotation runs in three steps: add the new key but not as primary, push the keyset to every binary, then make it primary and push again. [E, S15 "Key rotation"]
- `KmsEnvelopeAead` makes a new DEK per encryption. It wraps the DEK with a KMS KEK and stores it in the ciphertext. [E, S17]
- Tink warns that keys "are often leaked when stored in plaintext". [E, S16]

## 3. Compromise response vs scheduled rotation

### What NIST requires on compromise

- Stop all use of the key to apply protection (encrypt, wrap). Revoke it. [E, S1 §5.5]
- Decrypting with it under controlled conditions may still be allowed, only to remove existing protection. [E, S1 §5.5]
- Disclosure of a confidentiality key means "all information encrypted by that key could be determined by unauthorized entities". [E, S1 §5.5.1]
- Design so that one compromised key exposes as little data as possible. [E, S1 §5.5.2]
- Have a written compromise-recovery plan. It should include re-keying, a key inventory, monitoring that re-keying finished, and a list of exposed information. [E, S1 §5.5.2]

### How the two cases differ

| Step | Scheduled KEK rotation | Suspected KEK compromise |
|---|---|---|
| New KEK version | Yes | Yes, at once |
| Old KEK | Unwrap only, then retire on schedule | Stop wrapping now; unwrap only to migrate; destroy after migration [E, S1 §5.5; S7] |
| Rewrap DEKs | Yes (payload untouched) | Not enough on its own [I] |
| Re-encrypt payloads with new DEKs | No | Yes [E, S7 "re-encrypt the data"; I for "new DEKs"] |
| Rotate the real credentials (provider API keys, GitHub App keys, MCP creds) | No | Yes, if DB rows or backups may also be exposed [I] |
| Old backups | Still need the old KEK | Treat as exposed; the old KEK opens them [I] |

Why rewrap alone fails on compromise. [I] An attacker with the old KEK and any copy of the old rows (a backup, a replica, a dump) can unwrap the old DEKs. Rewrapping only changes the live rows. So the payloads need new DEKs. And because the plaintext credentials may already be known, the only real fix is to rotate them at the provider. AWS says the same thing for data keys: rotating the KMS key "will not mitigate the effect of a compromised data key". [E, S3]

If one DEK or one row leaks (not the KEK): re-encrypt that value under a new DEK and rotate that credential. Other rows are not affected, because each has its own DEK. [I, from S1 §5.5.2 and S9]

## 4. AES-GCM specifics

### Nonces (IVs)

- GCM requires that the same IV and key never repeat across distinct inputs, with probability no greater than 2^-32. [E, S2 §8]
- NIST says this is "almost as important as the secrecy of the key". [E, S2 §8]
- If an IV repeats under one key, an attacker can likely recover the hash subkey and forge ciphertexts. [E, S2 Appendix A]
- NIST recommends supporting only 96-bit IVs. [E, S2 §5.2.1.1]
- Random IVs: the random field must be at least 96 bits from an approved RBG. [E, S2 §8.2.2]
- Use one IV construction per key, not both. [E, S2 §8.2]

### Invocation limit

- With random IVs, a key may run the encrypt function at most 2^32 times in total, across all instances. [E, S2 §8.3]
- Tink gives the same 2^32 message limit for AES-GCM, and about 64 GB per message. Beyond those limits GCM "fails catastrophically". [E, S18]
- Vault says to rotate AES-GCM keys before about 2^32 encryptions. [E, S10]

### Does DEK-per-value remove the limit concern?

- **For DEKs: yes.** Each DEK encrypts one value once. That is one invocation, far below 2^32. [I, from S2 §8.3 and S9]
- **For a local KEK: not fully.** If the KEK wraps DEKs with AES-GCM and random nonces, each wrap is one invocation under the KEK. Every new secret write, every edit and every rewrap counts. [I, from S2 §8.3]
- Koinon's volume is tiny next to 2^32 (about 4.3 billion). Still, a per-KEK wrap counter is cheap and makes the limit visible. [I]
- KMS v2 avoids the problem a different way: a new DEK per encryption from a KDF. [E, S14] AWS KMS also derives a per-call key with a KDF. [E, S19 "Key derivation functions"]
- With Vault Transit or a cloud KMS as the wrapper, the service owns the KEK's GCM limit. [I]

### AAD binding

- AEAD associated data is authenticated, not encrypted. Decrypt succeeds only if the same associated data is passed again. [E, S18 "Associated data"]
- Tink's example binds a record to a `user-id` so it cannot move to another user. [E, S18]
- AWS's encryption context is AAD. It must match exactly on decrypt. It is logged in plaintext, so it must not hold secrets. Store just enough to rebuild it. [E, S20]
- In AWS, moving or renaming bound data makes decryption fail. [E, S20 "Storing encryption context"]
- For Koinon: bind each payload and each wrapped DEK to `(format version, org_id, table, row id, column)`. Then an attacker with DB write access cannot swap one org's ciphertext into another org's row, or one column into another. [I, from S18 and S20]
- Use the same AAD for the DEK wrap. Then a wrapped DEK cannot be moved to another row either. [I]

## 5. Operational patterns for a self-hosted product

### Zero-downtime KEK rotation in a Kubernetes Secret

Model it on the Kubernetes and Tink procedures: add first, promote later, remove last. [E for the pattern, S13 "Rotate a decryption key"; S15 "Key rotation"]

1. **Multi-key Secret.** The Secret holds several KEKs, each under its own ID, plus the ID of the active one. [I]
2. **Add.** Add the new KEK as a non-active entry. Every replica must load it before any replica wraps with it. [E for the order, S13 steps 1-2; S15]
3. **Propagate.** Mounted Secret volumes update on their own, with an eventually-consistent delay (kubelet sync plus cache delay). `subPath` mounts do not update. [E, S21 "Using Secrets as files"] A rolling restart is the simple way to be sure every replica has it. [I]
4. **Promote.** Change the active ID. Restart or reload all replicas again. [E for the order, S13 steps 4-5; S15]
5. **Rewrap.** Run a job that unwraps each DEK with the old KEK and wraps it with the new one. It must be idempotent and safe to re-run, like `kubectl replace`. [E for the pattern, S13 step 6; I for Koinon]
6. **Verify.** Count rows by `kek_id`. Remove the old KEK only when the count for it is zero. [E for the rule, S6 "no longer in use before destroying"; S13 step 7; I for the query]
7. **Retire.** Remove the old KEK from the live Secret. Keep an offline escrow copy for backup restores (next section). [I]

Why add-before-promote matters. [I] If one replica wraps with a new key that another replica has not loaded yet, that other replica fails to read the row. Kubernetes avoids this with two restarts. Tink avoids it by delaying the primary switch. [E, S13; S15]

### Backups

- A key that protects stored keying material "should be backed up or archived for as long as the protected keying material may need to be accessed". [E, S1 Appendix B.3.5]
- A DEK must stay available as long as data under it may need decrypting. [E, S1 Appendix B.3.4]
- Keep backed-up keys for at least as long as the same keys are in normal use. Destroy them when no longer needed. [E, S1 §8.2.2.1]
- For Koinon: a Postgres backup taken before a rotation still names the old `kek_id`. Restoring it needs the old KEK. So keep each retired KEK in escrow until the oldest backup that uses it expires. [I]
- Postgres backups must never hold the KEK. The KEK lives in a Kubernetes Secret, which sits in etcd. [I] Kubernetes Secrets are unencrypted in etcd by default. [E, S21 Caution] So an etcd backup plus a Postgres backup together reveal everything. Tell installers to store them apart, and to turn on encryption at rest for etcd. [I]
- If every copy of a key is lost, the data is gone. Kubernetes and Google both warn about this. [E, S13 step 3; S7]

### Audit

- Log key events: KEK added, promoted, rewrap started and finished (row counts per `kek_id`), KEK retired, compromise declared. Never log plaintext or key bytes. [I; AWS logs rotation and encryption context, E, S3, S20]
- Treat `kek_id` and the AAD fields as non-secret. Kubernetes says `key_id` may be logged and must not hold private data. [E, S14]
- Track access to plaintext keys. [E, S1 §5.5.2 item 6]
- This fits ADR 0023 (observability and audit). [I]

## 6. Recommendation for Koinon v0.1

### Data format

Store one envelope per secret value. Columns or a versioned binary blob both work. [I]

| Field | Meaning | Basis |
|---|---|---|
| `fmt` | Envelope format version (start at 1) | [I]; Vault `vault:`, Kubernetes `k8s:enc:` prefixes [E, S10, S13] |
| `alg` | `A256GCM` for payload and for local wrap | AES-256-GCM [E, S9] |
| `kek_id` | Non-secret KEK ID, e.g. `local:2026-10-23-a`, `transit:koinon:v3` | Key ID stored with data [E, S12, S15, S10]; never reused [E, S14] |
| `wrapped_dek` | DEK wrapped by the KEK; includes its own nonce and tag | [E, S5, S9] |
| `nonce` | 96-bit random nonce for the payload | [E, S2 §5.2.1.1, §8.2.2] |
| `ciphertext` | Payload ciphertext plus 128-bit tag | [E, S2] |
| AAD (not stored) | `koinon/secret/v{fmt}|{org_id}|{table}|{row_id}|{column}`, rebuilt from the row | [E, S18, S20; I for the fields] |

Rules:

- Generate a new 256-bit DEK on every write, including edits. Never reuse a DEK across values or orgs. [E, S9]
- Zero the plaintext DEK after use, as far as Go allows. [E, S5; I for Go]
- Do not trust `kek_id` alone. Authenticate it by putting it in the wrap's AAD, or by failing hard on a mismatch. Tink's prefix is unauthenticated for this reason. [E, S15; I]
- For Vault Transit, `wrapped_dek` is the `vault:vN:` string and `kek_id` records the key name. Use the `datakey` endpoint or Transit `encrypt` on a local DEK. [E, S11; I]

### Rotation commands and flow

Suggested CLI shape. [I]

- `koinon keys status`: list KEK IDs, the active ID, row counts per `kek_id`, wrap counts, and age.
- `koinon keys add`: create a new KEK in the Secret, not active.
- `koinon keys activate <id>`: make it active. Refuse unless every replica reports it loaded.
- `koinon keys rewrap [--from <id>]`: batch, idempotent, resumable. Rewrap DEKs only; the payload stays the same.
- `koinon keys retire <id>`: refuse while any row still uses it. Print a reminder to escrow it for backups.
- `koinon keys reencrypt --org/--all`: compromise only. New DEK and new payload ciphertext per value.

Replicas should report which KEK IDs they hold, for example in a health or status field. Then `activate` can check them, the way Kubernetes needs all API servers restarted first. [I, from S13 steps 2 and 5]

With Vault Transit: `keys add/activate` become Transit `rotate`. `rewrap` calls Transit `rewrap`, which never returns plaintext. `retire` raises `min_decryption_version`, and much later calls `trim`. [E for the Vault calls, S10, S11; I for the mapping]

### Defaults

- **KEK active period: 365 days.** Warn when it is overdue. Let installers set it shorter. [I] Basis: AWS's default is 365 days [E, S3]. NIST allows up to 2 years for a key that wraps relatively few keys [E, S1 §5.3.6]. Azure says at least every 2 years [E, S12]. Koinon wraps few DEKs, but its KEK sits in a Kubernetes Secret, not an HSM. That argues for the shorter end. [I]
- Do not adopt Kubernetes' 90 days or Tink's 30-90 days as the v0.1 default. Those fit managed KMS with automatic rotation. Koinon v0.1 rotation is manual. Very short manual rotations add error risk, which NIST warns about. [E for the warning, S1 §5.3.2; I for the choice]
- **Rewrap right after activation.** Then the old KEK's real recipient period is short, except for backups. [I, from S1 §5.3.6]
- **Old KEK escrow:** keep as long as the backup retention period. [E, S1 B.3.5; I]
- **DEK cryptoperiod:** one value, one write. No scheduled DEK rotation. [E, S9]
- **Helm:** generate the first KEK on install, keep it across upgrades, and let installers name an existing Secret. Never put the KEK in Postgres, values files or logs. [I; fits AGENTS.md "No secrets in sandboxes"]

### On suspected compromise

Follow NIST §5.5 and Google's re-encrypt guide. [E, S1 §5.5; S7]

1. Add and activate a new KEK at once. Stop all wrapping with the old one. [E, S1 §5.5]
2. Run `reencrypt --all`: new DEK and new payload ciphertext for every value. [E, S7; I for new DEKs]
3. Tell org admins to rotate the real credentials at each provider (model keys, GitHub App private keys, MCP credentials). Show the list. [I]
4. Retire and destroy the old KEK, including escrow copies, once the re-encryption is verified. Old backups made under it are then unrestorable for secrets. Weigh this against step 3: once the credentials are rotated, those old secrets are dead anyway. [I; GCP destroy semantics, E, S7]
5. Record the incident in the audit log. Write the recovery plan in the docs ahead of time. [E, S1 §5.5.2]

### Leave for later

- Cloud KMS adapters (AWS, GCP, Azure). The envelope format already fits them via `kek_id`. [I]
- Automatic scheduled rotation, and shorter defaults once rotation is automatic. [I, S6, S14]
- A per-org KEK, for smaller blast radius and crypto-shredding when an org is deleted. NIST favours limiting how much data one key protects. [E, S1 §5.5.2; I]
- Key-committing or nonce-misuse-resistant AEADs (Tink lists AES-GCM-SIV and AES-CTR-HMAC). [E, S18; I for "later"]
- A dedicated key-wrap mode instead of GCM for the local KEK. [I; not covered by these sources]

## Sources

- **S1** NIST SP 800-57 Part 1 Rev. 5, *Recommendation for Key Management: Part 1 – General* (May 2020). §5.3.2, §5.3.3.1, §5.3.6 (items 6, 7, Table 1), §5.5, §5.5.1, §5.5.2, §8.2.2.1, Appendix B.3.4, B.3.5. https://nvlpubs.nist.gov/nistpubs/SpecialPublications/NIST.SP.800-57pt1r5.pdf
- **S2** NIST SP 800-38D, *Recommendation for Block Cipher Modes of Operation: GCM and GMAC* (Nov 2007). §5.2.1.1, §8, §8.2, §8.2.2, §8.3, Appendix A. https://nvlpubs.nist.gov/nistpubs/Legacy/SP/nistspecialpublication800-38d.pdf
- **S3** AWS KMS Developer Guide, "Rotate AWS KMS keys" (sections: Why rotate KMS keys?, How key rotation works, Monitoring key rotation). https://docs.aws.amazon.com/kms/latest/developerguide/rotate-keys.html
- **S4** AWS KMS API Reference, "ReEncrypt". https://docs.aws.amazon.com/kms/latest/APIReference/API_ReEncrypt.html
- **S5** AWS KMS Developer Guide, "Generate data keys". https://docs.aws.amazon.com/kms/latest/developerguide/data-keys.html
- **S6** Google Cloud KMS, "Key rotation". https://docs.cloud.google.com/kms/docs/key-rotation
- **S7** Google Cloud KMS, "Re-encrypting data". https://docs.cloud.google.com/kms/docs/re-encrypt-data
- **S9** Google Cloud KMS, "Envelope encryption" (DEK and KEK best practices). https://docs.cloud.google.com/kms/docs/envelope-encryption
- **S10** HashiCorp Vault, "Transit secrets engine" (Usage, Working set management, NIST rotation guidance, Key types). https://developer.hashicorp.com/vault/docs/secrets/transit
- **S11** HashiCorp Vault API, "Transit secrets engine (API)" (Rewrap data, Generate data key, Update key configuration: `min_decryption_version`, `min_encryption_version`, `auto_rotate_period`; Trim key; `associated_data`). https://developer.hashicorp.com/vault/api-docs/secret/transit
- **S12** Microsoft Learn, "Configure cryptographic key auto-rotation in Azure Key Vault" (Key rotation policy, Important note). https://learn.microsoft.com/en-us/azure/key-vault/keys/how-to-configure-key-rotation
- **S13** Kubernetes, "Encrypting Confidential Data at Rest" (Understanding the encryption at rest configuration, Providers, Verify that newly written data is encrypted, Rotate a decryption key, Ensure all relevant data are encrypted). https://kubernetes.io/docs/tasks/administer-cluster/encrypt-data/
- **S14** Kubernetes, "Using a KMS provider for data encryption" (KMS v2, Understanding key_id and Key Rotation). https://kubernetes.io/docs/tasks/administer-cluster/kms-provider/
- **S15** Google Tink, "Keysets" design doc (primary key, key IDs, output prefix, key rotation). https://developers.google.com/tink/design/keysets
- **S16** Google Tink, "Key management overview". https://developers.google.com/tink/key-management-overview
- **S17** Google Tink, "Client-side encryption" (KMS envelope AEAD). https://developers.google.com/tink/client-side-encryption
- **S18** Google Tink, "AEAD" (associated data, key types, AES-GCM limits). https://developers.google.com/tink/aead
- **S19** AWS KMS Developer Guide, "AWS KMS cryptography essentials" (Key derivation functions, Envelope encryption). https://docs.aws.amazon.com/kms/latest/developerguide/kms-cryptography.html
- **S20** AWS KMS Developer Guide, "Encryption context" (rules, logging, storing). https://docs.aws.amazon.com/kms/latest/developerguide/encrypt_context.html
- **S21** Kubernetes, "Secrets" (Caution on etcd; Using Secrets as files from a Pod: mounted Secrets are updated automatically; subPath). https://kubernetes.io/docs/concepts/configuration/secret/

Notes on method: NIST PDFs were downloaded and read as extracted text. Web pages were read through a fetch tool that summarises; the quoted phrases above were returned as verbatim quotes. The Kubernetes Secret-volume and Vault "NIST rotation guidance" text came from that tool, not from a direct read of the HTML.
