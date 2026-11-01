// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// An MCP server the agent may use that is not serving tools when the run is about to start its
// harness is recorded, retried once and fails the run on a second miss (issue #326).
var _ = Describe("Checking the agent's MCP servers before the harness starts", func() {
	const retry = 300 * time.Millisecond

	// allow adds an MCP server in the given health to the org and lets the agent use it.
	allow := func(ctx context.Context, e *env, name, health string, failures int, extra string) {
		GinkgoHelper()
		var id string
		Expect(db.QueryRow(ctx, `INSERT INTO mcp_servers (org_id, name, url, health, health_reason, consecutive_failures`+extra+`)
			VALUES ($1, $2, 'http://mcp.example.com/mcp', $3::mcp_health, 'the gateway got no tools from the server', $4`+
			map[bool]string{true: ", 'disabled by an admin'", false: ""}[extra != ""]+`) RETURNING id::text`,
			e.org, name, health, failures).Scan(&id)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO agent_mcp_servers (org_id, agent_id, server_id) VALUES ($1, $2, $3::uuid)`,
			e.org, e.agent.ID, id)
		Expect(err).NotTo(HaveOccurred())
	}
	setHealth := func(ctx context.Context, e *env, name string) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `UPDATE mcp_servers SET health = 'healthy', consecutive_failures = 0 WHERE org_id = $1 AND name = $2`, e.org, name)
		Expect(err).NotTo(HaveOccurred())
	}
	missing := func(ctx context.Context, r store.Run) func() []store.RunEvent {
		return func() []store.RunEvent { return eventOf(events(ctx, r), "run.mcp_server_missing") }
	}

	It("starts the harness without a word when every allowed server is healthy", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "healthy", 0, "")
		allow(ctx, e, "wiki", "unknown", 0, "")
		allow(ctx, e, "old", "unhealthy", 5, ", disabled_reason")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.MCPRetryDelay = retry })
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(missing(ctx, r)()).To(BeEmpty())
	})

	It("records the miss, retries once, and starts the harness when the server is back", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "healthy", 2, "") // failing probes, below the threshold
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.MCPRetryDelay = time.Second })

		Eventually(missing(ctx, r)).WithTimeout(10 * time.Second).Should(HaveLen(1))
		ev := missing(ctx, r)()[0]
		Expect(payload(ev)).To(HaveKeyWithValue("attempt", BeNumerically("==", 1)))
		Expect(payload(ev)["servers"]).To(ConsistOf(HaveKeyWithValue("name", "tracker")))
		Expect(getRun(ctx, r).State).To(Equal(store.RunStateStarting))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(BeEmpty())

		setHealth(ctx, e, "tracker")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(eventOf(events(ctx, r), "run.mcp_server_recovered")).To(HaveLen(1))
		Expect(missing(ctx, r)()).To(HaveLen(1))
	})

	It("fails the run, naming the server, on a second miss", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "unhealthy", 3, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.MCPRetryDelay = retry })

		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(And(
			ContainSubstring("not serving tools"), ContainSubstring("tracker"), ContainSubstring("the gateway got no tools"),
		))
		evs := missing(ctx, r)()
		Expect(evs).To(HaveLen(2))
		Expect(payload(evs[1])).To(HaveKeyWithValue("attempt", BeNumerically("==", 2)))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(BeEmpty())
	})

	It("marks the run's root span with the servers that were missing", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "healthy", 1, "")
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", tracingOn(rcv, fakeDecider{traced: true, level: "none"}), func(c *agentd.Config) { c.MCPRetryDelay = time.Second })
		Eventually(missing(ctx, r)).WithTimeout(10 * time.Second).Should(HaveLen(1))
		setHealth(ctx, e, "tracker")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Eventually(rcv.count).WithTimeout(10 * time.Second).Should(BeNumerically(">=", 1))
		roots := spanByName(rcv.snapshot(), "invoke_agent "+e.agent.DisplayName)
		Expect(roots).To(HaveLen(1))
		Expect(attrMap(roots[0].Attributes)).To(And(
			HaveKeyWithValue(semconv.KoinonMCPMissingServers, "tracker"),
			HaveKeyWithValue(semconv.KoinonMCPMissingAttempts, "1"),
		))
	})

	// The gateway lists the tools itself: the health record can say healthy (the last probe passed,
	// or the drop fell between two probes) while the session's gateway has dropped the server.
	Describe("when agentd lists the gateway's tools itself", func() {
		withTools := func(l *stubLister) func(*agentd.Config) {
			return func(c *agentd.Config) { c.MCPTools = l; c.MCPRetryDelay = retry }
		}

		It("finds a server recorded healthy whose tools the gateway leaves out, and fails on the second miss", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "")
			allow(ctx, e, "wiki", "healthy", 0, "")
			slug, err := q.GetOrgSlug(ctx, e.org)
			Expect(err).NotTo(HaveOccurred())
			l := &stubLister{tools: []string{slug + "-wiki_search"}} // tracker's tools are missing
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withTools(l))

			Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
			Expect(getRun(ctx, r).OutcomeReason.String).To(And(ContainSubstring("tracker"), ContainSubstring("the gateway offers no tools")))
			Expect(getRun(ctx, r).OutcomeReason.String).NotTo(ContainSubstring("wiki"))
			evs := missing(ctx, r)()
			Expect(evs).To(HaveLen(2))
			Expect(payload(evs[0])["servers"]).To(ConsistOf(HaveKeyWithValue("name", "tracker")))
			prompts, _, _, _ := e.harness.snapshot()
			Expect(prompts).To(BeEmpty())
		})

		It("records the miss and starts the harness when the tools are back at the second look", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "")
			slug, err := q.GetOrgSlug(ctx, e.org)
			Expect(err).NotTo(HaveOccurred())
			l := &stubLister{}
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", func(c *agentd.Config) { c.MCPTools = l; c.MCPRetryDelay = time.Second })

			Eventually(missing(ctx, r)).WithTimeout(10 * time.Second).Should(HaveLen(1))
			l.set([]string{slug + "-tracker_create_issue"}, nil)
			Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
			Expect(eventOf(events(ctx, r), "run.mcp_server_recovered")).To(HaveLen(1))
		})

		It("does not trust a bad health record over the gateway's tools", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 2, "")
			slug, err := q.GetOrgSlug(ctx, e.org)
			Expect(err).NotTo(HaveOccurred())
			l := &stubLister{tools: []string{slug + "-tracker_create_issue"}}
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withTools(l))
			Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
			Expect(missing(ctx, r)()).To(BeEmpty())
		})

		It("falls back to the health record when the listing fails", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "unhealthy", 3, "")
			l := &stubLister{err: errors.New("connect: refused")}
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withTools(l))
			Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
			Expect(getRun(ctx, r).OutcomeReason.String).To(And(ContainSubstring("tracker"), ContainSubstring("the gateway got no tools")))
			Expect(l.calls()).To(BeNumerically(">=", 1))
		})
	})
})

// stubLister is the gateway's /mcp tool list, as a spec sets it.
type stubLister struct {
	mu    sync.Mutex
	tools []string
	err   error
	n     int
}

func (l *stubLister) ListTools(context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return l.tools, l.err
}

func (l *stubLister) set(tools []string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tools, l.err = tools, err
}

func (l *stubLister) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}
