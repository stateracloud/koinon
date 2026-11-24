---
status: proposed
date: 2026-11-24
area: security-audit
tags: [mcp, leak-guard, security, audit]
relates: ["0023", "0057", "0091", "0097"]
---
# 0125. Hide MCP tools that no admin reviewed

- Status: Proposed (needs main: ADR number)
- Date: 2026-11-24
- Area: security-audit
- Tags: mcp, leak-guard, security, audit
- Relates to: [0023](0023-observability-and-audit.md), [0057](0057-mcp-servers.md), [0091](0091-leak-guard-by-destination.md), [0097](0097-koinon-owned-gateway.md) (step 2, the tool pin; needs main: make this "Amends" once PR 1228 has merged)
- Issue: #1216

## Context

The text of an MCP tool (its name, description and schemas) reaches the model as instructions. ADR 0097
step 2 pins that text (#1132). The gateway serves the text that an admin approved. It leaves out a tool
whose text changed since the approval, and it refuses a call to that tool.

ADR 0097 served a tool that no admin approved yet as sanitised text (ADR 0057). Thus a server could add
a new tool, and the model saw its text before a person read it. Sanitising removes markup and control
characters. It does not remove an instruction that is written in plain words. So the sanitiser is not
a protection against a hostile tool text. It only makes the text smaller and cleaner.

Also, the sync stored one row for each name that a server listed. A server that lists new names on each
probe could add rows and audit events without limit.

Other products do this:

- LiteLLM (pinned mode), TrueFoundry and Cloudflare (`default_disabled`) hide a new tool until an admin
  turns it on.
- Cursor and Windsurf ask the user on each call.
- No product that we know uses sanitised text as its protection.

The maintainer decided on 2026-11-24 to hide a tool until an admin reviews it.

## Decision

1. **A tool that no admin reviewed is hidden.** "Reviewed" means that the tool row has an approved hash
   (`mcp_tools.approved_hash`). A tier change or an acknowledgement sets it. A hidden tool is not in
   `tools/list`. A call to it is refused with fixed text: "no admin reviewed this tool yet; it is not
   available until an admin reviews it". The target does not see the call. This applies in the gateway
   guard (`mcpguard.CheckToolList`) and in the ask-tier proxy (`mcpask`). The ask proxy does not record
   an approval request for a hidden tool: no agent waits on a person during a run.
2. **The guard still wakes the prober** for a hidden tool whose text the sync did not record (#1173).
   The tool thus reaches the review list in seconds.
3. **The sanitiser stays as a second layer.** The text that the sync stores, and the text that an admin
   approves, is still the sanitised text.
4. **A switch for each server: "show new tools before review".** The column
   `mcp_servers.show_unreviewed_tools` is false by default. When an admin turns it on for a server, a
   tool of that server that no admin reviewed is served as sanitised text, as before. It sets the
   untrusted-input leg when the server declares that leg (ADR 0091). A changed tool is still left out:
   the switch applies only to a tool that no admin reviewed. The API field, the declarative field and the
   console switch come in part 2 of #1216.
5. **A limit on unreviewed rows.** The sync stores at most 256 tool rows with no approved hash for one
   server (`mcpservers.MaxUnreviewedTools`). Absent rows count too, so a server cannot make room by
   dropping names. At the limit the sync stores no new row. It sets
   `mcp_servers.unreviewed_tools_capped_at` and writes one audit event,
   `mcp_server.unreviewed_tools_capped`. The event has the server name and the limit, and no tool name.
   Later syncs at the limit write no event. When the server is below the limit again, the sync clears
   the mark without an event. A later cap writes a new event.
6. **The sync locks the server row** (`SELECT ... FOR NO KEY UPDATE`). Two syncs of one server thus
   cannot both count below the limit and both add rows.
7. **Metrics.** `koinon_gateway_mcp_tools_left_out_total` gets the reason `unreviewed`. The labels stay
   a fixed set (ADR 0023). The gateway writes no log line for each hidden tool, because a server can list
   many names and a name is the server's own text.

## Alternatives considered

- **Keep serving sanitised text.** Rejected. The sanitiser cannot find an instruction in plain words.
- **Ask a person on each call (Cursor, Windsurf).** Rejected. An agent run has no person present, and the
  maintainer decided that no agent waits on a person during a run.
- **Prune old unreviewed rows to stay under the limit.** Rejected. A server that lists new names on each
  probe would then cause one prune and one audit event per sync.
- **Remove the tool from the gateway plan (no grant) instead of hiding it in the guard.**
  _(reconstructed)_ Rejected for now. The plan is built by the reconciler, and a change waits for the
  next reconcile pass. The guard sees each list, so it hides a tool at once.

## Consequences

- Positive: a new tool's text reaches the model only after a person reads it, or after an admin chose to
  trust the server.
- Positive: a hostile server cannot fill the database or the audit log with new names.
- Negative: an admin must review each new tool before an agent can use it. Part 2 of #1216 adds an
  approve-all action and a "tools waiting" notice to make this light.
- Negative: rows of tools that a server no longer lists can fill the limit. There is no admin action to
  delete them yet (a follow-up).
- Follow-ups: part 2 of #1216 (the API, declarative and console switch, approve-all and the notice). The
  ask proxy serves the current text of an approved ask-tier tool that has a pending change; it does not
  apply the pin (a follow-up). When PR 1228 has merged, main changes "Relates to" 0097 to "Amends" and adds
  0125 to the "Amended by" line of ADR 0097. This PR does not edit ADR 0097, because PR 1228 edits it.

## Links

- Issue #1216 and the maintainer decision on it (2026-11-24).
- ADR 0097 step 2 and #1132 (the tool pin), #1173 (the prober wake).
