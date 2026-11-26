// SPDX-License-Identifier: Apache-2.0

// Package leaderterm fences the database writes of the reconciler leader with a term (issue 635,
// ADR 0076 amendment 2026-11-26).
//
// The leader holds a Postgres session advisory lock. A hard node loss or a partition can let a new
// leader take the lock while the old one still works, for up to the heartbeat bound of ADR 0076. The
// term closes that window for database writes: each new leader increments the term (Take) while it
// holds the lock, and puts it on its context (WithTerm). DB sends each statement of a context that
// carries a term in one implicit transaction together with reconciler_check_term(term), so the
// database refuses a statement of an old term (ErrStale). The check holds a shared transaction
// advisory lock until the transaction ends, and the take holds it exclusive: the take of the next
// leader waits for each statement of the old term that is in progress, so none of them commits after
// the newer term (migration 00116). The transactions must be READ COMMITTED (the default), so the
// check reads the term after it holds the lock; the check refuses any other level.
//
// The term covers database writes only. Kubernetes deletes and execution-layer calls keep the time
// fence of ADR 0076 (gateway.WithFence); issue 1465 names their exit condition.
package leaderterm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// IdleInTransaction bounds how long a transaction of a lead may be idle. The reconciler's transactions
// are short; this bounds how long a vanished leader delays the next one. When it fires, Postgres ends
// the whole session, not only the transaction: a fenced transaction must do database work only, and
// never wait on a Kubernetes or backend call.
const IdleInTransaction = 10 * time.Second

// staleCode is the SQLSTATE that reconciler_check_term raises (migration 00116).
const staleCode = "KO635"

const (
	checkSQL = "SELECT reconciler_check_term($1)"
	// beginSQL is the check of a transaction. The transaction also ends when it is idle for longer
	// than IdleInTransaction: a leader that vanished in an open transaction (a partition) would hold
	// the shared lock until Postgres sees the dead client, which can be hours, and the next leader's
	// take would wait that long.
	beginSQL = "SELECT reconciler_check_term($1), set_config('idle_in_transaction_session_timeout', $2, true)"
	takeSQL  = "SELECT reconciler_take_term()"
)

// ErrStale is the error of a statement whose term is not the current term: a newer leader took the
// lead. The caller must stop leading.
var ErrStale = errors.New("leaderterm: a newer reconciler leader holds the term")

// Execer is what Take needs: the connection that holds the leader lock.
type Execer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Take increments the term and returns the new one. Call it only while c holds the leader lock.
func Take(ctx context.Context, c Execer) (int64, error) {
	var term int64
	if err := c.QueryRow(ctx, takeSQL).Scan(&term); err != nil {
		return 0, fmt.Errorf("leaderterm: take the term: %w", err)
	}
	return term, nil
}

type termKey struct{}

type lease struct {
	term int64
	lost func(error)
}

// WithTerm returns a ctx whose statements through DB carry term. lost, when not nil, is called with
// the error when the database refuses the term, so the leader stops all its work, not only the
// statement that failed.
func WithTerm(ctx context.Context, term int64, lost func(error)) context.Context {
	return context.WithValue(ctx, termKey{}, lease{term: term, lost: lost})
}

// From returns the term on ctx.
func From(ctx context.Context) (int64, bool) {
	l, ok := ctx.Value(termKey{}).(lease)
	return l.term, ok
}

// Pool is what DB wraps: *pgxpool.Pool satisfies it.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DB checks the term of the context of each statement. A statement whose context has no term (a
// replica that does not lead, a tool that is not the reconciler) goes to the pool unchanged. DB
// satisfies store.DBTX and staterev.DB.
type DB struct {
	Pool Pool
}

// Exec sends the check and the statement in one batch: one round trip, one implicit transaction.
func (d DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	l, ok := ctx.Value(termKey{}).(lease)
	if !ok {
		return d.Pool.Exec(ctx, sql, args...)
	}
	br := d.batch(ctx, l, sql, args)
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return pgconn.CommandTag{}, l.refused(err)
	}
	tag, err := br.Exec()
	if cerr := br.Close(); err == nil && cerr != nil {
		err = cerr
	}
	return tag, err
}

// Query sends the check and the statement in one batch. The rows close the batch.
func (d DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	l, ok := ctx.Value(termKey{}).(lease)
	if !ok {
		return d.Pool.Query(ctx, sql, args...)
	}
	br := d.batch(ctx, l, sql, args)
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return nil, l.refused(err)
	}
	rows, err := br.Query()
	if err != nil {
		_ = br.Close()
		return nil, err
	}
	return &batchRows{Rows: rows, br: br}, nil
}

// QueryRow sends the check and the statement in one batch. Scan closes the batch.
func (d DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	l, ok := ctx.Value(termKey{}).(lease)
	if !ok {
		return d.Pool.QueryRow(ctx, sql, args...)
	}
	br := d.batch(ctx, l, sql, args)
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return errRow{err: l.refused(err)}
	}
	return &batchRow{row: br.QueryRow(), br: br}
}

// Begin starts a transaction whose first statement is the check, so the whole transaction holds the
// term.
func (d DB) Begin(ctx context.Context) (pgx.Tx, error) { return begin(ctx, d.Pool) }

// Beginner is a database that only starts transactions, such as staterev.DB.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// TxDB is DB for a Beginner: each transaction of a context with a term starts with the check.
type TxDB struct {
	DB Beginner
}

// Begin starts a transaction whose first statement is the check.
func (d TxDB) Begin(ctx context.Context) (pgx.Tx, error) { return begin(ctx, d.DB) }

func begin(ctx context.Context, db Beginner) (pgx.Tx, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	l, ok := ctx.Value(termKey{}).(lease)
	if !ok {
		return tx, nil
	}
	if _, err := tx.Exec(ctx, beginSQL, l.term, strconv.FormatInt(IdleInTransaction.Milliseconds(), 10)); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, l.refused(err)
	}
	return tx, nil
}

func (d DB) batch(ctx context.Context, l lease, sql string, args []any) pgx.BatchResults {
	b := &pgx.Batch{}
	b.Queue(checkSQL, l.term)
	b.Queue(sql, args...)
	return d.Pool.SendBatch(ctx, b)
}

// refused maps the error of the check: ErrStale for a stale term (and lost is told), err otherwise.
func (l lease) refused(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != staleCode {
		return err
	}
	err = fmt.Errorf("%w (term %d)", ErrStale, l.term)
	if l.lost != nil {
		l.lost(err)
	}
	return err
}

type batchRows struct {
	pgx.Rows
	br       pgx.BatchResults
	closed   bool
	closeErr error
}

// Next closes the batch after the last row, so Err also reports an error of the implicit commit at
// the end of the batch (a deferred constraint, for example).
func (r *batchRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.close()
	return false
}

func (r *batchRows) Err() error {
	if err := r.Rows.Err(); err != nil {
		return err
	}
	return r.closeErr
}

func (r *batchRows) Close() { r.close() }

func (r *batchRows) close() {
	if r.closed {
		return
	}
	r.closed = true
	r.Rows.Close()
	r.closeErr = r.br.Close()
}

type batchRow struct {
	row pgx.Row
	br  pgx.BatchResults
}

func (r *batchRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if cerr := r.br.Close(); err == nil && cerr != nil {
		err = cerr
	}
	return err
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
