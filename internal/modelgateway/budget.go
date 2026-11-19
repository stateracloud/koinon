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
	mu   sync.Mutex
	c    map[counterKey]*counter
	live map[counterKey]struct{} // the generations of the config in force; only these have a counter
}

func newBudgets() *budgets {
	return &budgets{c: map[counterKey]*counter{}, live: map[counterKey]struct{}{}}
}

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
// until the window ends (0 for a limit without a window, which only a new generation resets). live
// is false when l's generation is retired: it has no counter and the caller must read the limit again.
func (b *budgets) check(a gateway.AgentRef, l gateway.Limit, now time.Time) (ok bool, retryAfter time.Duration, live bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := counterKey{a, l.Generation}
	if _, ok := b.live[k]; !ok {
		return true, 0, false
	}
	c := b.roll(k, l, now)
	if c.spent < l.Amount {
		return true, 0, true
	}
	if w := l.AppliedWindow(); w > 0 {
		end := time.Unix(c.windowStart, 0).Add(w)
		return false, max(end.Sub(now), time.Second), true
	}
	return false, 0, true
}

// charge adds the cost of a finished call, to the window that holds the end of the call. The ledger
// dates a record by its start: the two differ only for a call across a window edge, and the later
// window then counts a little more, which errs on the safe side. An overflow saturates: a call that
// cannot be added up must stop the key, not lose the spend.
func (b *budgets) charge(a gateway.AgentRef, l gateway.Limit, cost money.USD, now time.Time) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	k := counterKey{a, l.Generation}
	if _, ok := b.live[k]; !ok {
		return // the generation is retired: its spend is in the ledger's next net amount
	}
	c := b.roll(k, l, now)
	sum, err := c.spent.Add(cost)
	if err != nil {
		sum = money.USD(math.MaxInt64)
	}
	c.spent = sum
}

// swap makes limits the live set, drops the counters of generations the new config does not carry
// (a retired generation does not stay in memory: ADR 0023 bounds, the set follows the limits), and
// runs publish while it still holds the lock. A check that finds a generation retired therefore
// finds the snapshot that retired it already published, and one retry reads the new limit.
func (b *budgets) swap(limits map[gateway.AgentRef]gateway.Limit, publish func()) {
	live := make(map[counterKey]struct{}, len(limits))
	for a, l := range limits {
		live[counterKey{a, l.Generation}] = struct{}{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.live = live
	for k := range b.c {
		if _, ok := live[k]; !ok {
			delete(b.c, k)
		}
	}
	publish()
}
