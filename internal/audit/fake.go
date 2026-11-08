// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Fake is an in-memory Runner for unit tests of callers: it orders and validates a batch exactly as
// the Postgres runner does (they share the same code), and keeps what a committed transaction wrote.
// A transaction whose function or write fails leaves nothing behind. It has no hash chain and no
// locks; the Postgres specs cover those.
type Fake struct {
	// Queries is passed to the transaction function; tests that only check what was audited leave it nil.
	Queries *store.Queries

	mu       sync.Mutex
	n        int64
	installs []store.InstallAuditEvent
	orgs     []store.AuditEvent
}

var _ Runner = (*Fake)(nil)

// InTx implements Runner.
func (f *Fake) InTx(ctx context.Context, fn func(q *store.Queries, b Batch) error) (Written, error) {
	b := &batch{}
	if err := fn(f.Queries, b); err != nil {
		return Written{}, err
	}
	orgs, err := b.plan()
	if err != nil {
		return Written{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var w Written
	installID := ""
	var installs []store.InstallAuditEvent
	var rows []store.AuditEvent
	n := f.n
	for _, e := range b.installs {
		raw, err := buildSummary(e.Before, e.After, e.Reason, e.KoinonVersion, "")
		if err != nil {
			return Written{}, err
		}
		n++
		row := store.InstallAuditEvent{
			ID: uuidOf(n), ChainSeq: int64(len(f.installs) + len(installs) + 1), ActorKind: e.Actor.Kind, ActorID: e.Actor.ID,
			Action: e.Action, TargetKind: e.TargetKind, TargetID: e.TargetID, Summary: raw,
			RequestID: requestIDOf(ctx, e.RequestID),
		}
		installs = append(installs, row)
		if installID == "" {
			installID = row.ID.String()
		}
	}
	w.Install = installs
	for _, e := range orgs {
		if e.InstallEventID == "" {
			e.InstallEventID = installID
		}
		raw, err := buildSummary(e.Before, e.After, e.Reason, e.KoinonVersion, e.InstallEventID)
		if err != nil {
			return Written{}, err
		}
		n++
		row := store.AuditEvent{
			OrgID: e.Org, ID: uuidOf(n), ActorKind: e.Actor.Kind, ActorID: e.Actor.ID, Action: e.Action,
			TargetKind: e.TargetKind, TargetID: e.TargetID, Summary: raw, RequestID: requestIDOf(ctx, e.RequestID),
		}
		rows = append(rows, row)
	}
	f.n = n
	f.installs = append(f.installs, installs...)
	f.orgs = append(f.orgs, rows...)
	w.Org = rows
	return w, nil
}

func requestIDOf(ctx context.Context, id string) string {
	if id != "" {
		return id
	}
	return RequestID(ctx)
}

// InstallEvents returns the install events written so far, oldest first.
func (f *Fake) InstallEvents() []store.InstallAuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.InstallAuditEvent(nil), f.installs...)
}

// OrgEvents returns the org events written so far in the order they were written. With org valid it
// returns that org's events only.
func (f *Fake) OrgEvents(org pgtype.UUID) []store.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AuditEvent
	for _, e := range f.orgs {
		if !org.Valid || e.OrgID == org {
			out = append(out, e)
		}
	}
	return out
}

// SummaryOf decodes the summary of a written event.
func SummaryOf(raw []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}
