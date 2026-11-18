// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/executiontest"
)

// wakingBackend wakes a sandbox right after it stopped it, as a router request that was on the wire
// when Substrate suspended the actor does (ADR 0119, "Router resume").
type wakingBackend struct {
	execution.Backend
	f     *executiontest.Fake
	wake  bool
	stops int
}

func (w *wakingBackend) StopSandbox(ctx context.Context, ws, name string) (execution.Sandbox, error) {
	w.stops++
	sb, err := w.Backend.StopSandbox(ctx, ws, name)
	if err == nil && w.wake {
		_, err = w.f.StartSandbox(ctx, ws, name)
	}
	return sb, err
}

func newWaking(t *testing.T, wake bool) (*Reconciler, *wakingBackend) {
	t.Helper()
	ctx := context.Background()
	f := executiontest.NewFake()
	if _, err := f.EnsureWorkspace(ctx, "ws", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateSandbox(ctx, "ws", execution.SandboxSpec{Name: "sb", Image: "img", Command: []string{"cat"}}); err != nil {
		t.Fatal(err)
	}
	b := &wakingBackend{Backend: execution.WithoutEgress(f), f: f, wake: wake}
	return &Reconciler{backend: b, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}, b
}

func TestStopSandboxReportsWhatIsTrueAfterTheStop(t *testing.T) {
	ctx := context.Background()
	r, _ := newWaking(t, false)
	sb, err := r.stopSandbox(ctx, "ws", "sb")
	if err != nil || !sb.Phase.IsStopped() {
		t.Fatalf("a stop that held: phase %s, err %v", sb.Phase, err)
	}

	r, b := newWaking(t, true)
	sb, err = r.stopSandbox(ctx, "ws", "sb")
	if err != nil || sb.Phase != execution.PhaseReady {
		t.Fatalf("a woken sandbox must be recorded as running, got %s, err %v", sb.Phase, err)
	}
	if b.stops != 1 {
		t.Fatalf("one stop per pass, got %d", b.stops)
	}
	// The next pass sees a running sandbox and stops it again.
	if _, err = r.stopSandbox(ctx, "ws", "sb"); err != nil || b.stops != 2 {
		t.Fatalf("next pass: stops %d, err %v", b.stops, err)
	}
}

func TestWithoutEgressIsANoOp(t *testing.T) {
	b := execution.WithoutEgress(executiontest.NewFake())
	ctx := context.Background()
	if err := b.EnsureProvider(ctx, "ws", execution.Provider{}); err != nil {
		t.Fatal(err)
	}
	if v, err := b.EnsurePolicy(ctx, "ws", "sb", execution.Policy{}); err != nil || v != 0 {
		t.Fatalf("policy version %d, err %v", v, err)
	}
	if err := b.DeleteProvider(ctx, "ws", "p"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteProviderProfile(ctx, "ws", "p"); err != nil {
		t.Fatal(err)
	}
}
