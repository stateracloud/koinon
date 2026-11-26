// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"os"
	"sort"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/catalog"
	refusalv1 "github.com/stateracloud/koinon/internal/gen/koinon/refusal/v1"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/pricing"
)

// protoReasons is the names of the enum koinon.refusal.v1.Reason, without the zero value (ADR 0116 decision 5).
func protoReasons() []string {
	var out []string
	vals := refusalv1.Reason(0).Descriptor().Values()
	for i := range vals.Len() {
		if v := vals.Get(i); v.Number() != 0 {
			out = append(out, string(v.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// The proto enum, the OpenAPI list and the Go code carry the same reasons. Each of the three fails this
// spec when it changes alone.
func TestReasonsAreEqualInProtoOpenAPIAndGo(t *testing.T) {
	raw, err := os.ReadFile("../../api/admin/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	openapi := append([]string(nil), doc.Components.Schemas["ErrorReason"].Enum...)
	sort.Strings(openapi)
	proto := protoReasons()

	if len(proto) == 0 {
		t.Fatal("the proto enum has no reasons")
	}
	if !equal(proto, openapi) {
		t.Fatalf("proto enum %v, OpenAPI ErrorReason %v", proto, openapi)
	}
	// Go: the generated REST type knows every proto name, and the proto enum knows every REST value.
	for _, name := range proto {
		if !adminapi.ErrorReason(name).Valid() {
			t.Errorf("the generated Go type has no ErrorReason %q", name)
		}
	}
	// Go string reasons (the catalog and MCP validation errors carry them as strings) must be in the enum.
	for _, name := range []string{mcpservers.ReasonCredentialTooShort} {
		if n, ok := refusalv1.Reason_value[name]; !ok || n == 0 {
			t.Errorf("the Go reason %q is not a value of the proto enum", name)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newServer() *adminapi.Server {
	return adminapi.New(nil, &catalog.Service{}, &tokens.Service{}, nil, pricing.Embedded())
}

// Old behaviour: a REST error had a code and a message only. New behaviour: it also has a reason, the kind
// reason when there is no other, so a client can switch on it.
func TestRestErrorCarriesTheKindReason(t *testing.T) {
	s := newServer()
	_, inv := adminapi.Problem(s, &catalog.InvalidError{Msg: "bad name"})
	if inv.Error.Reason == nil || *inv.Error.Reason != adminapi.INVALID {
		t.Fatalf("invalid request reason = %v", inv.Error.Reason)
	}
	_, nf := adminapi.Problem(s, catalog.ErrNotFound)
	if nf.Error.Reason == nil || *nf.Error.Reason != adminapi.NOTFOUND {
		t.Fatalf("not found reason = %v", nf.Error.Reason)
	}
	_, held := adminapi.Problem(s, restorehold.ErrHeld)
	if held.Error.Reason == nil || *held.Error.Reason != adminapi.INSTALLHELD {
		t.Fatalf("held reason = %v", held.Error.Reason)
	}
}

// A validation error that names its own reason keeps it; a name that is not in the list is dropped, so the
// REST API never sends a reason outside the shared list.
func TestRestErrorKeepsASpecificReasonOnlyFromTheList(t *testing.T) {
	s := newServer()
	_, e := adminapi.Problem(s, &catalog.InvalidError{Msg: "too short", Reason: mcpservers.ReasonCredentialTooShort})
	if e.Error.Reason == nil || *e.Error.Reason != adminapi.CREDENTIALTOOSHORT {
		t.Fatalf("reason = %v", e.Error.Reason)
	}
	_, e = adminapi.Problem(s, &catalog.InvalidError{Msg: "odd", Reason: "NOT_A_REASON"})
	if e.Error.Reason == nil || *e.Error.Reason != adminapi.INVALID {
		t.Fatalf("an unknown reason must fall back to the kind reason, got %v", e.Error.Reason)
	}
}

// Old behaviour stays where no client acts on a reason: an error of another kind has none.
func TestRestErrorWithoutAClientReasonHasNone(t *testing.T) {
	_, e := adminapi.Problem(newServer(), outward.ErrDisabled)
	if e.Error.Reason != nil {
		t.Fatalf("reason = %v", *e.Error.Reason)
	}
}
