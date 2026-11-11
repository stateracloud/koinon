// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/templates"
)

func TestSpecHashCoversEveryResource(t *testing.T) {
	base := execution.SandboxSpec{
		Image: "img", CPURequest: "250m", CPULimit: "2", MemoryRequest: "1Gi", MemoryLimit: "2Gi",
		Labels: map[string]string{},
	}
	want := specHash(base)
	if specHash(base) != want {
		t.Fatal("the hash is not stable")
	}
	for name, mutate := range map[string]func(*execution.SandboxSpec){
		"cpu request":    func(s *execution.SandboxSpec) { s.CPURequest = "500m" },
		"cpu limit":      func(s *execution.SandboxSpec) { s.CPULimit = "4" },
		"memory request": func(s *execution.SandboxSpec) { s.MemoryRequest = "2Gi" },
		"memory limit":   func(s *execution.SandboxSpec) { s.MemoryLimit = "4Gi" },
	} {
		s := base
		mutate(&s)
		if specHash(s) == want {
			t.Errorf("changing the %s does not change the hash", name)
		}
	}
	// A request and a limit are different fields: swapping them is a change.
	swapped := base
	swapped.CPURequest, swapped.CPULimit = base.CPULimit, base.CPURequest
	if specHash(swapped) == want {
		t.Error("swapping the CPU request and limit does not change the hash")
	}
}

func TestSandboxSpecRendersRequestsAndLimits(t *testing.T) {
	r := &Reconciler{cfg: Config{RuntimeClassName: "kata"}}
	res := templates.Resources{
		CPU:    templates.Quantities{Request: "250m", Limit: "2"},
		Memory: templates.Quantities{Request: "2Gi", Limit: "2Gi"},
	}
	spec, hash := r.sandboxSpec(pgtype.UUID{}, pgtype.UUID{}, execution.AgentNames{Sandbox: "sb"}, Flavor{Image: "img"}, nil, nil, res)
	if spec.CPURequest != "250m" || spec.CPULimit != "2" || spec.MemoryRequest != "2Gi" || spec.MemoryLimit != "2Gi" {
		t.Errorf("resources not carried into the spec: %+v", spec)
	}
	if spec.CPURequest == spec.CPULimit {
		t.Error("the CPU request must not be the limit")
	}
	if spec.Labels[LabelSpecHash] != hash {
		t.Error("the spec hash label is not the returned hash")
	}
}

func TestSandboxResourcesLogsAClampOnce(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	pol := sandboxres.DefaultPolicy()
	r := &Reconciler{cfg: Config{SandboxResources: &pol}, log: log}
	w := &work{
		org:   store.ListOrgsToReconcileRow{Slug: "acme"},
		agent: store.Agent{ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}},
		cfg: agentconfig.Config{Resources: templates.Resources{
			Memory: templates.Quantities{Limit: "64Gi"},
		}},
	}
	for range 3 {
		got := r.sandboxResources(w)
		if got.Memory.Limit != "8Gi" || got.Memory.Request != "8Gi" {
			t.Fatalf("memory = %+v, want 8Gi/8Gi", got.Memory)
		}
	}
	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, out)
	}
	for _, want := range []string{"org=acme", "memory.limit 64Gi -> 8Gi"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestSandboxResourcesAgentOverrideWinsOverTemplate(t *testing.T) {
	pol := sandboxres.DefaultPolicy()
	r := &Reconciler{cfg: Config{SandboxResources: &pol}, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	tpl := templates.Resources{
		CPU:    templates.Quantities{Request: "250m", Limit: "1"},
		Memory: templates.Quantities{Limit: "1Gi"},
	}
	w := &work{
		org: store.ListOrgsToReconcileRow{Slug: "acme"},
		agent: store.Agent{
			ID:                 pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
			SandboxCpuLimit:    pgtype.Text{String: "3", Valid: true},
			SandboxMemoryLimit: pgtype.Text{String: "4Gi", Valid: true},
		},
		cfg: agentconfig.Config{Resources: tpl},
	}
	got := r.sandboxResources(w)
	if got.CPU.Limit != "3" {
		t.Errorf("cpu limit = %q, want the override 3", got.CPU.Limit)
	}
	if got.CPU.Request != "250m" {
		t.Errorf("cpu request = %q, want the template's 250m (the override sets none)", got.CPU.Request)
	}
	if got.Memory.Limit != "4Gi" || got.Memory.Request != "4Gi" {
		t.Errorf("memory = %+v, want 4Gi/4Gi (override limit; the request follows it)", got.Memory)
	}

	// An override left over from before the bounds were lowered is clamped, not trusted.
	w.agent.SandboxCpuLimit = pgtype.Text{String: "64", Valid: true}
	if got := r.sandboxResources(w); got.CPU.Limit != "4" {
		t.Errorf("cpu limit = %q, want it clamped to the maximum 4", got.CPU.Limit)
	}

	// No override: the template again.
	w.agent = store.Agent{ID: w.agent.ID}
	if got := r.sandboxResources(w); got.CPU.Limit != "1" || got.Memory.Limit != "1Gi" {
		t.Errorf("without an override the template applies, got %+v", got)
	}
}
