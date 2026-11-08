// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Batch collects the audit events of one transaction (the audit batch, ADR 0100 decision 1.7). A
// caller adds an event when the change happens and never chooses the order: just before commit the
// audit module writes the install events first, in the order they were added, then the org events
// sorted by org ID. Two transactions that audit orgs A,B and B,A therefore take the heads in the same
// order and cannot deadlock.
//
// An org event links to its install event through install_event_id in its summary. The link means
// "written in the same transaction", not "caused by". When the batch has exactly one install event,
// an org event that leaves InstallEventID empty gets that event's ID. When it has several, the module
// does not guess: an org event with no InstallEventID is refused (ErrAmbiguousInstallLink).
//
// Adding does not validate; the write does, and a refused event (an empty actor, a summary that
// looks like a secret, an ambiguous install link, a database refusal) fails the whole transaction, so
// an event commits with its change or not at all.
type Batch interface {
	// Install adds an install event. A batch may hold several; they are written in the order added.
	Install(e InstallEvent)
	// Org adds an org event. Events of one org keep the order they were added in.
	Org(e Event)
}

// Written is what the audit module wrote for one transaction, in the order it wrote it. A caller that
// needs a stored event (its ID, say) reads it here, after the transaction.
type Written struct {
	// Install are the install events, in the order they were added; empty when the batch had none.
	Install []store.InstallAuditEvent
	// Org are the org events, sorted by org ID.
	Org []store.AuditEvent
}

// Runner opens the transaction of an audited change. Production code uses the Postgres runner
// (NewRunner); a unit test of a caller may use Fake and check what was audited.
type Runner interface {
	// InTx runs fn in a transaction, then writes the batch fn filled and commits. q belongs to the
	// transaction: take domain locks and make the change with it. Any error from fn or from the
	// write rolls the whole transaction back. A Fake passes the Queries it was made with, which may
	// be nil.
	InTx(ctx context.Context, fn func(q *store.Queries, b Batch) error) (Written, error)
}

// Beginner opens transactions; *pgxpool.Pool is one.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewRunner returns the Postgres Runner for db.
func NewRunner(db Beginner) Runner { return pgRunner{db: db} }

// InTx is NewRunner(db).InTx.
func InTx(ctx context.Context, db Beginner, fn func(q *store.Queries, b Batch) error) (Written, error) {
	return pgRunner{db: db}.InTx(ctx, fn)
}

type pgRunner struct{ db Beginner }

func (r pgRunner) InTx(ctx context.Context, fn func(q *store.Queries, b Batch) error) (Written, error) {
	return InPgTx(ctx, r.db, func(tx pgx.Tx, b Batch) error { return fn(store.New(tx), b) })
}

// InPgTx is InTx for a caller that needs the pgx transaction itself, such as one that runs dynamic SQL
// with tx.Exec. Postgres only: a Fake has no transaction to give.
func InPgTx(ctx context.Context, db Beginner, fn func(tx pgx.Tx, b Batch) error) (Written, error) {
	var w Written
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		b := &batch{}
		if err := fn(tx, b); err != nil {
			return err
		}
		var err error
		w, err = b.write(ctx, store.New(tx))
		return err
	})
	if err != nil {
		return Written{}, err
	}
	return w, nil
}

// WriteInTx adds events with fn and writes them in tx, a transaction the caller opened and ends itself
// (Postgres only, like InPgTx). It is for the one caller whose transaction outlives a function: a key
// change takes an advisory lock before it edits a Secret and writes its events after, so it cannot hand
// the whole change to InTx. The caller owns the lock order: it takes its own locks first, and this call
// writes the install events, then the org events by org ID, as InTx does. A failed write leaves tx
// aborted unless the caller wrapped the call in a savepoint.
//
// Only internal/keyscli may call WriteInTx; everything else uses InTx or InPgTx, which open and end
// the transaction themselves. hack/audit-batch-lint.sh fails the build for any other caller (#371).
func WriteInTx(ctx context.Context, tx pgx.Tx, fn func(b Batch)) (Written, error) {
	b := &batch{}
	fn(b)
	return b.write(ctx, store.New(tx))
}

// ErrAmbiguousInstallLink is in the error of a batch that holds more than one install event and an
// org event with no InstallEventID: the module refuses to pick the install event it links to, and the
// whole transaction rolls back (ADR 0100 decision 1.7).
var ErrAmbiguousInstallLink = errors.New("audit: the batch has several install events; an org event must name its install_event_id")

// batch is the Batch the Postgres runner and the Fake share, so both order and validate the same way.
type batch struct {
	installs []InstallEvent
	orgs     []Event
}

func (b *batch) Install(e InstallEvent) { b.installs = append(b.installs, e) }
func (b *batch) Org(e Event)            { b.orgs = append(b.orgs, e) }

// plan checks the batch and returns the org events in the order they are written. It is the one place
// that decides the order and the refusals.
func (b *batch) plan() ([]Event, error) {
	for _, e := range b.installs {
		if err := needActor(e.Actor, e.Action); err != nil {
			return nil, err
		}
	}
	orgs := append([]Event(nil), b.orgs...)
	for _, e := range orgs {
		if err := needActor(e.Actor, e.Action); err != nil {
			return nil, err
		}
		if !e.Org.Valid {
			return nil, fmt.Errorf("audit: org event %s has no org", e.Action)
		}
		if len(b.installs) > 1 && e.InstallEventID == "" {
			return nil, fmt.Errorf("audit: org event %s: %w", e.Action, ErrAmbiguousInstallLink)
		}
	}
	sort.SliceStable(orgs, func(i, j int) bool { return bytes.Compare(orgs[i].Org.Bytes[:], orgs[j].Org.Bytes[:]) < 0 })
	return orgs, nil
}

func needActor(a Actor, action string) error {
	if a.Kind == "" || a.ID == "" {
		return fmt.Errorf("audit: event %s needs a named actor", action)
	}
	return nil
}

// write writes the batch with q: the install events first, then the org events by org ID.
func (b *batch) write(ctx context.Context, q *store.Queries) (Written, error) {
	return b.writeWith(
		func(e InstallEvent) (store.InstallAuditEvent, error) { return recordInstall(ctx, q, e) },
		func(e Event) (store.AuditEvent, error) { return record(ctx, q, e) },
	)
}

// writeWith is the one write loop: it plans the batch, then inserts the install events and the org
// events in order with the two insert functions. The Postgres runner passes the store inserts, the
// Fake its in-memory ones, so the order, the refusals and the install link cannot drift apart.
func (b *batch) writeWith(
	insertInstall func(InstallEvent) (store.InstallAuditEvent, error),
	insertOrg func(Event) (store.AuditEvent, error),
) (Written, error) {
	orgs, err := b.plan()
	if err != nil {
		return Written{}, err
	}
	var w Written
	for _, e := range b.installs {
		row, err := insertInstall(e)
		if err != nil {
			return Written{}, err
		}
		w.Install = append(w.Install, row)
	}
	for _, e := range orgs {
		if e.InstallEventID == "" && len(w.Install) == 1 {
			e.InstallEventID = w.Install[0].ID.String()
		}
		row, err := insertOrg(e)
		if err != nil {
			return Written{}, err
		}
		w.Org = append(w.Org, row)
	}
	return w, nil
}

// uuidOf returns the pgtype.UUID for n, for the Fake's sequential IDs.
func uuidOf(n int64) pgtype.UUID {
	var u pgtype.UUID
	u.Valid = true
	for i := 0; i < 8; i++ {
		u.Bytes[15-i] = byte((n >> (8 * i)) & 0xff)
	}
	return u
}
