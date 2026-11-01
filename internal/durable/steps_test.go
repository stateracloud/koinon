// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"

	"github.com/stateracloud/koinon/internal/durable"
	_ "github.com/stateracloud/koinon/internal/services"          // declares the run-now and direct-message steps
	_ "github.com/stateracloud/koinon/internal/triggers/schedule" // declares the schedule-fire steps
)

var updateSteps = flag.Bool("update-steps", false, "rewrite testdata/workflow_steps.json (only with a new durable.WorkflowVersion, or when nothing changed)")

const stepsFile = "testdata/workflow_steps.json"

type stepRecord struct {
	Version   string              `json:"version"`
	Workflows map[string][]string `json:"workflows"`
}

// TestWorkflowSteps fails when a registered workflow's step list changes without a bump of
// durable.WorkflowVersion (ADR 0085, decision 11). A workflow pending under the old steps could not
// continue from the middle under the new ones; the bump makes the carry-over sweep fork it from
// step 0 instead. To record an intended change: bump WorkflowVersion, check that every workflow is
// still idempotent from step 0, then run `go test ./internal/durable -run TestWorkflowSteps -update-steps`.
func TestWorkflowSteps(t *testing.T) {
	got := stepRecord{Version: durable.WorkflowVersion, Workflows: durable.DeclaredSteps()}
	raw, err := os.ReadFile(stepsFile)
	if err != nil {
		t.Fatalf("read %s: %v", stepsFile, err)
	}
	var want stepRecord
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse %s: %v", stepsFile, err)
	}
	same := reflect.DeepEqual(got.Workflows, want.Workflows)

	if *updateSteps {
		if !same && got.Version == want.Version {
			t.Fatalf("the step lists changed but durable.WorkflowVersion is still %q: bump it first", got.Version)
		}
		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stepsFile, append(out, '\n'), 0o644); err != nil { //nolint:gosec // a test fixture
			t.Fatal(err)
		}
		return
	}

	switch {
	case !same && got.Version == want.Version:
		t.Errorf("a registered workflow's steps changed but durable.WorkflowVersion is still %q.\n"+
			"Bump it (a pending workflow of the old steps is then carried over from step 0), confirm every workflow is "+
			"idempotent from step 0, and run this test with -update-steps.\nrecorded: %v\ncurrent:  %v",
			got.Version, want.Workflows, got.Workflows)
	case !same || got.Version != want.Version:
		t.Errorf("%s is stale (recorded version %q, code %q): run this test with -update-steps.",
			stepsFile, want.Version, got.Version)
	}
}

// TestFourWorkflowsDeclared pins the workflows that ADR 0085 verified idempotent from step 0. A new
// workflow joins the list only after the same check, and the rule is in durable.WorkflowVersion.
func TestFourWorkflowsDeclared(t *testing.T) {
	declared := durable.DeclaredSteps()
	for _, name := range []string{"koinon.notify", "koinon.trigger-fire", "koinon.trigger-run-now", "koinon.direct-message"} {
		if len(declared[name]) == 0 {
			t.Errorf("workflow %s has no declared steps", name)
		}
	}
	if len(declared) != 4 {
		t.Errorf("%d workflows declare steps, want the 4 verified idempotent from step 0 (ADR 0085): %v", len(declared), declared)
	}
}
