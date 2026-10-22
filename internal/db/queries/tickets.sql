-- SPDX-License-Identifier: Apache-2.0
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateTicket :one
INSERT INTO tickets (org_id, title, agent_id, created_by)
VALUES (@org_id, @title, sqlc.narg(agent_id), sqlc.narg(created_by))
RETURNING *;

-- name: GetTicket :one
SELECT * FROM tickets
WHERE org_id = @org_id AND id = @id;

-- name: ListTickets :many
SELECT * FROM tickets
WHERE org_id = @org_id
ORDER BY created_at, id;

-- name: ListTicketsByStatus :many
-- One board column, oldest first.
SELECT * FROM tickets
WHERE org_id = @org_id AND status = @status
ORDER BY created_at, id;

-- name: MoveTicket :one
-- Moves a ticket to a board column. Callers moving to Done also cancel its
-- queued runs (CancelQueuedRunsForTicket) in the same transaction.
UPDATE tickets
SET status = @status,
    done_at = CASE WHEN @status::ticket_status = 'done' THEN coalesce(done_at, now()) END
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: SetTicketHarnessSession :one
-- Stores the harness session for the ticket. Fenced by the run epoch: only the
-- current owner of one of the ticket's active runs can set it.
UPDATE tickets t
SET harness_session_id = @harness_session_id::text, harness_session_cwd = @harness_session_cwd::text
WHERE t.org_id = @org_id AND t.id = @id
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.org_id = @org_id AND r.ticket_id = t.id AND r.id = @run_id
        AND r.epoch = @epoch AND r.state IN ('starting', 'running'))
RETURNING t.*;

-- name: AppendTicketEvent :one
WITH ticket AS (
    UPDATE tickets
    SET last_event_seq = last_event_seq + 1
    WHERE tickets.org_id = @org_id AND tickets.id = @ticket_id
    RETURNING tickets.org_id, tickets.id, tickets.last_event_seq
)
INSERT INTO ticket_events (org_id, ticket_id, seq, kind, actor_kind, actor_user_id,
                           actor_agent_id, run_id, payload)
SELECT ticket.org_id, ticket.id, ticket.last_event_seq, @kind, @actor_kind,
       sqlc.narg(actor_user_id), sqlc.narg(actor_agent_id), sqlc.narg(run_id), @payload
FROM ticket
RETURNING *;

-- name: ListTicketEvents :many
-- The timeline after a sequence number (0 for the start), in order.
SELECT * FROM ticket_events
WHERE org_id = @org_id AND ticket_id = @ticket_id AND seq > @after_seq
ORDER BY seq
LIMIT @max_events;

-- name: ListTicketMessagesAfter :many
-- The users' messages on the ticket after a timeline seq, in order: what
-- agentd sends the harness as a run's prompt.
SELECT * FROM ticket_events
WHERE org_id = @org_id AND ticket_id = @ticket_id AND seq > @after_seq
  AND kind = 'message' AND actor_kind = 'user'
ORDER BY seq;
