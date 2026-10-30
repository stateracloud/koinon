// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// PGStore is the Postgres Store.
type PGStore struct{ Q store.Querier }

// NewPGStore returns a Store over db.
func NewPGStore(db store.DBTX) *PGStore { return &PGStore{Q: store.New(db)} }

// Attribute implements Store.
func (s *PGStore) Attribute(ctx context.Context, org, run pgtype.UUID, trace [16]byte) (Turn, error) {
	traced, err := s.Q.GetRunTraced(ctx, store.GetRunTracedParams{OrgID: org, ID: run})
	if errors.Is(err, pgx.ErrNoRows) {
		return Turn{}, ErrUnknownRun
	}
	if err != nil {
		return Turn{}, err
	}
	if !traced.Valid || !traced.Bool {
		return Turn{}, ErrUntraced
	}
	t, err := s.Q.GetRunTurnByTrace(ctx, store.GetRunTurnByTraceParams{OrgID: org, TraceID: trace[:]})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && t.RunID != run {
		return Turn{}, ErrUnknownTrace
	}
	if err != nil {
		return Turn{}, err
	}
	out := Turn{Seq: t.Seq, StartedAt: t.StartedAt.Time}
	if t.EndedAt.Valid {
		end := t.EndedAt.Time
		out.EndedAt = &end
	}
	return out, nil
}

// ModelCalls implements Store.
func (s *PGStore) ModelCalls(ctx context.Context, org, run pgtype.UUID, from, to time.Time) ([]ModelCall, error) {
	rows, err := s.Q.ListRunModelCalls(ctx, store.ListRunModelCallsParams{
		OrgID: org, RunID: run,
		FromTime: pgtype.Timestamptz{Time: from, Valid: true}, ToTime: pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	out := make([]ModelCall, 0, len(rows))
	for _, r := range rows {
		c := ModelCall{
			RequestID: r.RequestID, Model: r.Model, Provider: r.ProviderType,
			Input: r.InputTokens, Output: r.OutputTokens, CacheRead: r.CacheReadTokens, CacheWrite: r.CacheWriteTokens, Reasoning: r.ReasoningTokens,
			Provisional: r.Provisional, Estimated: r.Estimated, UpstreamStatus: int(r.UpstreamStatus.Int16), Time: r.OccurredAt.Time,
		}
		if r.AgentID.Valid {
			c.AgentID = r.AgentID.String()
		}
		if r.TaskID.Valid {
			c.TaskID = r.TaskID.String()
		}
		if r.Priced {
			c.Cost = numericFloat(r.CostUsd)
		}
		c.GatewayCost = numericFloat(r.GatewayCostUsd)
		out = append(out, c)
	}
	return out, nil
}

func numericFloat(n pgtype.Numeric) *float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return nil
	}
	return &f.Float64
}

// PGHealth records destination health in Postgres.
type PGHealth struct{ Q store.Querier }

// NewPGHealth returns a Health over db.
func NewPGHealth(db store.DBTX) *PGHealth { return &PGHealth{Q: store.New(db)} }

// Success implements Health.
func (h *PGHealth) Success(ctx context.Context, org pgtype.UUID, route tracedest.Route) error {
	if route.Platform {
		return h.Q.RecordPlatformTraceDestinationSuccess(ctx)
	}
	id, err := parseUUID(route.ID)
	if err != nil {
		return err
	}
	return h.Q.RecordTraceDestinationSuccess(ctx, store.RecordTraceDestinationSuccessParams{OrgID: org, ID: id})
}

// Failure implements Health. message is secret-free (the exporter writes it so).
func (h *PGHealth) Failure(ctx context.Context, org pgtype.UUID, route tracedest.Route, message string, dropped int64) error {
	text := pgtype.Text{String: message, Valid: true}
	if route.Platform {
		return h.Q.RecordPlatformTraceDestinationError(ctx, store.RecordPlatformTraceDestinationErrorParams{LastError: text, SpansDropped: dropped})
	}
	id, err := parseUUID(route.ID)
	if err != nil {
		return err
	}
	return h.Q.RecordTraceDestinationError(ctx, store.RecordTraceDestinationErrorParams{OrgID: org, ID: id, LastError: text, SpansDropped: dropped})
}

func parseUUID(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil || !id.Valid {
		return id, fmt.Errorf("router: bad destination id")
	}
	return id, nil
}

// secretColumn is one place an org's secrets are stored as an envelope.
type secretColumn struct {
	table, column, key string
	// values turns the opened plaintext into the secret values it holds.
	values func(plain []byte) []string
}

func whole(plain []byte) []string { return []string{string(plain)} }

// connectionFields: a connection's envelope is a JSON object of setup-guide field ID to value
// (bytes, so base64 in JSON).
func connectionFields(plain []byte) []string {
	var m map[string][]byte
	if json.Unmarshal(plain, &m) != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, string(v))
	}
	return out
}

// secretColumns are the org-owned envelopes whose values must never appear in a trace: provider API
// keys, connection fields, remote MCP server credentials, agent home repository passwords and the
// org's own trace destination credentials (a JSON object of field to value, or, for generic, header
// to value). Saved work is files, not credentials, and is left out. internal/keyscli.Columns lists
// every envelope; a new column that holds an org credential belongs here too.
var secretColumns = []secretColumn{
	{catalog.ProvidersTable, catalog.KeyColumn, "id", whole},
	{connections.Table, connections.Column, "id", connectionFields},
	{mcpservers.ServersTable, mcpservers.CredentialColumn, "id", whole},
	{checkpoint.Table, checkpoint.Column, "agent_id", whole},
	{tracedest.DestinationsTable, tracedest.CredentialsColumn, "id", func(plain []byte) []string {
		var m map[string]string
		if json.Unmarshal(plain, &m) != nil {
			return nil
		}
		out := make([]string, 0, len(m))
		for _, v := range m {
			out = append(out, v)
		}
		return out
	}},
}

// Opener opens an envelope; *secretstore.Keyring is one.
type Opener interface {
	Open(b secretstore.Binding, e secretstore.Envelope) ([]byte, error)
}

// PGSecrets builds an org's secret set from its stored envelopes (ADR 0074, decision 6: the values
// of the org's secret store and provider records).
type PGSecrets struct {
	DB interface {
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	}
	Keys Opener
}

// NewPGSecrets returns a SecretSource over pool. keys nil gives an empty set: without key-encryption
// keys nothing is stored.
func NewPGSecrets(pool *pgxpool.Pool, keys Opener) *PGSecrets {
	return &PGSecrets{DB: pool, Keys: keys}
}

// Secrets implements SecretSource.
func (s *PGSecrets) Secrets(ctx context.Context, org pgtype.UUID) (*scrub.Secrets, error) {
	if s.Keys == nil {
		return scrub.NewSecrets(), nil
	}
	var values []string
	for _, c := range secretColumns {
		// The identifiers are constants of this package.
		rows, err := s.DB.Query(ctx, fmt.Sprintf(`SELECT %[3]s::text, %[2]s FROM %[1]s WHERE org_id = $1 AND %[2]s IS NOT NULL`, c.table, c.column, c.key), org)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", c.table, err)
		}
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("reading %s: %w", c.table, err)
			}
			var env secretstore.Envelope
			if json.Unmarshal(raw, &env) != nil {
				continue
			}
			plain, err := s.Keys.Open(secretstore.Binding{OrgID: org.String(), Table: c.table, RowID: id, Column: c.column}, env)
			if err != nil {
				// One value that cannot be opened (a retired key) must not blind the scrubber to the others.
				continue
			}
			values = append(values, c.values(plain)...)
			clear(plain)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("reading %s: %w", c.table, err)
		}
	}
	return scrub.NewSecrets(values...), nil
}
