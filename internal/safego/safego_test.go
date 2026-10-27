// SPDX-License-Identifier: Apache-2.0

package safego_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/safego"
)

// lockedBuffer is a log sink the test reads while the goroutine may still write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestGoRecoversAndLogsTheStack(t *testing.T) {
	var buf lockedBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	done := make(chan struct{})
	safego.Go(context.Background(), log, "boom", func(context.Context) {
		defer close(done)
		panic("kaboom")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the goroutine did not run")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "kaboom") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	out := buf.String()
	for _, want := range []string{"goroutine panicked", "goroutine=boom", "kaboom", "safego_test.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestGoPassesTheContext(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	got := make(chan any, 1)
	safego.Go(ctx, nil, "ctx", func(ctx context.Context) { got <- ctx.Value(key{}) })
	if v := <-got; v != "v" {
		t.Fatalf("context value = %v", v)
	}
}

func TestDoReportsAPanicAndALoopGoesOn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	runs := 0
	for i := range 3 {
		panicked := safego.Do(log, "iter", func() {
			runs++
			if i == 1 {
				panic("second")
			}
		})
		if panicked != (i == 1) {
			t.Fatalf("iteration %d: panicked = %v", i, panicked)
		}
	}
	if runs != 3 {
		t.Fatalf("runs = %d", runs)
	}
}

func TestGoOrStopCallsOnPanicOnlyWhenFnPanics(t *testing.T) {
	stopped := make(chan struct{}, 2)
	safego.GoOrStop(context.Background(), nil, "ok", func() { stopped <- struct{}{} }, func(context.Context) {})
	select {
	case <-stopped:
		t.Fatal("onPanic ran without a panic")
	case <-time.After(100 * time.Millisecond):
	}
	safego.GoOrStop(context.Background(), nil, "boom", func() { stopped <- struct{}{} }, func(context.Context) { panic("kaboom") })
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("onPanic did not run after a panic")
	}
}
