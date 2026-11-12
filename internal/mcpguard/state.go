// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/leakguard"
)

// DefaultMaxFingerprints bounds the taint store of one session, as leakguard.MemoryStore does:
// about 1 MiB of private canonical text at the default window. Past it the oldest go first and the
// session is marked evicted.
const DefaultMaxFingerprints = 1 << 20

// Session is one home generation of one agent: the scope of its legs and its taint (ADR 0084
// decision 4, ADR 0091 decision 4).
type Session struct {
	Org, Agent pgtype.UUID
	Generation string
}

func (s Session) scope() leakguard.Scope {
	return leakguard.Scope{Org: s.Org.String(), Agent: s.Agent.String(), Generation: s.Generation}
}

// Legs are a session's two legs.
type Legs struct {
	UntrustedInput, PrivateData bool
}

// Both reports whether both legs are held.
func (l Legs) Both() bool { return l.UntrustedInput && l.PrivateData }

// Any reports whether either leg is set.
func (l Legs) Any() bool { return l.UntrustedInput || l.PrivateData }

// State is the guard's state in Postgres: sessions with their legs and fingerprint keys, the taint
// store, and the budgets of runs. Every koinon-api replica and agentd share it.
type State struct {
	pool            *pgxpool.Pool
	q               *store.Queries
	maxFingerprints int
	metrics         *Metrics
}

// NewState returns the state on pool. maxFingerprints <= 0 means DefaultMaxFingerprints; metrics
// may be nil.
func NewState(pool *pgxpool.Pool, maxFingerprints int, metrics *Metrics) *State {
	if maxFingerprints <= 0 {
		maxFingerprints = DefaultMaxFingerprints
	}
	return &State{pool: pool, q: store.New(pool), maxFingerprints: maxFingerprints, metrics: metrics}
}

// Generation returns the home generation of an agent's sandbox: home_generation, or the sandbox ID
// that becomes it on first ready. False when the agent has no sandbox yet.
func Generation(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (string, bool, error) {
	sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if sb.HomeGeneration.Valid && sb.HomeGeneration.String != "" {
		return sb.HomeGeneration.String, true, nil
	}
	if sb.SandboxID.Valid && sb.SandboxID.String != "" {
		return sb.SandboxID.String, true, nil
	}
	return "", false, nil
}

// Ensure returns the session's row, creating it (both legs clear, a new random key) the first
// time.
func (s *State) Ensure(ctx context.Context, sess Session) (store.LeakGuardSession, error) {
	return ensure(ctx, s.q, sess)
}

func ensure(ctx context.Context, q *store.Queries, sess Session) (store.LeakGuardSession, error) {
	if sess.Generation == "" {
		return store.LeakGuardSession{}, errors.New("mcpguard: a session needs a home generation")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return store.LeakGuardSession{}, err
	}
	return q.EnsureLeakGuardSession(ctx, store.EnsureLeakGuardSessionParams{
		OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, FpKey: key,
	})
}

// Fingerprinter returns the session's fingerprinter at threshold (0 is the default).
func Fingerprinter(row store.LeakGuardSession, threshold int) (*leakguard.Fingerprinter, error) {
	p := leakguard.DefaultParams()
	if threshold > 0 {
		p.Threshold = threshold
	}
	return leakguard.NewFingerprinter(row.FpKey, p)
}

// Record sets legs on the session (they only grow) and adds the fingerprints of texts to its taint
// store. The texts are fingerprinted here and never stored. It returns the legs after the change.
func (s *State) Record(ctx context.Context, sess Session, legs Legs, threshold int, texts []string) (Legs, error) {
	var out Legs
	var evicted int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		row, err := ensure(ctx, q, sess)
		if err != nil {
			return err
		}
		if legs.Any() {
			row, err = q.AddLeakGuardLegs(ctx, store.AddLeakGuardLegsParams{
				OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation,
				UntrustedInput: legs.UntrustedInput, PrivateData: legs.PrivateData,
			})
			if err != nil {
				return err
			}
		}
		out = Legs{UntrustedInput: row.UntrustedInput, PrivateData: row.PrivateData}
		s.metrics.legs(legs)
		if len(texts) == 0 {
			return nil
		}
		fp, err := Fingerprinter(row, threshold)
		if err != nil {
			return err
		}
		var fps []int64
		for _, t := range texts {
			for _, f := range fp.Winnow(t) {
				fps = append(fps, int64(f)) //nolint:gosec // two's complement, as stored
			}
		}
		evicted, err = s.add(ctx, q, sess, fps)
		return err
	})
	if err == nil {
		s.metrics.evicted(evicted)
	}
	return out, err
}

// AddFingerprints adds fingerprints a caller computed with the session's key (agentd's feed) and
// sets legs.
func (s *State) AddFingerprints(ctx context.Context, sess Session, legs Legs, fps []leakguard.Fingerprint) error {
	var evicted int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := ensure(ctx, q, sess); err != nil {
			return err
		}
		if legs.Any() {
			if _, err := q.AddLeakGuardLegs(ctx, store.AddLeakGuardLegsParams{
				OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation,
				UntrustedInput: legs.UntrustedInput, PrivateData: legs.PrivateData,
			}); err != nil {
				return err
			}
		}
		out := make([]int64, len(fps))
		for i, f := range fps {
			out[i] = int64(f) //nolint:gosec // the bits are kept: a fingerprint is stored as its two's complement
		}
		var err error
		evicted, err = s.add(ctx, q, sess, out)
		return err
	})
	if err == nil {
		s.metrics.evicted(evicted)
	}
	return err
}

// add inserts fingerprints in chunks and evicts the oldest past the bound. The texts of one result
// winnow to the same fingerprints more than once (a tool's content and its raw output, an edit's two
// sides), so each is inserted once (AddLeakGuardFingerprints dedupes too: one statement may not
// upsert a row twice). It returns the number of fingerprints it evicted; the caller counts them once
// the transaction commits, so a rolled-back eviction is not counted.
func (s *State) add(ctx context.Context, q *store.Queries, sess Session, fps []int64) (int64, error) {
	fps = slices.Compact(slices.Sorted(slices.Values(fps)))
	const chunk = 8192
	for len(fps) > 0 {
		n := min(len(fps), chunk)
		if _, err := q.AddLeakGuardFingerprints(ctx, store.AddLeakGuardFingerprintsParams{
			OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, Fps: fps[:n],
		}); err != nil {
			return 0, err
		}
		fps = fps[n:]
	}
	count, err := q.CountLeakGuardFingerprints(ctx, store.CountLeakGuardFingerprintsParams{OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation})
	if err != nil {
		return 0, err
	}
	if over := count - int64(s.maxFingerprints); over > 0 {
		n, err := q.EvictLeakGuardFingerprints(ctx, store.EvictLeakGuardFingerprintsParams{
			OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, N: int32(min(over, 1<<30)),
		})
		if err != nil {
			return 0, err
		}
		if err := q.MarkLeakGuardEvicted(ctx, store.MarkLeakGuardEvictedParams{OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation}); err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, nil
}

// taintStore is the leakguard.Store of one session's table rows, for the TaintChecker.
type taintStore struct {
	q       *store.Queries
	sess    Session
	evicted bool
}

var _ leakguard.Store = (*taintStore)(nil)

func (t *taintStore) Add(context.Context, leakguard.Scope, []leakguard.Fingerprint) error {
	return errors.New("mcpguard: the check's store is read-only")
}

func (t *taintStore) Drop(context.Context, leakguard.Scope) error {
	return errors.New("mcpguard: the check's store is read-only")
}

func (t *taintStore) Lookup(ctx context.Context, scope leakguard.Scope, fps []leakguard.Fingerprint) (leakguard.Hits, error) {
	if scope != t.sess.scope() {
		return leakguard.Hits{}, fmt.Errorf("mcpguard: lookup of scope %v in the store of %v", scope, t.sess.scope())
	}
	want := make([]int64, len(fps))
	for i, f := range fps {
		want[i] = int64(f) //nolint:gosec // two's complement, as stored
	}
	held := map[int64]struct{}{}
	const chunk = 8192
	for i := 0; i < len(want); i += chunk {
		rows, err := t.q.LookupLeakGuardFingerprints(ctx, store.LookupLeakGuardFingerprintsParams{
			OrgID: t.sess.Org, AgentID: t.sess.Agent, Generation: t.sess.Generation, Fps: want[i:min(i+chunk, len(want))],
		})
		if err != nil {
			return leakguard.Hits{}, err
		}
		for _, r := range rows {
			held[r] = struct{}{}
		}
	}
	found := make([]bool, len(fps))
	for i, w := range want {
		_, found[i] = held[w]
	}
	return leakguard.Hits{Found: found, Evicted: t.evicted}, nil
}
