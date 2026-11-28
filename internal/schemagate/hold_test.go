// SPDX-License-Identifier: Apache-2.0

package schemagate_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/schemagate"
)

// fakeSchema is a schema reader the test changes between checks.
type fakeSchema struct {
	mu  sync.Mutex
	s   schemagate.Schema
	err error
}

func (f *fakeSchema) set(s schemagate.Schema, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s, f.err = s, err
}

func (f *fakeSchema) read(context.Context) (schemagate.Schema, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, f.err
}

func newHoldGate(f *fakeSchema) *schemagate.Gate {
	return schemagate.NewFunc(f.read, schemagate.Range{Min: 10, Latest: 12}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// startHold starts g.Hold with a work function that reports its start and then blocks until its
// context ends or release is closed.
func startHold(ctx context.Context, g *schemagate.Gate, release <-chan struct{}) (started <-chan struct{}, done <-chan error) {
	s, d := make(chan struct{}), make(chan error, 1)
	go func() {
		d <- g.Hold(ctx, func(wctx context.Context) error {
			close(s)
			select {
			case <-wctx.Done():
				return wctx.Err()
			case <-release:
				return nil
			}
		})
	}()
	return s, d
}

const holdWait = 5 * time.Second

func TestHoldWaitsForASupportedSchema(t *testing.T) {
	f := &fakeSchema{s: schemagate.Schema{Version: 9}} // the migration Job has not run yet
	g := newHoldGate(f)
	release := make(chan struct{})
	started, done := startHold(t.Context(), g, release)

	_ = g.Check(t.Context())
	f.set(schemagate.Schema{}, errors.New("connection refused"))
	_ = g.Check(t.Context())
	select {
	case <-started:
		t.Fatal("work started while the schema was older than the build needs, or unreadable")
	case <-time.After(50 * time.Millisecond):
	}

	f.set(schemagate.Schema{Version: 12}, nil) // the Job ran
	_ = g.Check(t.Context())
	select {
	case <-started:
	case <-time.After(holdWait):
		t.Fatal("work did not start once the schema was supported")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Hold must return work's result, got %v", err)
	}
}

func TestHoldStopsWorkWhenTheSchemaTurnsUnsupported(t *testing.T) {
	f := &fakeSchema{s: schemagate.Schema{Version: 12}}
	g := newHoldGate(f)
	_ = g.Check(t.Context())
	started, done := startHold(t.Context(), g, make(chan struct{}))
	<-started

	// A short database outage is not a verdict: work goes on.
	f.set(schemagate.Schema{}, errors.New("connection refused"))
	_ = g.Check(t.Context())
	f.set(schemagate.Schema{Version: 12}, nil)
	_ = g.Check(t.Context())
	select {
	case err := <-done:
		t.Fatalf("an unreadable schema stopped the work: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A newer release ran a breaking migration (an app-only rollback).
	f.set(schemagate.Schema{Version: 14, MinReader: 13}, nil)
	_ = g.Check(t.Context())
	select {
	case err := <-done:
		if !errors.Is(err, schemagate.ErrUnsupported) || !strings.Contains(err.Error(), "knows up to 12") {
			t.Fatalf("Hold must return ErrUnsupported with the verdict, got %v", err)
		}
	case <-time.After(holdWait):
		t.Fatal("work was not stopped when the schema became unsupported")
	}
}

func TestHoldDoesNotMissAShortRejection(t *testing.T) {
	f := &fakeSchema{s: schemagate.Schema{Version: 12}}
	g := newHoldGate(f)
	_ = g.Check(t.Context())
	started, done := startHold(t.Context(), g, make(chan struct{}))
	<-started

	f.set(schemagate.Schema{Version: 9}, nil) // restored from an older backup
	_ = g.Check(t.Context())
	f.set(schemagate.Schema{Version: 12}, nil) // migrated again before the watcher looked
	_ = g.Check(t.Context())
	select {
	case err := <-done:
		if !errors.Is(err, schemagate.ErrUnsupported) {
			t.Fatalf("Hold must return ErrUnsupported, got %v", err)
		}
	case <-time.After(holdWait):
		t.Fatal("a rejection followed by a pass did not stop the work")
	}
}

func TestHoldEndsWithItsContextWhileItWaits(t *testing.T) {
	g := newHoldGate(&fakeSchema{s: schemagate.Schema{Version: 9}})
	ctx, cancel := context.WithCancel(t.Context())
	started, done := startHold(ctx, g, make(chan struct{}))
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(holdWait):
		t.Fatal("Hold did not return when its context ended")
	}
	select {
	case <-started:
		t.Fatal("work started although no check passed")
	default:
	}
}

func TestHoldReturnsWorksError(t *testing.T) {
	g := newHoldGate(&fakeSchema{s: schemagate.Schema{Version: 12}})
	_ = g.Check(t.Context())
	boom := errors.New("boom")
	if err := g.Hold(t.Context(), func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v, want work's error", err)
	}
}
