// SPDX-License-Identifier: Apache-2.0

package remoteagents

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Outcome is the result of one card read by the health check.
type Outcome struct {
	// Card is the card that was read; nil when the read failed.
	Card *a2aout.Card
	// Reason is the fixed text of a failed read (see reasonOf); empty when the read worked.
	Reason string
	// Seen is the updated_at of the row the read was made for. When the row changed since (an admin moved
	// the URL, the credential or the pin), the result is for another agent and RecordCheck drops it.
	Seen pgtype.Timestamptz
}

// ListCheckable returns the remote agents of one org that the health check reads.
func (s *Service) ListCheckable(ctx context.Context, org pgtype.UUID) ([]store.RemoteAgent, error) {
	return store.New(s.pool).ListCheckableRemoteAgents(ctx, org)
}

// ListOrgsToCheck returns the orgs that have a remote agent to check.
func (s *Service) ListOrgsToCheck(ctx context.Context) ([]pgtype.UUID, error) {
	return store.New(s.pool).ListOrgIDsWithCheckableRemoteAgents(ctx)
}

// Check reads the card of one remote agent and records the result. It returns the stored row.
func (s *Service) Check(ctx context.Context, row store.RemoteAgent) (RemoteAgent, error) {
	if err := s.hold.Err(); err != nil {
		return RemoteAgent{}, err
	}
	opener := s.opener
	if opener == nil && s.keys != nil {
		opener = s.keys
	}
	secret, err := OpenCredential(opener, row.OrgID, row.ID, row.Credential)
	if err != nil {
		// The log carries the error, which names no secret; the stored reason is a fixed text.
		s.log.Warn("remote agent check: credential not opened", "org", row.OrgID.String(), "agent", row.ID.String(), "err", err)
		return s.RecordCheck(ctx, row.OrgID, row.ID, Outcome{Reason: ReasonCredentialBad, Seen: row.UpdatedAt})
	}
	card, err := s.fetch(ctx, FetchRequest{
		URL: row.CardUrl, Header: row.CredentialHeader.String, Prefix: row.CredentialPrefix, Secret: secret,
		CABundle: row.CaBundle, InsecureSkipVerify: row.InsecureSkipVerify,
	}, s.policy)
	if err != nil {
		return s.RecordCheck(ctx, row.OrgID, row.ID, Outcome{Reason: reasonOf(err), Seen: row.UpdatedAt})
	}
	return s.RecordCheck(ctx, row.OrgID, row.ID, Outcome{Card: card, Seen: row.UpdatedAt})
}

// RecordCheck records one health check on the locked row.
//
//   - A failed read counts one failure. The agent is unhealthy after FailureThreshold failures in a row.
//     Health and the observed card stay as they were until then.
//   - A card with another hash than the pin is stored as the observed card. The agent is unhealthy at once:
//     the card is a finding, never an update, and the pin does not move.
//   - A card that is the pin makes the agent healthy, unless a finding is open. A finding stays until an
//     admin accepts the observed card: a card that changed and changed back is still something to read.
func (s *Service) RecordCheck(ctx context.Context, org, id pgtype.UUID, o Outcome) (RemoteAgent, error) {
	var out store.RemoteAgent
	err := s.tx(ctx, func(q *store.Queries, _ audit.Batch) error {
		cur, err := q.GetRemoteAgentForUpdate(ctx, store.GetRemoteAgentForUpdateParams{OrgID: org, ID: id})
		if err != nil {
			return err
		}
		if o.Seen.Valid && !o.Seen.Time.Equal(cur.UpdatedAt.Time) {
			// The row changed since the read: the result is stale. The next check reads the new row.
			out = cur
			return nil
		}
		p := store.RecordRemoteAgentCheckParams{
			OrgID: org, ID: id, Health: cur.Health, HealthReason: cur.HealthReason, ConsecutiveFailures: cur.ConsecutiveFailures,
			ObservedCardHash: cur.ObservedCardHash, ObservedCard: cur.ObservedCard,
		}
		switch {
		case o.Card == nil:
			p.ConsecutiveFailures++
			if p.ConsecutiveFailures >= FailureThreshold {
				p.Health = store.McpHealthUnhealthy
			}
			if !cur.ObservedCardHash.Valid {
				// An open finding keeps its reason: "card changed" is what an admin must act on.
				p.HealthReason = pgtype.Text{String: o.Reason, Valid: true}
			}
		case o.Card.Hash != cur.CardHash:
			p.CardRead, p.ConsecutiveFailures = true, 0
			p.Health = store.McpHealthUnhealthy
			p.HealthReason = pgtype.Text{String: ReasonCardChanged, Valid: true}
			p.ObservedCardHash = pgtype.Text{String: o.Card.Hash, Valid: true}
			p.ObservedCard = pgtype.Text{String: o.Card.JSON, Valid: true}
		case cur.ObservedCardHash.Valid:
			p.CardRead, p.ConsecutiveFailures = true, 0
			p.Health = store.McpHealthUnhealthy
			p.HealthReason = pgtype.Text{String: ReasonCardUnaccept, Valid: true}
		default:
			p.CardRead, p.ConsecutiveFailures = true, 0
			p.Health = store.McpHealthHealthy
			p.HealthReason = pgtype.Text{}
		}
		out, err = q.RecordRemoteAgentCheck(ctx, p)
		return err
	})
	if err != nil {
		return RemoteAgent{}, fmt.Errorf("remoteagents: record check: %w", mapErr(err))
	}
	return toRemoteAgent(out), nil
}
