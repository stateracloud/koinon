// SPDX-License-Identifier: Apache-2.0

package schemagate

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnsupported is the cause of a Hold that the gate stopped: a check read a schema this build
// cannot run against.
var ErrUnsupported = errors.New("the database schema is not supported by this build")

// Hold runs work only while the schema is supported (issue 680). A probe that fails takes the pod
// out of its Service, but it does not stop a loop that claims work from the database on its own
// (koinon-agentd's workers, koinon-reconciler's passes); Hold does.
//
// Hold waits until a check passes, then runs work under a context that ends when a later check
// reads the schema and judges it unsupported. Then Hold returns an error that wraps
// ErrUnsupported, after work has returned: the caller exits, and the restarted process waits here
// again. A check that cannot read the schema (the database is down) does not stop work: work
// fails on its own then, and stopping it would turn every short outage into a restart.
//
// Something must call Run (or Check) meanwhile; Hold only reads the verdicts. It returns ctx's
// error when ctx ends while it waits, and otherwise work's error.
func (g *Gate) Hold(ctx context.Context, work func(context.Context) error) error {
	seen, err := g.waitSupported(ctx)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		if err := g.waitRejected(wctx, seen); err != nil {
			cancel(err)
		}
	}()
	err = work(wctx)
	cancel(nil)
	<-watched
	if cause := context.Cause(wctx); errors.Is(cause, ErrUnsupported) {
		return cause
	}
	return err
}

// waitSupported waits until the latest check passed and returns the rejection count at that time.
func (g *Gate) waitSupported(ctx context.Context) (uint64, error) {
	for {
		g.mu.Lock()
		ok, seen, next := g.checked && g.err == nil, g.rejected, g.checks
		g.mu.Unlock()
		if ok {
			return seen, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-next:
		}
	}
}

// waitRejected waits until a check after seen judged the schema unsupported, and returns that
// verdict wrapped in ErrUnsupported; nil when ctx ends first. It counts the rejections, so a
// rejection is not missed when a later check passes before this goroutine reads the state.
func (g *Gate) waitRejected(ctx context.Context, seen uint64) error {
	for {
		g.mu.Lock()
		rejected, verdict, next := g.rejected, g.err, g.checks
		g.mu.Unlock()
		if rejected != seen {
			if verdict == nil {
				return ErrUnsupported // a later check passed again; the stop still holds
			}
			return fmt.Errorf("%w: %w", ErrUnsupported, verdict)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-next:
		}
	}
}
