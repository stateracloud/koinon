// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Verdict is what an object's status says about it.
type Verdict int

const (
	// Pending: the controller has not reported on the current generation yet.
	Pending Verdict = iota
	// Accepted: the controller accepted the object and, where the kind has them, attached it and
	// programmed it.
	Accepted
	// Refused: the controller refused the object, or part of it.
	Refused
)

// Status is the controller's answer for one object.
type Status struct {
	Verdict Verdict
	// Reason holds the controller's messages for a Refused object.
	Reason string
}

// requiredConditions are the condition types that must be True, by kind. ConfigMaps have no status:
// the policy that selects them reports a bad entry (as PartiallyValid).
var requiredConditions = map[string][]string{
	KindGateway:    {"Accepted", "Programmed"},
	KindPolicy:     {"Accepted", "Attached"},
	KindModel:      {"Accepted", "ResolvedRefs"},
	KindHTTPRoute:  {"Accepted", "ResolvedRefs"},
	KindBackend:    {"Accepted"},
	KindParameters: nil,
}

// Evaluate reads the status of o that agentgateway's controller wrote for o's current generation.
// Conditions are found in status.conditions (Gateway, Backend), status.ancestors[] (Policy) and
// status.parents[] (Model, HTTPRoute); entries of other controllers are ignored.
func Evaluate(o *unstructured.Unstructured) Status {
	need, known := requiredConditions[o.GetKind()]
	if !known || len(need) == 0 {
		return Status{Verdict: Accepted}
	}
	groups := conditionGroups(o)
	if len(groups) == 0 {
		return Status{Verdict: Pending}
	}
	gen := o.GetGeneration()
	var reasons []string
	pending := false
	for _, conds := range groups {
		for _, typ := range need {
			c, ok := find(conds, typ)
			if !ok || observed(c) < gen {
				pending = true
				continue
			}
			if c["status"] != "True" {
				reasons = append(reasons, fmt.Sprintf("%s=%v (%v): %v", typ, c["status"], c["reason"], c["message"]))
				continue
			}
			// Accepted with a reason of PartiallyValid: the object is live except for what the
			// message lists (a policy naming a bad key entry).
			if typ == "Accepted" && c["reason"] == "PartiallyValid" {
				reasons = append(reasons, fmt.Sprint(c["message"]))
			}
		}
	}
	switch {
	case len(reasons) > 0:
		return Status{Verdict: Refused, Reason: strings.Join(reasons, "; ")}
	case pending:
		return Status{Verdict: Pending}
	}
	return Status{Verdict: Accepted}
}

// conditionGroups returns each controller-owned condition list of o.
func conditionGroups(o *unstructured.Unstructured) [][]map[string]any {
	var out [][]map[string]any
	if conds, ok, _ := unstructured.NestedSlice(o.Object, "status", "conditions"); ok {
		out = append(out, toMaps(conds))
	}
	for _, field := range []string{"ancestors", "parents"} {
		entries, _, _ := unstructured.NestedSlice(o.Object, "status", field)
		for _, e := range entries {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if c, _ := m["controllerName"].(string); c != ControllerName {
				continue
			}
			conds, _ := m["conditions"].([]any)
			out = append(out, toMaps(conds))
		}
	}
	return out
}

func toMaps(in []any) []map[string]any {
	var out []map[string]any
	for _, c := range in {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func find(conds []map[string]any, typ string) (map[string]any, bool) {
	for _, c := range conds {
		if c["type"] == typ {
			return c, true
		}
	}
	return nil, false
}

func observed(c map[string]any) int64 {
	switch v := c["observedGeneration"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}
