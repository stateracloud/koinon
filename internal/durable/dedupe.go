// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Dedupe sources (ADR 0066, practice B1). A key is unique within an org and a source.
const (
	// SourceWebhook keys on the delivery ID of a signed webhook (GitHub X-GitHub-Delivery, Gitea
	// X-Gitea-Delivery).
	SourceWebhook = "webhook"
	// SourceSlack keys on a Slack event's event_id.
	SourceSlack = "slack"
	// SourceTrigger keys on a trigger plus its fire time: see TriggerFireKey.
	SourceTrigger = "trigger"
	// SourceManualTrigger keys on a trigger plus the second an admin fired it by hand ("run now").
	// It is a source of its own so that a manual fire never counts as the scheduled fire of the
	// same second.
	SourceManualTrigger = "trigger_manual"
	// SourceQuestion keys on an ask_user question ID.
	SourceQuestion = "question"
	// SourceChildTask keys on a child task ID.
	SourceChildTask = "child_task"
	// SourceDirectMessage keys on the client's message ID of a direct message (TR3).
	SourceDirectMessage = "direct_message"
	// SourcePoll keys on an item a code-host poller found: the connection, the resource and the
	// item's key at the provider (an event or comment ID), so a re-poll, a restart or a second
	// replica produce the same key (ADR 0066, amendment of 2026-11-12).
	SourcePoll = "poll"
)

// TriggerFireKey is the dedupe key of one fire of a trigger: the trigger's ID and the scheduled
// fire time (not the time the fire was processed), to the second, in UTC. Two processes that
// fire the same schedule slot, or a DBOS recovery that fires it again, produce the same key.
func TriggerFireKey(triggerID string, fire time.Time) string {
	return triggerID + "@" + strconv.FormatInt(fire.UTC().Unix(), 10)
}

// Outcome is the result of Once.
type Outcome struct {
	// First is true when this call claimed the key and ran the effect; false for a replay.
	First bool
	// EffectKind and EffectID name what the first call produced, as it returned them from the
	// effect, for both the first call and every replay. They are empty when the effect named none.
	EffectKind string
	EffectID   pgtype.UUID
}

// Effect is the work a dedupe key guards. It runs in the caller's transaction (q is bound to it)
// and returns what it produced, so a replay can find it. kind and id may both be zero.
type Effect func(q *store.Queries) (kind string, id pgtype.UUID, err error)

// Once runs effect at most once per (org, source, key). q must be bound to a transaction, the
// same one the effect writes in: the claim and the effect then commit or roll back together, so
// a failed effect leaves the key free for the redelivery, and a committed one blocks it. A
// concurrent claim of the same key waits for that transaction and then sees its outcome. That
// needs READ COMMITTED (pgx.BeginFunc's default): the follow-up read needs a fresh snapshot, and
// under REPEATABLE READ or SERIALIZABLE the waiting claim fails with a serialization error.
//
// One (source, key) guards exactly one effect kind. A caller that gets a replay must check
// Outcome.EffectKind before using EffectID: a record written by another effect, or by one that
// named none, is not its own.
func Once(ctx context.Context, q *store.Queries, org pgtype.UUID, source, key string, effect Effect) (Outcome, error) {
	if !org.Valid || source == "" || key == "" {
		return Outcome{}, errors.New("durable: dedupe needs an org, a source and a key")
	}
	_, err := q.ClaimDedupeKey(ctx, store.ClaimDedupeKeyParams{OrgID: org, Source: source, Key: key})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		rec, err := q.GetDedupeRecord(ctx, store.GetDedupeRecordParams{OrgID: org, Source: source, Key: key})
		if err != nil {
			return Outcome{}, fmt.Errorf("durable: read dedupe record: %w", err)
		}
		return Outcome{EffectKind: rec.EffectKind.String, EffectID: rec.EffectID}, nil
	case err != nil:
		return Outcome{}, fmt.Errorf("durable: claim dedupe key: %w", err)
	}
	kind, id, err := effect(q)
	if err != nil {
		return Outcome{}, err
	}
	if kind != "" && id.Valid {
		if err := q.SetDedupeEffect(ctx, store.SetDedupeEffectParams{
			OrgID: org, Source: source, Key: key,
			EffectKind: pgtype.Text{String: kind, Valid: true}, EffectID: id,
		}); err != nil {
			return Outcome{}, fmt.Errorf("durable: record dedupe effect: %w", err)
		}
	}
	return Outcome{First: true, EffectKind: kind, EffectID: id}, nil
}
