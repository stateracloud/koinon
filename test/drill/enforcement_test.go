// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"testing"

	"github.com/stateracloud/koinon/test/drill"
)

func TestParseExportEnforcement(t *testing.T) {
	got, err := drill.ParseExportEnforcement("exported {Prefix:/tmp/x Turns:2 Events:9 Decisions:0 Outbound:0 Enforcement:enforce Objects:[run.jsonl]} to /tmp/x\n")
	if err != nil || got != "enforce" {
		t.Fatalf("%q %v", got, err)
	}
	for _, out := range []string{"", "exported {Prefix:x}", "Enforcement: Objects:[]"} {
		if _, err := drill.ParseExportEnforcement(out); err == nil {
			t.Fatalf("%q must not parse", out)
		}
	}
}

func TestEnforcementPassesOnlyWhenEveryRunEnforces(t *testing.T) {
	if err := drill.VerifyEnforcement(map[string]string{"a": "enforce", "b": "enforce"}); err != nil {
		t.Fatal(err)
	}
	if err := drill.VerifyEnforcement(nil); err == nil {
		t.Fatal("zero runs must fail")
	}
	for _, bad := range []string{"unknown", "audit", "none", ""} {
		if err := drill.VerifyEnforcement(map[string]string{"a": "enforce", "b": bad}); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestAnEnforcementFailureFailsTheDrill(t *testing.T) {
	r := drill.Result{Checks: []drill.Check{{Name: "enforcement", Verdict: drill.Fail}}}
	r.Finish()
	if r.Verdict != drill.Fail {
		t.Fatalf("%+v", r)
	}
}
