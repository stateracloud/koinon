// SPDX-License-Identifier: Apache-2.0

package modelgateway

import (
	"math"
	"sync"
	"time"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/money"
)

// The budget stop (ADR 0041 decisions 4 to 6, ADR 0097 decision 5, #279 slice 2): charge after the call.
//
// The spend ledger pushes each agent's limit net of the spend it recorded, and changes the limit's
// Generation with every new net amount (gateway.Limit). The gateway therefore counts only what it
// served since that generation began: one counter for each (agent, generation), reset by the
// limit's window. A call is refused when the counter has reached the amount, so a limit is
// overshot by at most the responses still in flight (gateway.Limit). A zero amount refuses every call.
//
// The counters are in memory, one set for each replica. A restart starts them at zero, which is
// safe in the direction the ledger covers: it pushes a new net amount (a new generation) as soon as
// it reads the usage records, so spend that a restart forgets reaches the gateway again as a
// smaller limit. Until then, a restart can let a replica overshoot by what it had counted
// (ADR 0097 decision 5 records the bound; the call reservation of step 3 removes it).

// counterKey names a spend counter. It carries the agent but never leaves the process: no metric
// and no log line uses it (ADR 0023).
type counterKey struct {
	agent gateway.AgentRef
	gen   string
}

type counter struct {
	windowStart int64 // Unix seconds of the window's start, 0 for a limit without a window
	spent       money.USD
}

// budgets holds the spend counters of one gateway.
type budgets struct {
	mu sync.Mutex
	c  map[counterKey]*counter
}

func newBudgets() *budgets { return &budgets{c: map[counterKey]*counter{}} }

// windowOf returns the start of the window that contains now. Windows align to the Unix epoch, as
// agentgateway's do: a 24 h window starts at midnight UTC.
func windowOf(l gateway.Limit, now time.Time) int64 {
	w := int64(l.AppliedWindow() / time.Second)
	if w == 0 {
		return 0
	}
	return now.Unix() / w * w
}

// roll resets c when its window has passed. The caller holds b.mu.
func (b *budgets) roll(k counterKey, l gateway.Limit, now time.Time) *counter {
	c := b.c[k]
	start := windowOf(l, now)
	if c == nil {
		c = &counter{windowStart: start}
		b.c[k] = c
	} else if c.windowStart != start {
		c.windowStart, c.spent = start, 0
	}
	return c
}

// check reports whether the agent may start a call under l. When it may not, retryAfter is the time
// until the window ends (0 for a limit without a window, which only a new generation resets).
func (b *budgets) check(a gateway.AgentRef, l gateway.Limit, now time.Time) (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.roll(counterKey{a, l.Generation}, l, now)
	if c.spent < l.Amount {
		return true, 0
	}
	if w := l.AppliedWindow(); w > 0 {
		end := time.Unix(c.windowStart, 0).Add(w)
		return false, max(end.Sub(now), time.Second)
	}
	return false, 0
}

// charge adds the cost of a finished call. An overflow saturates: a call that cannot be added up
// must stop the key, not lose the spend.
func (b *budgets) charge(a gateway.AgentRef, l gateway.Limit, cost money.USD, now time.Time) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.roll(counterKey{a, l.Generation}, l, now)
	sum, err := c.spent.Add(cost)
	if err != nil {
		sum = money.USD(math.MaxInt64)
	}
	c.spent = sum
}

// keep drops the counters of generations the new config does not carry, so a retired generation does
// not stay in memory (ADR 0023 bounds: the set follows the limits).
func (b *budgets) keep(limits map[gateway.AgentRef]gateway.Limit) {
	live := make(map[counterKey]struct{}, len(limits))
	for a, l := range limits {
		live[counterKey{a, l.Generation}] = struct{}{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.c {
		if _, ok := live[k]; !ok {
			delete(b.c, k)
		}
	}
}
