---
status: proposed
date: 2026-11-19
area: work-model
tags: [api, auth, tasks]
relates: ["0011", "0065", "0066", "0101", "0120"]
---
# 0123. External A2A edge

- Status: Proposed
- Date: 2026-11-19
- Area: work-model
- Tags: api, auth, tasks
- Relates to: [0011](0011-agent-to-agent.md), [0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md), [0101](0101-agent-substrate-and-kagent-not-adopted.md), [0120](0120-execution-hardening-defaults.md)
- Issue: [#1129](https://github.com/stateracloud/koinon/issues/1129)
- Verification: Pending: a kind test with a kagent agent in each direction (issue 1129). The inbound
  half is covered by specs with a fake backend. The outbound client is covered by specs with a fake
  external agent (v1.0 and v0.3).

## Context

[0011](0011-agent-to-agent.md) says A2A is the protocol at the edge. Nothing implements it yet.
kagent agents speak A2A. A Koinon agent must be able to take a task from such an agent, and (second
slice) send a task to one.

Koinon already has a work model ([0065](0065-work-model-tasks-inbox.md)): a task has one owning
agent, a state, and a timeline of events. A message to an agent starts a direct-message thread
([0066](0066-triggers-and-scheduling.md)). The task states are close to the A2A states.

## Decision

1. **Inbound edge.** Koinon serves A2A on `/a2a/v1/agents/{agent}/`. It serves the v1.0 JSON-RPC
   binding at `jsonrpc` and the v0.3 binding at `v0.3/jsonrpc`. It serves one agent card per agent at
   `.well-known/agent-card.json`. The edge implements `a2asrv.RequestHandler` of a2a-go directly over
   Koinon tasks. It has no second task store and no executor.
2. **A thread is a context. A turn is a task.** The edge follows A2A 1.0.0: a task that has ended
   (completed, failed, canceled, rejected) never takes a new message, and a follow-up turn is a new
   task in the same `contextId` (sections 3.1.1, 3.4.3). A Koinon direct-message thread is one
   `contextId`: the UUID of the Koinon task. Each turn of the thread is one A2A task. The task ID is
   `<thread UUID>.<turn>`, with the turn counted from 1, and a task ID is never reused. A turn
   starts with the first message of a person, and with each message of a person that reopens a
   completed thread. A message that answers a question is part of the open turn. Every turn before
   the last has ended as completed, because only a completed thread takes a follow-up.
   The new task is linked to the one before it: the first message of the turn has
   `referenceTaskIds` with the earlier task. `SendMessage` has three forms:
   - No task ID and no context ID: a new thread, turn 1.
   - A context ID and no task ID: the next turn in that thread. If the last turn is still open,
     the message is part of it (it is the same thread, and Koinon queues the message), and the
     answer holds the open task. A thread that failed or was canceled takes no message.
   - A task ID: the answer to that task. Only the last turn can take it, and only while it is open.
     A turn that ended gives `UnsupportedOperationError`. A task ID that is not a turn of a thread
     of the agent gives `TaskNotFoundError`. A `contextId` that is not the thread of the task gives
     `InvalidParamsError`.

   The message goes through the same path as a person who writes to the agent: the checks of the
   message, the org queue when durable work is on, and the same agent and trigger rules. The
   A2A message ID is the idempotency key. A retry of a message that was taken returns the task, also
   when the task has ended since. An agent takes A2A messages only if it has a `direct_message` trigger.
   The edge shows no other task of the agent.

   A retry of a message that was taken returns the task that holds the message, which can be an
   earlier turn. A message to a task is checked against its turn twice: by the edge, and again
   under the lock of the thread (the delivery refuses it when a follow-up reopened the thread after
   the turn started). With durable work on, the workflow keeps the result under the message ID,
   a refusal too. A client that is refused sends the next message with a new message ID.
3. **State mapping.**

   | Koinon | A2A |
   | --- | --- |
   | submitted | `TASK_STATE_SUBMITTED` |
   | working | `TASK_STATE_WORKING` |
   | input_required | `TASK_STATE_INPUT_REQUIRED` |
   | completed | `TASK_STATE_COMPLETED` |
   | canceled | `TASK_STATE_CANCELED` |
   | failed | `TASK_STATE_FAILED` |

   The state of the last turn is the Koinon state. An earlier turn is `completed`. Messages of people are user messages.
   Messages and questions of the agent are agent messages. When the task needs input, the status
   message holds the question. A completed task has one artifact, `reply`, with the last reply of
   the agent, for the turn of the task. A message to a task that has completed is refused inside the
   lock of the task, so a race with the end of the turn cannot reopen it. Only a message with the
   context ID reopens the thread.
4. **Blocking send.** `SendMessage` waits until the task is `input-required` or has ended, up to 30
   seconds, then returns the task as it stands. `returnImmediately` returns at once. The client
   then reads the task with `GetTask`. The wait reads only the events after the message. A run that
   parks writes `run.parked` and no status event, so the wait takes `run.parked` or
   `question.asked` after the message as the proof of `input-required`, not the state alone (the
   state can be left over from before a reply). At most 64 waits run at one time; more return at
   once. An error that is not for the client returns as an A2A internal error and is logged.
5. **Auth.** The edge uses org API tokens behind the same gate as the admin API (token check,
   failure rate limit, audit event per call). The token needs the member role. Messages are written
   as the user that created the token. The agent card names an HTTP bearer scheme. The edge adds no
   new key, credential or table.
6. **Not served in this slice.** Streaming, task list, push notifications and the extended card
   return the A2A errors for an unsupported operation. The card says so in its capabilities.
7. **Outbound edge.** A Koinon agent calls an external A2A agent through the gateway, never from
   the sandbox. The client is package `internal/a2aout`. It follows
   [0120](0120-execution-hardening-defaults.md) and the other outbound clients:
   - It dials through the platform URL policy (`URLPolicy.DialContext`), checked at dial time, with
     no proxy and no redirect (issue 657). A name that passes the check and then resolves to a
     denied address is refused at the dial.
   - The credential is the one of the MCP target (`CredentialHeader`, `CredentialPrefix`,
     `CredentialFile`). The gateway reads it on each request and sets it. It goes to the host of the
     target only and only over https. The sandbox gets no credential: no env, image, home volume or
     log holds it, and no error names it. The CA bundle and `insecureSkipVerify` of the target apply
     to this client only. Both the MCP path and this client build the HTTP client with
     `mcpgateway.NewTargetHTTPClient`, so one rule covers both.
   - The agent card is read at `/.well-known/agent-card.json` under the target URL (a target with a
     path works). It must offer a JSON-RPC interface with the scheme and host of the target. A card that points
     to another host is refused: it cannot move the credential or the traffic. The client reads a
     v1.0 card and a v0.3 card, and speaks both bindings (kagent serves v0.3).
   - A response body has a size limit, and the text of a question or reply is cut at 64 KiB. It is
     another party's text: quoted data.
8. **Outbound turns and states.** The client follows the same A2A 1.0.0 rules as decision 2:
   - A task that has ended (completed, failed, canceled, rejected) never gets a message. The client
     reads the task first and refuses (`ErrTaskEnded`). A follow-up turn is a new task in the same
     `contextId`, with `referenceTaskIds` to the task before it, and no task ID.
   - An answer goes to a task that is `input-required` only, in the context of that task. A task
     in `auth-required` takes no answer from the agent, and its text is not shown as a question.
   - A send returns at once with the task by default. A blocking send is bounded by the request
     timeout, and a task that runs longer would be lost to the caller, so the caller waits with
     `GetTask`.
   - A task ID is never reused. An agent that answers a follow-up with the task it follows, or
     that answers in another context or for another task, is refused.
   - The client does not subscribe. `SubscribeToTask` on a task that ended is an error in A2A
     1.0.0, and a read of the task has no such race. The client waits with `GetTask`.
   - The external state maps back to Koinon:

     | A2A | Koinon |
     | --- | --- |
     | `TASK_STATE_SUBMITTED` | submitted |
     | `TASK_STATE_WORKING` | working |
     | `TASK_STATE_INPUT_REQUIRED` | input_required, with the question of the status message |
     | `TASK_STATE_AUTH_REQUIRED` | input_required, reason `auth_required` |
     | `TASK_STATE_COMPLETED` | completed |
     | `TASK_STATE_CANCELED` | canceled |
     | `TASK_STATE_FAILED` | failed |
     | `TASK_STATE_REJECTED` | failed, reason `rejected` |

     A state that is not in this table is an error. The client never guesses. `auth-required`
     waits for a person: an admin adds the credential to the remote agent, because the sandbox never
     supplies one. A message with no task is `completed`, with its text as the reply.
9. **How an agent reaches the client: remote agents.** An external A2A agent is its own resource
   of the org, a *remote agent*. It is not an MCP server and not a value of the `mcp_transport`
   enum, which this ADR does not change. The table `remote_agents` is keyed by `(org_id, id)`. Its
   fields:
   - `name`: a DNS label of at most 24 characters, unique in the org.
   - `card_url`: the base URL of the external agent. The card is at `/.well-known/agent-card.json`
     under it. The platform URL policy applies at registration, in the reconciler and at each dial.
   - The credential: `credential_header`, `credential_prefix` and `credential`. They have the same
     meaning, the same sealed envelope and the same code (`internal/mcpservers`) as in `mcp_servers`.
     The API never returns them. A credential needs an https URL (decision 7).
   - The TLS trust: `ca_bundle` and `insecure_skip_verify`, with the same checks as in `mcp_servers`.
   - The card pin: `card_hash`, `card` (the pinned card, at most 256 KiB), `observed_card_hash`,
     `observed_card` and `card_checked_at`. See decision 11.
   - The health: `health` (unknown, healthy, unhealthy), `health_reason`, `health_checked_at`,
     `health_since` and `consecutive_failures`.
   - `disabled_reason`, `created_by` and the timestamps.

   The table `agent_remote_agents` `(org_id, agent_id, remote_agent_id)` holds the grants. It has
   composite foreign keys to `agents` and `remote_agents`. An agent with no row calls no remote
   agent. A change of `remote_agents` or `agent_remote_agents` sends a NOTIFY to the reconciler, as
   the MCP tables do (migrations 00059 and 00061).
10. **The built-in gateway target and its three tools.** The gateway serves one built-in target per
    org, `<org slug>-agents`, besides the targets of the MCP servers. The target has no upstream
    URL. The gateway runs its tools in its own process, with `internal/a2aout`. The tools are fixed
    in code. No card changes them.

    | Tool | Arguments | Effect |
    | --- | --- | --- |
    | `send_task` | `agent`, `message`, optional `context_id`, optional `task_id` | Sends one message. Returns the state and the reply text. |
    | `get_task` | `agent`, `task_id` | Reads the task. Returns the same. |
    | `cancel_task` | `agent`, `task_id` | Cancels the task. Returns the same. |

    `agent` is the `name` of a remote agent. It is an argument and not part of the tool name, so a
    new remote agent adds no tool and does not change `tools/list`. The state names are the Koinon
    names of decision 8. A task that needs input returns `input_required` with the question. The
    agent answers with `send_task` and the same `task_id`.

    The gateway refuses a call in each of these cases. The agent has no grant for the remote agent.
    The remote agent is disabled. The remote agent is unhealthy. The remote agent is in another
    org. The refusal text is fixed. It names no URL and no card text.

    **Where it runs.** Only `internal/mcpgateway` runs the built-in target. The charts default to
    the `agentgateway` MCP driver, and the koinon gateway does not become the default before #1132
    closes (decision 11). So the feature works when `mcp.driver` is `koinon`. With another driver
    the reconciler does not serve the built-in target and reports why. No default install has the
    tools until #1132 closes.

    **How the gateway gets its data.** The gateway reads no database. The reconciler pushes it in
    `gateway.MCPConfig`. `MCPTarget` gets the field `Builtin` (the value `a2a`). `MCPState.validate`
    accepts an empty URL for a target with `Builtin` set, and for no other target. A target has a
    list of `MCPRemoteAgent` entries: the name, the ID, the card URL, the credential file, the TLS
    trust, the pinned `card_hash`, and the IDs of the agents that have a grant. The reconciler
    leaves out a remote agent that is disabled or unhealthy. A driver that cannot serve a
    `Builtin` target refuses it with an error, so one config never fails for all orgs.

    **Rows.** The `mcp_tools` rows need a parent `mcp_servers` row. The org gets one built-in row,
    with the name `agents`, created with the first remote agent. The migration adds the column
    `builtin text` (the value `a2a`). The URL of a built-in row is the fixed text
    `koinon-builtin://a2a`. The migration widens the URL check to accept that text and nothing
    else, and adds a check that `builtin` and this URL go together. The columns `url` and
    `transport` stay `NOT NULL`. An older binary reads the row without error. Its URL policy
    refuses the URL, so it reports the row as not served and goes on with the other servers.

    Code that writes `mcp_servers` must skip a built-in row. `UpdateMCPServer`, `DeleteMCPServer`
    and the class calls refuse it. `ShadowUIMCPServers` does not disable it. The MCP server
    service refuses the name `agents`, and the registration of the first remote agent fails with
    a clear message when an MCP server of the org already has this name. The prober does not dial a
    built-in row. It reads the tool list from the gateway as for any target. The fixed tool list
    gives one `mcp_tools` row for each tool, with a `content_hash` of the fixed description and
    schema.

    **Tiers, guard and audit.** Each tool starts in the `ask` tier, as every new MCP tool does. An
    admin sets `auto` or `never` with the existing call. `agent_mcp_servers` grants the target to
    an agent, and `agent_remote_agents` grants each remote agent. The gateway checks both. The ask
    tier uses the approval proxy of ADR 0057. The binding of an approved call (`call_binding`)
    holds the ID of the remote agent, the hash of its `card_url` and its `card_hash`, besides the
    tool hash. A change of the URL or an accepted card makes the call park again. The proxy checks
    `agent_remote_agents` before it asks a person. Each call passes `checkCall`, the span and the
    audit row of any other call. The audit row names the target, the tool and the kind of
    credential. It never holds the message, the reply, the card or a token. The metric labels are
    fixed (ADR 0023). The `send_task` tool has an external sink class, with `agent` as the address
    argument, so the leak guard of ADR 0084 sees the message as data that leaves Koinon.

    **Credential.** The gateway reads the credential of the remote agent on each request from a
    file, as for an MCP server. The file name holds the ID of the remote agent and the hash of its
    envelope. `a2aout` gets its HTTP client from `mcpgateway.NewTargetHTTPClient`, as decision 7
    says. The gateway maps each `a2aout` error to a fixed text before the model sees it, because
    a `url.Error` can name the full URL.

    **Task handles.** All agents with a grant share the credential of one remote agent, and the
    remote agent cannot tell them apart. So the gateway gives the model a handle in place of the
    task ID: `<task ID>.<tag>`. The tag is an HMAC-SHA256 over the org, the calling agent, the
    remote agent and the task ID. The key is a gateway secret that the reconciler renders, so all
    replicas share it. `get_task`, `cancel_task` and an answer in `send_task` refuse a handle with
    a wrong tag. No agent can read or cancel the task of another agent.
11. **The card is for admins only, and it is pinned.** The text of the agent card (name,
    description, skills, examples) is data from another party. It never reaches a model: not in a
    tool description, a tool result, an error or a log. The tools of decision 10 do not change with
    the card. An admin reads the stored card in the API, and the UI shows it as quoted text.
    - **Pin.** `card_hash` is the SHA-256 (hex) of the canonical JSON of the card (sorted keys, no
      insignificant white space). Registration reads the card, applies the checks of decision 7 and
      stores the card and its hash. It fails if the card cannot be read.
    - **The gateway checks the pin at the point of use.** `a2aout.New` takes the expected
      `card_hash` and refuses a card with another hash. The gateway builds a client for each call
      or session, so a changed card stops the next call at once. The check does not wait for the
      prober. The gateway writes the audit outcome `card_changed`.
    - **Health check.** The reconciler prober reads the card of each enabled remote agent every 60
      seconds. It opens the credential as the reconciler does to render the file. A card with
      another hash is a finding: `health` is `unhealthy` with the reason `card_changed`, at the
      first difference and without the three-failure rule of the MCP prober. The finding does not
      clear itself. The prober stores the new card in `observed_card` and its hash in
      `observed_card_hash`. It never changes `card` or `card_hash`. An unreachable card is another
      finding that follows the MCP rule: it needs three failures, and one success clears it.
    - **Accept.** `AcceptRemoteAgentCard` takes the `observed_card_hash` that the admin read. It
      fails with `ABORTED` and changes nothing if the stored `observed_card_hash` is different now
      (the rule of `SetToolSink`). Otherwise it copies `observed_card` to `card` and the hash to
      `card_hash`, and clears the finding. A server that shows a different card to each caller can
      still fail the check at the gateway. It cannot make an admin accept a card that the gateway
      did not see.
    - **Not in this decision.** A tool for each skill of a card needs a pin of `tools/list`
      ([#1132](https://github.com/stateracloud/koinon/issues/1132)) first. The koinon gateway MCP
      path does not become the default before that issue closes. This ADR does not touch
      `tools/list` handling.
12. **A reply is an untrusted tool result.** The text of a remote agent (a reply or a question) is
    the result of the tool call. It has the same leg handling as the result of any other tool.
    The guard's untrusted-input leg applies to every tool of the built-in target, so a run that
    reads a reply is tainted, as after any other untrusted result. The text is cut at 64 KiB
    (decision 7). A remote agent can never change a tier or a grant.
13. **Names of the interfaces.** The protobuf package is `koinon.remoteagent.v1alpha1`, with the
    service `RemoteAgentService`. Its calls are `ListRemoteAgents`, `CreateRemoteAgent`,
    `UpdateRemoteAgent`, `DeleteRemoteAgent`, `GetRemoteAgentCard`, `AcceptRemoteAgentCard`,
    `GetAgentRemoteAgents` and `SetAgentRemoteAgents`. The admin API has `/remote-agents`,
    `/remote-agents/{remoteAgentId}`, `/remote-agents/{remoteAgentId}/card`,
    `/remote-agents/{remoteAgentId}/card/accept` and `/agents/{agentId}/remote-agents`. The roles
    are the roles of the MCP server calls: admin and owner. Each write has an audit event
    (`remote_agent.created`, `.updated`, `.deleted`, `.card.accepted` and
    `agent.remote_agents.set`). Proto fields are only added. The migration only adds tables and
    columns and widens one check, so it follows the expand-only rule of ADR 0085.

## Alternatives considered

- **The SDK executor and task store.** It keeps a second copy of every task and needs a bridge to
  keep both copies in step. Rejected.
- **A new token type for A2A.** More key infrastructure for no gain over org tokens. Rejected.
- **Streaming first.** The blocking send covers a first interop. Streaming needs a way to follow
  the timeline. Left for later.

## Consequences

- Positive: a kagent agent can send a task to a Koinon agent with a plain A2A client. The edge is
  small, because the work model already has the states.
- Negative: no streaming, so a long task needs polling with `GetTask`.
- Negative: a thread longer than 10,000 events shows its first 10,000 only in `GetTask`. _(inferred)_
- Negative: the audit event of a call has the HTTP status, so a JSON-RPC error shows as 200.
  An approval that parks a run shows `input-required` with a note and no question: the client
  cannot decide it. _(inferred)_
- Negative: a reply to an `input-required` task is a new message in the thread. It is not checked
  that the message answers the open question of the agent. _(inferred)_
- Negative: the turns are read from the timeline: no new column or table. A thread whose timeline is
  over 10,000 events has a wrong turn count in `GetTask`, as above.
- Negative: a thread has one open turn at a time, so a client cannot run two tasks in one context
  at once. It starts a new thread (a new context) for work in parallel.
- Negative: `CancelTask` checks the turn in the edge only. A cancel can reach a turn that opened
  between the check and the cancel. The window is one request. _(inferred)_
- Negative: a Koinon agent sees no skill of an external agent. It sends free text to a remote agent
  that it chose by name. The admin reads the card. A tool for each skill comes later, behind the
  pin of #1132 (decision 11).
- Negative: any change of a card, also a harmless one, stops the calls until an admin accepts it.
  _(inferred)_
- Negative: a built-in row in `mcp_servers` means that code which writes servers must skip it
  (decision 10). _(inferred)_
- Negative: the tools work only with `mcp.driver: koinon`, so no default install has them until
  #1132 closes (decision 10).
- Negative: the gateway builds a card fetch for each client, so a call to a new session costs one
  more request. _(inferred)_
- Negative: the client polls. A long task costs one `GetTask` per interval.
- Negative: an `auth-required` task has no automatic way out: a person adds the credential and
  starts a new turn. _(inferred)_
- Follow-ups: tools from the skills of a card, behind #1132; pin the kagent version used by the e2e ([0101](0101-agent-substrate-and-kagent-not-adopted.md)).

## Open questions

- Does a reply in the thread answer an open `ask_user` question without a separate step? To check
  on kind.

## Upstream verification

a2a-go v2.8.0 serves v1.0 and, through `a2acompat/a2av0`, v0.3. Checked on 2026-11-19 in the module
source. kagent was not checked: the VM has no access to its registry.
