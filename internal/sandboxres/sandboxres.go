// SPDX-License-Identifier: Apache-2.0

// Package sandboxres decides the CPU and memory requests and limits of an agent's sandbox (ADR
// 0083): the template's values, else the platform default, clamped to the platform bounds.
package sandboxres

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stateracloud/koinon/templates"
)

// Range bounds one value. An empty Min or Max is unbounded on that side.
type Range struct {
	Min, Max string
}

// Bounds are the platform's range for each of the four values.
type Bounds struct {
	CPURequest, CPULimit       Range
	MemoryRequest, MemoryLimit Range
}

// Policy is the platform's sandbox sizing: what an agent gets when its template says nothing, the
// range no agent may leave, and whether the memory request is raised to the limit.
type Policy struct {
	Default templates.Resources
	Bounds  Bounds
	// MemoryRequestEqualsLimit makes the memory request equal the memory limit. The default
	// runtime class is Kata, whose guest never returns memory to the host, so a request below the
	// limit would let the node be over-committed on memory that cannot be reclaimed (ADR 0083).
	MemoryRequestEqualsLimit bool
}

// DefaultPolicy is the policy of an install whose config sets none; the chart's values.yaml
// carries the same numbers.
func DefaultPolicy() Policy {
	return Policy{
		Default: templates.Resources{
			CPU:    templates.Quantities{Request: "250m", Limit: "2"},
			Memory: templates.Quantities{Request: "1Gi", Limit: "2Gi"},
		},
		Bounds: Bounds{
			CPURequest:    Range{Min: "100m", Max: "2"},
			CPULimit:      Range{Max: "4"},
			MemoryRequest: Range{Min: "512Mi", Max: "8Gi"},
			MemoryLimit:   Range{Min: "512Mi", Max: "8Gi"},
		},
		MemoryRequestEqualsLimit: true,
	}
}

// Validate checks that every quantity parses, that each range has Min <= Max, and that the
// default is a valid pair of requests and limits.
func (p Policy) Validate() error {
	if err := p.Default.Validate(); err != nil {
		return fmt.Errorf("default: %w", err)
	}
	for _, r := range []struct {
		name string
		rng  Range
	}{
		{"cpuRequest", p.Bounds.CPURequest}, {"cpuLimit", p.Bounds.CPULimit},
		{"memoryRequest", p.Bounds.MemoryRequest}, {"memoryLimit", p.Bounds.MemoryLimit},
	} {
		lo, err := parse(r.rng.Min)
		if err != nil {
			return fmt.Errorf("bounds.%s.min %q: %w", r.name, r.rng.Min, err)
		}
		hi, err := parse(r.rng.Max)
		if err != nil {
			return fmt.Errorf("bounds.%s.max %q: %w", r.name, r.rng.Max, err)
		}
		if lo != nil && hi != nil && lo.Cmp(*hi) > 0 {
			return fmt.Errorf("bounds.%s: min %s is greater than max %s", r.name, r.rng.Min, r.rng.Max)
		}
	}
	return nil
}

func parse(s string) (*resource.Quantity, error) {
	if s == "" {
		return nil, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// Clamp records one value that Effective changed.
type Clamp struct {
	// Field is "cpu.request", "cpu.limit", "memory.request" or "memory.limit".
	Field string
	// From is the value before, To the value after.
	From, To string
	// Reason is "below the minimum", "above the maximum", "above the limit" or "raised to the limit".
	Reason string
}

func (c Clamp) String() string {
	return fmt.Sprintf("%s %s -> %s (%s)", c.Field, c.From, c.To, c.Reason)
}

// Effective returns the resources a sandbox gets for a template's values, and what the bounds
// changed. Unset template values take the policy default; each of the four is then clamped to its
// bounds; the memory request becomes the limit if the policy says so; and a request above its
// limit is lowered to the limit. The policy must be valid.
func (p Policy) Effective(tpl templates.Resources) (templates.Resources, []Clamp) {
	var clamps []Clamp
	pick := func(t, d string) string {
		if t != "" {
			return t
		}
		return d
	}
	out := templates.Resources{
		CPU: templates.Quantities{
			Request: pick(tpl.CPU.Request, p.Default.CPU.Request),
			Limit:   pick(tpl.CPU.Limit, p.Default.CPU.Limit),
		},
		Memory: templates.Quantities{
			Request: pick(tpl.Memory.Request, p.Default.Memory.Request),
			Limit:   pick(tpl.Memory.Limit, p.Default.Memory.Limit),
		},
	}
	clampTo := func(field string, v *string, r Range) {
		nv, reason := clamp(*v, r)
		if reason != "" {
			clamps = append(clamps, Clamp{Field: field, From: *v, To: nv, Reason: reason})
			*v = nv
		}
	}
	clampTo("cpu.request", &out.CPU.Request, p.Bounds.CPURequest)
	clampTo("cpu.limit", &out.CPU.Limit, p.Bounds.CPULimit)
	clampTo("memory.request", &out.Memory.Request, p.Bounds.MemoryRequest)
	clampTo("memory.limit", &out.Memory.Limit, p.Bounds.MemoryLimit)

	if p.MemoryRequestEqualsLimit && out.Memory.Limit != "" && out.Memory.Request != out.Memory.Limit {
		// Only a request the template set is worth a warning: the platform default follows the
		// limit by design, and a respelling of an equal value is no change.
		if tpl.Memory.Request != "" && !equalQuantity(out.Memory.Request, out.Memory.Limit) {
			clamps = append(clamps, Clamp{"memory.request", out.Memory.Request, out.Memory.Limit, "raised to the limit"})
		}
		out.Memory.Request = out.Memory.Limit
	}
	for _, q := range []struct {
		field string
		q     *templates.Quantities
	}{{"cpu.request", &out.CPU}, {"memory.request", &out.Memory}} {
		if q.q.Request == "" || q.q.Limit == "" {
			continue
		}
		req, _ := parse(q.q.Request)
		lim, _ := parse(q.q.Limit)
		if req.Cmp(*lim) > 0 {
			clamps = append(clamps, Clamp{q.field, q.q.Request, q.q.Limit, "above the limit"})
			q.q.Request = q.q.Limit
		}
	}
	return out, clamps
}

func equalQuantity(a, b string) bool {
	qa, _ := parse(a)
	qb, _ := parse(b)
	return qa != nil && qb != nil && qa.Cmp(*qb) == 0
}

// clamp moves v into r. It returns the new value and why it moved, or v and "" when it did not.
// An empty or unparsable v is returned unchanged.
func clamp(v string, r Range) (string, string) {
	q, err := parse(v)
	if err != nil || q == nil {
		return v, ""
	}
	if lo, _ := parse(r.Min); lo != nil && q.Cmp(*lo) < 0 {
		return r.Min, "below the minimum"
	}
	if hi, _ := parse(r.Max); hi != nil && q.Cmp(*hi) > 0 {
		return r.Max, "above the maximum"
	}
	return v, ""
}
