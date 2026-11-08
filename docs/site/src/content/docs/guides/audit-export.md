---
title: Export and verify the audit log
description: The JSON lines export format of the audit chains, the hash encoding, and how to check a copy off the cluster.
---

The install log and each org's audit log are hash chains (see [the audit log](/guides/audit/)). You can
export a chain as JSON lines and check the copy anywhere, with no database: with `koinon-api audit verify
--file`, or with any tool that implements the hash described below. Nothing is deleted by an export.

## Export

```sh
# from the cluster
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit export --scope install > install.jsonl
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit export --scope org --org <org-id> > org.jsonl
# only the events from chain_seq 120 on
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit export --scope install --from-seq 120 > install-120.jsonl

# or over the admin API, streamed
curl -H "Authorization: Bearer $TOKEN" "https://koinon.example.com/admin/v1/audit/export" > org.jsonl
curl -H "Authorization: Bearer $TOKEN" "https://koinon.example.com/admin/v1/audit/export?scope=install" > install.jsonl
```

The admin API follows the rules of the read. `scope=org` (the default) exports the token's own org;
naming another org with `org=` is 403. `scope=install` needs a token whose root user is a platform
admin, checked on every request. The export reads the chain page by page and writes each line as it
goes, so a long chain is not held in memory.

## Format `koinon.audit.export.v1`

The first line is the header, then one line per event in `chain_seq` order. Every line is one JSON object.

```json
{"format":"koinon.audit.export.v1","chain":"install","install_id":"0192...","from_seq":1,"head":{"chain_seq":2,"hash":"9f2c...64 hex digits"}}
{"chain_seq":1,"id":"0192...","actor_kind":"system","actor_id":"koinon-api:bootstrap","action":"platform_admin.bootstrap","target_kind":"user","target_id":"0192...","summary_text":"{\"after\": {}}","summary":{"after":{}},"request_id":"","created_at_micros":1788000000000000,"prev_hash":"0000...","hash":"ab12..."}
```

| Header field | Meaning |
| --- | --- |
| `format` | Always `koinon.audit.export.v1`. |
| `chain` | `install` or `org`. |
| `install_id` | The install's ID (lower-case UUID text). |
| `org_id` | The org's ID, for `chain: org` only. |
| `from_seq` | `chain_seq` of the first event line: 1 for the whole chain. |
| `head` | `chain_seq` and `hash` of the chain's last event when the export started. Events appended later are not in the file. |

An event line has `chain_seq`, `id`, `actor_kind`, `actor_id`, `action`, `target_kind`, `target_id`,
`request_id`, `created_at_micros` (microseconds since the Unix epoch), `prev_hash` and `hash` (64
lower-case hex digits), and the summary twice: `summary_text` is the text Postgres prints for the `jsonb`
value, which is what the hash covers, and `summary` is the same value as a JSON object for readers. A
verifier must hash `summary_text` and refuse a line where `summary` is another value. The summaries are
the scrubbed audit data; an export holds no key material and no secret values.

## The hash

`hash = sha256(canonical_row || prev_hash)`. `canonical_row` is a list of fields, each written as its UTF-8
byte length as an 8-byte big-endian integer followed by its bytes. `prev_hash` follows as its 32 raw bytes
(32 zero bytes for `chain_seq` 1). The fields, in order:

1. the tag: `koinon.install_audit.v1` for the install chain, `koinon.audit.v1` for an org chain;
2. the scope ID: the header's `install_id` (install chain) or `org_id` (org chain), as lower-case UUID text;
3. `id`;
4. `chain_seq` in decimal;
5. `actor_kind`, `actor_id`, `action`, `target_kind`, `target_id`;
6. `summary_text`;
7. `request_id`;
8. `created_at_micros` in decimal.

In Python:

```python
import hashlib, struct

def field(s): b = s.encode(); return struct.pack(">Q", len(b)) + b

def event_hash(tag, scope, e):
    row = b"".join(field(x) for x in [tag, scope, e["id"], str(e["chain_seq"]), e["actor_kind"], e["actor_id"],
        e["action"], e["target_kind"], e["target_id"], e["summary_text"], e["request_id"], str(e["created_at_micros"])])
    return hashlib.sha256(row + bytes.fromhex(e["prev_hash"])).hexdigest()
```

A chain holds when, for every line: `chain_seq` is one more than the line before (the first is `from_seq`),
`prev_hash` is the hash of the line before (zeros for `chain_seq` 1), `hash` is the recomputed hash, and the
last line's `chain_seq` and `hash` are the header's `head`. A line removed from the end shows against the head.

## Verify off the cluster

```sh
koinon-api audit verify --file install.jsonl
```

The binary runs anywhere and needs no database. It prints `ok` or `BROKEN` with the first `chain_seq` that
does not fit, and exits non-zero when the file does not hold. A changed field, a removed or swapped line and
a truncated tail each fail with that `chain_seq`. The file is read one line at a time.

A partial export (`--from-seq`) has no line to take its first `prev_hash` from. Check it against an earlier
export that ends at the line before:

```sh
koinon-api audit verify --file install-120.jsonl --after install.jsonl
```

`--after` verifies the earlier file too, then requires its last `hash` to be the partial file's first
`prev_hash`. Without `--after` the partial file's own links are checked and the report says the first
`prev_hash` was not.

An export proves that the file is a consistent chain with the head it names. It does not prove that the
head is the chain's real head: someone who rewrites a whole copy from the start, header included, gets a
file that verifies. Compare the head with one you kept earlier (an earlier export given to `--after`). Anchors
and signed checkpoints that pin the head outside the database are planned (ADR 0100); `verify --file` will
check them when they exist.
