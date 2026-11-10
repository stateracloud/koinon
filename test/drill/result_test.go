// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/test/drill"
)

func TestRTOAndRPOAreMeasuredFromTheDisaster(t *testing.T) {
	d := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	m := drill.Marks{
		DumpStart: d.Add(-30 * time.Second), DumpEnd: d.Add(-20 * time.Second), Disaster: d,
		TargetInstalled: d.Add(120 * time.Second), Restored: d.Add(150 * time.Second),
		HeldChecksDone: d.Add(270 * time.Second), Released: d.Add(290 * time.Second),
		FirstRunDone: d.Add(330 * time.Second), QueueDrained: d.Add(500 * time.Second),
		AllChecksPassed: d.Add(520 * time.Second), LatestMarker: d.Add(-22 * time.Second),
	}
	got := m.Compute()
	if got.RTOSeconds != 520 || got.RPOSeconds != 22 {
		t.Fatalf("RTO %v RPO %v, want 520 and 22", got.RTOSeconds, got.RPOSeconds)
	}
	for name, want := range map[string]float64{"backup": 10, "targetInstall": 120, "restore": 30, "heldChecks": 120, "release": 20, "firstRun": 40, "queueDrained": 210} {
		if got.Phases[name] != want {
			t.Errorf("phase %s = %v, want %v", name, got.Phases[name], want)
		}
	}
}

func TestAnUnfinishedRunReportsWhatItMeasured(t *testing.T) {
	d := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	got := drill.Marks{DumpStart: d, DumpEnd: d.Add(time.Second)}.Compute()
	if got.RTOSeconds != 0 || got.RPOSeconds != 0 || got.Phases["backup"] != 1 {
		t.Fatalf("%+v", got)
	}
	if _, ok := got.Phases["restore"]; ok {
		t.Fatal("a phase without both ends must be left out")
	}
}

func TestVerdictPassesOnKnownGapsAndFailsOnAnythingElse(t *testing.T) {
	r := drill.Result{Checks: []drill.Check{
		{Name: "signin", Verdict: drill.Pass},
		{Name: "quarantine", Verdict: drill.KnownGap, Issue: "#516"},
	}}
	r.Finish()
	if r.Verdict != drill.Pass || len(r.KnownGaps) != 1 {
		t.Fatalf("%+v", r)
	}
	r.Checks = append(r.Checks, drill.Check{Name: "schedule", Verdict: drill.Fail})
	r.Finish()
	if r.Verdict != drill.Fail {
		t.Fatalf("a failed check must fail the drill: %+v", r)
	}
	r.Checks[2].Verdict = drill.NotRun
	r.Finish()
	if r.Verdict != drill.Fail {
		t.Fatal("a check that never ran is not a pass")
	}
}

func TestClassifyFailureNeedsTheLabelAndTheMessage(t *testing.T) {
	gap := drill.GapMessage("#516", "x")
	if v, i := drill.ClassifyFailure([]string{"known-gap", "importer"}, gap); v != drill.KnownGap || i != "#516" {
		t.Fatalf("%s %s", v, i)
	}
	if v, _ := drill.ClassifyFailure([]string{"known-gap", "importer"}, "port-forward: connection refused"); v != drill.Fail {
		t.Fatal("a labelled spec that fails on something unrelated is a fail")
	}
	if v, _ := drill.ClassifyFailure([]string{"signin"}, gap); v != drill.Fail {
		t.Fatal("a gap message without the label is a fail")
	}
}

func TestResultFileIsWritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "drill-result.json")
	r := drill.Result{RunID: "a1b2c3", Checks: []drill.Check{{Name: "signin", Verdict: drill.Pass}}}
	r.Finish()
	if err := r.Write(p); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || len(b) == 0 {
		t.Fatalf("%v %d", err, len(b))
	}
}

func TestKindTopologyIsValid(t *testing.T) {
	top, err := drill.LoadTopology("topology/kind.yaml", "a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	if top.Source.Namespace != "koinon-drill-src-a1b2c3" || top.Target.GatewayNamespace != "koinon-drill-os-dst-a1b2c3" {
		t.Fatalf("%+v", top)
	}
}

func TestTopologyRefusesAForeignNamespace(t *testing.T) {
	top, err := drill.LoadTopology("topology/kind.yaml", "a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	top.Target.Namespace = "koinon"
	if top.Validate() == nil {
		t.Fatal("the guide's own namespace must be refused")
	}
	top, _ = drill.LoadTopology("topology/kind.yaml", "a1b2c3")
	top.Target.GatewayNamespace = top.Source.GatewayNamespace
	if top.Validate() == nil {
		t.Fatal("a shared gateway must be refused")
	}
	top, _ = drill.LoadTopology("topology/kind.yaml", "a1b2c3")
	top.Target.GatewayID = top.Source.GatewayID
	if top.Validate() == nil {
		t.Fatal("a shared gateway ID must be refused")
	}
	top, _ = drill.LoadTopology("topology/kind.yaml", "a1b2c3")
	top.Target.GatewayID = "openshell"
	if top.Validate() == nil {
		t.Fatal("the default gateway ID must be refused")
	}
	top, _ = drill.LoadTopology("topology/kind.yaml", "a1b2c3")
	top.Target.Release = "prod"
	if top.Validate() == nil {
		t.Fatal("a release without koinon must be refused")
	}
}

func TestDiffObjects(t *testing.T) {
	before := []drill.Object{{Kind: "sandbox", ID: "1", Name: "a", ResourceVersion: 3}, {Kind: "provider", ID: "2", Name: "p", ResourceVersion: 1}}
	if d := drill.DiffObjects(before, before); len(d) != 0 {
		t.Fatalf("unchanged: %v", d)
	}
	after := []drill.Object{{Kind: "sandbox", ID: "1", Name: "a", ResourceVersion: 4}, {Kind: "provider", ID: "9", Name: "q", ResourceVersion: 1}}
	if d := drill.DiffObjects(before, after); len(d) != 3 {
		t.Fatalf("want a changed, a gone and a new object: %v", d)
	}
}

func TestFakeModelManifestIsLabelledAndParses(t *testing.T) {
	m := drill.FakeModelManifest("koinon-drill-fakes-a1b2c3", "a1b2c3", "kind.local/koinon-fakemodel:e2e", "model: drill\nturns:\n  - text: x\n")
	docs := strings.Split(m, "\n---\n")
	if len(docs) != 4 {
		t.Fatalf("want 4 documents, got %d", len(docs))
	}
	for i, d := range docs {
		var v struct {
			Metadata struct {
				Name   string
				Labels map[string]string
			}
		}
		if err := yaml.Unmarshal([]byte(d), &v); err != nil {
			t.Fatalf("document %d: %v", i, err)
		}
		if v.Metadata.Labels[drill.RunLabel] != "a1b2c3" {
			t.Errorf("document %d (%s) has no run label", i, v.Metadata.Name)
		}
	}
}

func TestValuesOverlayHoldsAndNamesTheGateway(t *testing.T) {
	gw := drill.Gateway{Namespace: "koinon-drill-os-dst-a1b2c3", Service: "openshell-dst-a1b2c3", ClientTLSSecret: "openshell-dst-a1b2c3-client-tls"}
	s, err := drill.ValuesOverlay("koinon-drill-fakes-a1b2c3", gw, "drill-dst-a1b2c3", true, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"restoreHold: true", "openshell-dst-a1b2c3.koinon-drill-os-dst-a1b2c3.svc:8080", "fakemodel.koinon-drill-fakes-a1b2c3.svc:8080", "openshell-dst-a1b2c3-client-tls", "gatewayId: drill-dst-a1b2c3"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestFakeModelScriptParses(t *testing.T) {
	b, err := os.ReadFile("fakemodel/drill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := fakemodel.ParseScript(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.Model != drill.ModelName {
		t.Fatalf("model %q, want %q", s.Model, drill.ModelName)
	}
}
