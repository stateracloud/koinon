---
status: accepted
date: 2026-11-26
area: work-model
tags: [tasks, chat, identity, database, api]
amends: ["0127"]
relates: ["0006", "0023", "0065", "0066", "0127"]
---
# 0130. The external actor kind: people without a Koinon account on the task timeline

- Status: Accepted (issue #1408; the maintainer accepted it on 2026-11-26)
- Date: 2026-11-26
- Area: work-model
- Tags: tasks, chat, identity, database, api
- Amends: [0127](0127-chat-adapter-pure-core.md) (decision 2, "Actors on the timeline": where the identity of an
  unlinked person is kept)
- Relates to: [0006](0006-identity-layers.md), [0023](0023-observability-and-audit.md),
  [0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md),
  [0127](0127-chat-adapter-pure-core.md)

## Context

A person can write to the Koinon bot in Slack or Teams ([0127](0127-chat-adapter-pure-core.md)). Some of these
people have no Koinon account, or they did not link their account. A channel event trigger can accept their messages
([0066](0066-triggers-and-scheduling.md) decision 3).

The task timeline (`task_events`) names who wrote each event with `task_actor_kind`. The enum has three values:
`user` (a Koinon user), `agent` and `system`. A database check requires that a `user` event names a Koinon user
(migration 00003, line 200). Thus an unlinked person has no actor kind. The only choices today are `system`, which
says that Koinon wrote the message, or no event at all. [0127](0127-chat-adapter-pure-core.md) question 6 decided a
new actor kind `external` and gave it its own decision, because the change touches the enum and every reader of the
timeline. Until this decision lands, the chat core returns `Drop(unlinked_actor)` for such a message.

The readers of the timeline are: the API (`TaskEvent` in `koinon.task.v1alpha1`), the web timeline, the catalog of
typed timeline events (`internal/tasks`, issue #735), the prompt that agentd builds from the task's messages, and the
A2A edge ([0123](0123-a2a-external-edge.md)), which shows a task's history to a calling agent.

The maintainer decided on 2026-11-26 (issue #1408):

- An `external` actor can start a task (by an @mention or a direct message) and reply in the thread of that task.
- An `external` actor cannot approve tool calls and cannot answer "blocked on you" questions. Org members do that in
  the Koinon inbox.
- The identity is the platform, the workspace or team, and the platform user ID.

## Decision

### 1. The actor kind `external`

`task_actor_kind` gets the value `external`. An `external` event is a message from a person who writes from the chat
platform workspace or tenant of the org's connection and who has no linked Koinon account. People from other
workspaces (Slack Connect) and guest accounts are not `external` actors: the chat core still drops their messages
with `Drop(external_user)` ([0127](0127-chat-adapter-pure-core.md) question 10). The drop reason and the actor kind
are two different things.

A person who links a Koinon account later writes `user` events from then on. Their earlier events stay `external`.
The timeline is a record of what happened, and Koinon does not rewrite it.

### 2. The identity is a row, not a payload field

A new table `external_actors` holds one row for each person:

- `org_id`, `id` (the key, as for every org table).
- `platform`: `slack` or `teams`.
- `team`: the Slack team ID or the Teams tenant ID of the workspace in which the person wrote. It is the `team` of
  the thread key ([0127](0127-chat-adapter-pure-core.md) decision 5).
- `platform_user_id`: the Slack user ID or the Teams user ID.
- `created_at`.

`(org_id, platform, team, platform_user_id)` is unique. The writer gets the row with one insert that returns the
existing row on a conflict (`EnsureExternalActor`), so two writers for the same person get the same row. The row holds no display name, no email address and no other profile data. Koinon does not
store what the platform says about the person.

`task_events` gets the column `actor_external_id`, with a foreign key `(org_id, actor_external_id)` to
`external_actors`. A check makes the column and the kind agree, as for `user` and `agent`:
`(actor_kind = 'external') = (actor_external_id IS NOT NULL)`.

The payload of an `external` message is a normal `TimelineMessage`. Its `source` is the platform (`slack` or
`teams`). It does not repeat the platform user ID: the row has it. This changes the text of
[0127](0127-chat-adapter-pure-core.md) decision 2, which put the platform user ID in the payload.

### 3. What an external actor can do

- **Start a task.** The chat driver's `CreateTask` ([0127](0127-chat-adapter-pure-core.md) decision 2) creates the
  task with the person's message as its first event. The task's creator stays the trigger (`task_creator_kind`
  `trigger`). `task_creator_kind` does not change.
- **Reply in the thread of the task.** `WakeTask` adds the message to the timeline.
- Nothing else. An `external` event must have the kind `message`. A database check enforces this:
  `actor_kind <> 'external' OR kind = 'message'`. Thus no code path can record an approval, an answer, a dismissal,
  a state change or a cancel by an `external` actor. The approval and question APIs already take a Koinon user, and
  only an org member with a role can call them.
- An `external` message never interrupts a running turn, and it never reopens a completed task. Both need a linked
  person ([0127](0127-chat-adapter-pure-core.md) decisions 2 and 5).

The catalog of typed timeline events in `internal/tasks` records the same rule for the writer: `ExternalActorCanWrite`
is true only for `message`. The database check is the rule that holds; the catalog function lets a writer refuse
the event before the insert.

### 4. What the timeline shows

- **API.** `TaskEvent.actor_kind` is `ACTOR_KIND_EXTERNAL`. The new field `TaskEvent.actor_external` holds the
  platform, the team and the platform user ID. `actor_display_name` stays empty. Any role that can read the task can
  read these fields, as for `actor_display_name`.
- **Web.** The timeline shows the message as a message from a person, with the label "Slack user" or "Teams user",
  the platform user ID, and the note "No Koinon account". A later change can show the platform's display name. It must
  read the name from the platform at display time and must not store it.
- **Agent prompt.** An `external` message is part of the run's prompt (`task_events.prompt` is true). A database check
  enforces this: `actor_kind <> 'external' OR prompt`. Thus a writer that forgets the flag fails, and the agent never
  misses the message. agentd puts one fixed line before its text and one fixed line after it. The line says that a person without a Koinon account wrote the message on the platform,
  and that this person cannot approve tool calls or answer questions. The line has no name and no ID. The agent thus
  does not ask this person for an approval. The line after the text marks where the person's text stops, so text in
  the message that claims to come from Koinon stays the person's text.
- **A2A edge.** An `external` message is a message from a person. The A2A history shows it with the role `user`, as a
  message from a Koinon user. Such a message also opens and answers A2A turns as a message from a Koinon user does,
  so the turns agree with the history.

### 5. Metrics and logs

No metric label carries the platform user ID, the team or the row ID ([0023](0023-observability-and-audit.md)). A log
line can name the row ID. It never names a message body ([0127](0127-chat-adapter-pure-core.md) decision 8).

### 6. Migration and upgrade

Two new migrations, both expand only ([0085](0085-upgrade-compatibility-and-recovery-policy.md)):

1. `ALTER TYPE task_actor_kind ADD VALUE 'external'`. It is alone in its migration, because Postgres does not let a
   transaction use an enum value that the same transaction added.
2. The `external_actors` table, the `actor_external_id` column, its foreign key and the three checks (the column
   and the kind agree, only `message`, always `prompt`). The checks and the foreign key are validated when they are
   added. `task_events` has no row with `actor_external_id` yet, so the scan is short.

No old row changes. An old binary never writes `external`. The first writer is the chat driver's apply step, which a
later issue adds; this decision adds the readers first.

## Alternatives considered

- **Record the message as `system` with the identity in the payload.** Rejected in
  [0127](0127-chat-adapter-pure-core.md) question 6. The timeline would say that Koinon wrote a person's words, and
  every reader would have to find people in `system` events by their payload.
- **Keep the identity in three columns on `task_events`.** Simple, but each event repeats the identity. A later link
  of the person to a Koinon account, or the removal of their identity, would then have to change many timeline rows.
  One row for each person keeps one place for both. _(reconstructed)_
- **Keep the identity only in the payload, as [0127](0127-chat-adapter-pure-core.md) first wrote.** The database could
  not check it, and a reader would need the payload to know who wrote the event. _(reconstructed)_
- **A shadow Koinon user for each person.** It gives the person a `users` row with no sign-in. Every check that trusts
  `users` (roles, approvals, the inbox) would then have to exclude these rows. A separate kind keeps them out by
  design. _(reconstructed)_

## Consequences

- Positive: the timeline shows who wrote each message, also for people without an account. A database check, not only
  the code, keeps such people away from approvals and questions.
- Positive: one row for each person is the place for a later account link and for the removal of a person's identity.
- Negative: every switch on `task_actor_kind` must handle a fourth value. The enum audit in `internal/services` fails
  until the API table has a row for it. _(inferred)_
- Negative: the timeline shows a platform user ID, not a name, until a later change reads the name from the platform.
- Negative: a question that the agent asks in a chat thread goes to the latest Koinon user who wrote in the thread
  (`LatestUserMessageActor`), not to an `external` person, who cannot answer it. When the latest message is from an
  `external` person, the question thus goes to an earlier Koinon user. The chat driver's issue decides how it shows
  such a question; the label "the person in this chat" is wrong for it. _(inferred)_
- Follow-ups: the chat driver's apply step writes `external` events (the code issue of
  [0127](0127-chat-adapter-pure-core.md) decision 2, which removes `Drop(unlinked_actor)`). The removal of an
  external person's identity on request is not decided here; it gets its own issue when the driver writes rows.

## Open questions

None.

## Upstream verification

Chatwoot (checked 2026-11-26, commit `73df3a610359a4c12e754ac254f98d36bafe6d09`) keeps a person who writes from an
outside channel as a `Contact`, apart from its `User` (an account that signs in). `contact_inboxes` has a unique
`(inbox_id, source_id)`: the platform's ID of the person in one channel. A message names its sender through a
polymorphic `sender_type` and `sender_id`. This decision takes the separate record and the unique platform key. It
differs in two points: a typed column with a database check instead of a polymorphic reference, and no profile data
(Chatwoot stores the name and the email address).

## Links

- Issue #1408, the decision comment of 2026-11-26.
- [0127](0127-chat-adapter-pure-core.md) decision 2 and questions 6 and 10.
- Chatwoot `app/models/message.rb` and `app/models/contact_inbox.rb` at commit
  `73df3a610359a4c12e754ac254f98d36bafe6d09`.
