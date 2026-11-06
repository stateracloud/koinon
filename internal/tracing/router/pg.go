// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
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
func (s *PGStore) ModelCalls(ctx context.Context, org, run pgtype.UUID, seq int32, from, to time.Time) ([]ModelCall, error) {
	rows, err := s.Q.ListRunModelCalls(ctx, store.ListRunModelCallsParams{
		OrgID: org, RunID: run, TurnSeq: seq,
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
		if r.ModelID.Valid {
			c.ModelID = r.ModelID.String()
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

// secretColumn is one place an org's secrets are stored as an envelope. The binding comes from the
// package that writes the column, so it cannot drift from what was sealed.
type secretColumn struct {
	table, column, key string
	binding            func(org, id pgtype.UUID) secretstore.Binding
	// values turns the opened plaintext into the secret values it holds.
	values func(plain []byte) []string
	// rowBinding, when set, replaces binding for a table whose key is not a UUID.
	rowBinding func(org pgtype.UUID, row string) secretstore.Binding
}

func whole(plain []byte) []string { return withAuthParts(string(plain)) }

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
// keys, connection fields, remote MCP server credentials, agent home repository passwords (every
// repository epoch) and the org's own trace destination credentials (a JSON object of field to value, or, for generic, header
// to value). Saved work is files, not credentials, and is left out. internal/keyscli.Columns lists
// every envelope; a new column that holds an org credential belongs here too.
var secretColumns = []secretColumn{
	{catalog.ProvidersTable, catalog.KeyColumn, "id", catalog.KeyBinding, whole, nil},
	{connections.Table, connections.Column, "id", connections.BindingFor, connectionFields, nil},
	{mcpservers.ServersTable, mcpservers.CredentialColumn, "id", mcpservers.CredentialBinding, whole, nil},
	{checkpoint.Table, checkpoint.Column, "agent_id", checkpoint.Binding, whole, nil},
	{checkpoint.EpochsTable, checkpoint.Column, "binding_row", nil, whole, checkpoint.EpochRowBinding},
	{tracedest.DestinationsTable, tracedest.CredentialsColumn, "id", tracedest.Binding, func(plain []byte) []string {
		var m map[string]string
		if json.Unmarshal(plain, &m) != nil {
			return nil
		}
		out := make([]string, 0, len(m))
		for _, v := range m {
			out = append(out, withAuthParts(v)...)
		}
		return out
	}, nil},
}

// Opener opens an envelope; *secretstore.Keyring is one.
type Opener interface {
	Open(b secretstore.Binding, e secretstore.Envelope) ([]byte, error)
}

// ErrUnopenable is returned by PGSecrets when a stored envelope cannot be opened: the set would miss
// a value, so the router sends no content for the org until it can be built whole.
var ErrUnopenable = errors.New("router: a stored credential of the org cannot be opened")

// PGSecrets builds an org's secret set from its stored envelopes (ADR 0074, decision 6: the values
// of the org's secret store and provider records).
type PGSecrets struct {
	DB interface {
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
	Keys Opener
	Log  *slog.Logger
}

// NewPGSecrets returns a SecretSource over pool. keys nil gives an empty set: without key-encryption
// keys nothing is stored.
func NewPGSecrets(pool *pgxpool.Pool, keys Opener, log *slog.Logger) *PGSecrets {
	if log == nil {
		log = slog.Default()
	}
	return &PGSecrets{DB: pool, Keys: keys, Log: log}
}

// Version fingerprints the org's stored envelopes (an md5 of their text), so the router notices a
// credential that was added, changed or removed by any writer without each writer telling it.
func (s *PGSecrets) Version(ctx context.Context, org pgtype.UUID) (string, error) {
	parts := make([]string, 0, len(secretColumns))
	for _, c := range secretColumns {
		parts = append(parts, fmt.Sprintf(`SELECT '%[1]s/' || %[3]s::text AS k, %[2]s::text AS v FROM %[1]s WHERE org_id = $1 AND %[2]s IS NOT NULL`, c.table, c.column, c.key))
	}
	var v string
	err := s.DB.QueryRow(ctx, `SELECT md5(coalesce(string_agg(k || '=' || v, '|' ORDER BY k), '')) FROM (`+strings.Join(parts, ` UNION ALL `)+`) s`, org).Scan(&v)
	return v, err
}

// Secrets implements SecretSource.
func (s *PGSecrets) Secrets(ctx context.Context, org pgtype.UUID) (*scrub.Secrets, error) {
	if s.Keys == nil {
		return scrub.NewSecrets(), nil
	}
	var (
		values     []string
		unopenable int
	)
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
				unopenable++
				continue
			}
			var b secretstore.Binding
			if c.rowBinding != nil {
				b = c.rowBinding(org, id)
			} else if rowID, perr := parseUUID(id); perr == nil {
				b = c.binding(org, rowID)
			} else {
				unopenable++
				continue
			}
			plain, err := s.Keys.Open(b, env)
			if err != nil {
				unopenable++
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
	if unopenable > 0 {
		// A value the scrubber cannot know is a value it cannot remove: no content for this org until
		// the envelope is readable again (a key-encryption key restored, the row fixed or deleted).
		s.Log.Error("trace router: stored credentials cannot be opened; sending no trace content for the org", "org", org.String(), "envelopes", unopenable)
		return nil, fmt.Errorf("%w (%d)", ErrUnopenable, unopenable)
	}
	return scrub.NewSecrets(values...), nil
}

// authScheme matches the value of an Authorization-style header: the scheme, then the token.
var authScheme = regexp.MustCompile(`(?i)^(bearer|basic|token|apikey|api-key|digest)\s+(\S.*)$`)

// withAuthParts adds the token after a known auth scheme (and, for Basic, the decoded user and
// password), because a trace may carry the token alone, without the scheme word.
func withAuthParts(v string) []string {
	out := []string{v}
	m := authScheme.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return out
	}
	out = append(out, m[2])
	if strings.EqualFold(m[1], "basic") {
		if dec, err := base64.StdEncoding.DecodeString(m[2]); err == nil {
			out = append(out, string(dec))
			out = append(out, strings.Split(string(dec), ":")...)
		}
	}
	return out
}
