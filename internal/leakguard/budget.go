// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Reason says why a check allowed or blocked. The set is fixed, so it can be a metric label
// (ADR 0023); it never carries an org, agent, run or call identifier.
type Reason string

// Reasons a check returns.
const (
	ReasonPass    Reason = "pass"    // allowed: no rule matched
	ReasonTrusted Reason = "trusted" // allowed: the host is on the org's fetch-trusted list

	ReasonTaint Reason = "taint" // the arguments carry tainted text

	ReasonURLInvalid     Reason = "url_invalid"
	ReasonURLScheme      Reason = "url_scheme"
	ReasonURLUserinfo    Reason = "url_userinfo"
	ReasonURLIPLiteral   Reason = "url_ip_literal"
	ReasonURLPort        Reason = "url_port"
	ReasonURLLength      Reason = "url_length"
	ReasonURLQueryLength Reason = "url_query_length"
	ReasonURLEncoded     Reason = "url_encoded_data"
	ReasonURLOpaque      Reason = "url_opaque_component"
	ReasonURLRandom      Reason = "url_random_component"
	ReasonURLHostBudget  Reason = "url_host_budget"  // too many distinct untrusted hosts in the run
	ReasonURLBytesBudget Reason = "url_bytes_budget" // too many path and query bytes to one host

	ReasonSearchLength Reason = "search_length"
	ReasonSearchOpaque Reason = "search_opaque_component"
	ReasonSearchRandom Reason = "search_random_component"
	ReasonSearchBudget Reason = "search_budget"

	ReasonBudgetArgs     Reason = "budget_args"
	ReasonBudgetInput    Reason = "budget_input_size"
	ReasonBudgetWork     Reason = "budget_work"
	ReasonBudgetTime     Reason = "budget_time"
	ReasonDecodeDepth    Reason = "budget_decode_depth"
	ReasonBudgetVariants Reason = "budget_variants"

	ReasonStoreError Reason = "store_error" // the taint store failed: fail closed
)

// Decision is the result of a check. A blocking decision always has a Reason other than
// ReasonPass and ReasonTrusted, and a Detail an agent can act on. Detail never quotes the
// arguments; Match (taint only) holds the text that would leave, for the approval card.
type Decision struct {
	Allow  bool
	Reason Reason
	Detail string
	Match  *Match
	// Evicted reports that the taint store has forgotten some of the scope's private text to
	// stay within its size limits, so a pass may miss it. It does not change Allow; what to do
	// about it is the caller's choice (old issue 349).
	Evicted bool
}

func allow(r Reason) Decision { return Decision{Allow: true, Reason: r} }

func block(r Reason, format string, a ...any) Decision {
	return Decision{Reason: r, Detail: fmt.Sprintf(format, a...)}
}

// Limits bound the work of one taint check. The zero value of a field means its default.
type Limits struct {
	MaxArgs       int           // string arguments per call (default 1024)
	MaxInputBytes int           // total bytes of the arguments (default 1 MiB)
	MaxDepth      int           // decode nesting (default 3)
	MaxVariants   int           // decoded variants per text (default 64)
	MaxWorkBytes  int           // bytes decoded and normalised per call (default 32 MiB)
	MaxDuration   time.Duration // wall clock backstop per call (default 5 s; the work budget decides)
	JoinEndBytes  int           // bytes of each end of a long argument in the call-order join (default 512)
}

// DefaultLimits returns the default budgets.
func DefaultLimits() Limits {
	return Limits{
		MaxArgs:       1024,
		MaxInputBytes: 1 << 20,
		MaxDepth:      3,
		MaxVariants:   64,
		MaxWorkBytes:  32 << 20,
		MaxDuration:   5 * time.Second,
		JoinEndBytes:  512,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxArgs <= 0 {
		l.MaxArgs = d.MaxArgs
	}
	if l.MaxInputBytes <= 0 {
		l.MaxInputBytes = d.MaxInputBytes
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxVariants <= 0 {
		l.MaxVariants = d.MaxVariants
	}
	if l.MaxWorkBytes <= 0 {
		l.MaxWorkBytes = d.MaxWorkBytes
	}
	if l.MaxDuration <= 0 {
		l.MaxDuration = d.MaxDuration
	}
	if l.JoinEndBytes <= 0 {
		l.JoinEndBytes = d.JoinEndBytes
	}
	return l
}

// budgetError is a budget that ran out; it becomes a blocking Decision.
type budgetError struct {
	reason Reason
	detail string
}

func (e *budgetError) Error() string { return string(e.reason) + ": " + e.detail }

func (e *budgetError) decision() Decision {
	return Decision{Reason: e.reason, Detail: e.detail}
}

// meter counts the work of one check against the work and time budgets. It is safe for the
// workers that decode a call's texts side by side.
type meter struct {
	ctx      context.Context
	used     atomic.Int64
	max      int64
	deadline time.Time
	next     atomic.Int64 // used value at which the clock is read next
}

const meterClockEvery = 64 << 10

func newMeter(ctx context.Context, l Limits) *meter {
	return &meter{ctx: ctx, max: int64(l.MaxWorkBytes), deadline: time.Now().Add(l.MaxDuration)}
}

// add charges n bytes of work and fails when a budget is spent or the context is done.
func (m *meter) add(n int) error {
	used := m.used.Add(int64(n))
	if used > m.max {
		return &budgetError{ReasonBudgetWork, fmt.Sprintf("the check needed more than %d bytes of decoding work; the tool call could not be checked", m.max)}
	}
	if next := m.next.Load(); used >= next && m.next.CompareAndSwap(next, used+meterClockEvery) {
		return m.clock()
	}
	return nil
}

func (m *meter) clock() error {
	if err := m.ctx.Err(); err != nil {
		return &budgetError{ReasonBudgetTime, "the check was cancelled before it finished; the tool call could not be checked"}
	}
	if time.Now().After(m.deadline) {
		return &budgetError{ReasonBudgetTime, "the check ran out of time; the tool call could not be checked"}
	}
	return nil
}
