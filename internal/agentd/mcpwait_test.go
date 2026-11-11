// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// An MCP server the agent may use that is not serving tools when the run is about to start its
// harness is recorded, retried once and fails the run on a second miss (old issue 326).
var _ = Describe("Checking the agent's MCP servers before the harness starts", func() {
	const retry = 300 * time.Millisecond

	// tool adds a present tool of the given tier to an MCP server of the org.
	tool := func(ctx context.Context, e *env, server, name, tier string) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `INSERT INTO mcp_tools (org_id, server_id, name, content_hash, tier)
			SELECT org_id, id, $3, repeat('a', 64), $4::mcp_tier FROM mcp_servers WHERE org_id = $1 AND name = $2`,
			e.org, server, name, tier)
		Expect(err).NotTo(HaveOccurred())
	}

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
		tool(ctx, e, name, "create_issue", "auto")
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
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(missing(ctx, r)()).To(BeEmpty())
	})

	It("records the miss, retries once, and starts the harness when the server is back", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "healthy", 2, "") // failing probes, below the threshold
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.MCPRetryDelay = time.Second })

		Eventually(missing(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
		ev := missing(ctx, r)()[0]
		Expect(payload(ev)).To(HaveKeyWithValue("attempt", BeNumerically("==", 1)))
		Expect(payload(ev)["servers"]).To(ConsistOf(HaveKeyWithValue("name", "tracker")))
		Expect(getRun(ctx, r).State).To(Equal(store.RunStateStarting))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(BeEmpty())

		setHealth(ctx, e, "tracker")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(eventOf(events(ctx, r), "run.mcp_server_recovered")).To(HaveLen(1))
		Expect(missing(ctx, r)()).To(HaveLen(1))
	})

	It("fails the run, naming the server, on a second miss", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "unhealthy", 3, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.MCPRetryDelay = retry })

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
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
		Eventually(missing(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
		setHealth(ctx, e, "tracker")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(BeNumerically(">=", 1))
		roots := spanByName(rcv.snapshot(), "invoke_agent "+e.agent.DisplayName)
		Expect(roots).To(HaveLen(1))
		Expect(attrMap(roots[0].Attributes)).To(And(
			HaveKeyWithValue(semconv.KoinonMCPMissingServers, "tracker"),
			HaveKeyWithValue(semconv.KoinonMCPMissingAttempts, "1"),
		))
	})

	It("gives a run that fails on the second miss no root span, only the attempt-2 event", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "unhealthy", 3, "")
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", tracingOn(rcv, fakeDecider{traced: true, level: "none"}), func(c *agentd.Config) { c.MCPRetryDelay = retry })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(missing(ctx, r)()).To(HaveLen(2))
		Consistently(rcv.count).WithTimeout(time.Second).Should(BeZero())
	})

	It("puts the missing-server attributes on the first turn's root span only", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		allow(ctx, e, "tracker", "healthy", 1, "")
		rcv := newReceiver()
		t := e.task(ctx, markWait)
		r, err := q.CreateRun(ctx, store.CreateRunParams{OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: 6})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-a", tracingOn(rcv, fakeDecider{traced: true, level: "none"}), func(c *agentd.Config) {
			c.MCPRetryDelay = time.Second
			c.WrapUpNotice = 4 * time.Second
		})
		Eventually(missing(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
		setHealth(ctx, e, "tracker")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateCanceled))
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(BeNumerically(">=", 2))
		roots := spanByName(rcv.snapshot(), "invoke_agent "+e.agent.DisplayName)
		Expect(len(roots)).To(BeNumerically(">=", 2))
		for _, s := range roots {
			attrs := attrMap(s.Attributes)
			if attrs[semconv.KoinonTurn] == "1" {
				Expect(attrs).To(HaveKeyWithValue(semconv.KoinonMCPMissingServers, "tracker"))
			} else {
				Expect(attrs).NotTo(HaveKey(semconv.KoinonMCPMissingServers))
				Expect(attrs).NotTo(HaveKey(semconv.KoinonMCPMissingAttempts))
			}
		}
	})

	Describe("the live check with the agent's own key (old issue 488)", func() {
		// It holds the tool names the gateway lists to the agent. It is koinon-api's answer; agentd
		// never sees the agent's key, so nothing here carries one.
		liveTools := &fakeMCPTools{}
		BeforeEach(func() { liveTools.set(nil, nil) })

		withLive := func(c *agentd.Config) { c.MCPTools = liveTools; c.MCPRetryDelay = time.Second }
		slug := func(ctx context.Context, e *env) string {
			GinkgoHelper()
			var s string
			Expect(db.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, e.org).Scan(&s)).To(Succeed())
			return s
		}

		It("starts the harness when the gateway lists the server's tools", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "") // the health record is fine
			liveTools.set([]string{slug(ctx, e) + "-tracker_create_issue"}, nil)
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(missing(ctx, r)()).To(BeEmpty())
			Expect(liveTools.calls()).To(BeNumerically(">=", 1))
		})

		It("catches a server that dropped between two probes: the record is healthy, the gateway lists nothing", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "")
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)

			Eventually(missing(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
			ev := missing(ctx, r)()[0]
			Expect(payload(ev)).To(HaveKeyWithValue("attempt", BeNumerically("==", 1)))
			Expect(payload(ev)["servers"]).To(ConsistOf(HaveKeyWithValue("name", "tracker")))
			prompts, _, _, _ := e.harness.snapshot()
			Expect(prompts).To(BeEmpty())

			liveTools.set([]string{slug(ctx, e) + "-tracker_create_issue"}, nil)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(eventOf(events(ctx, r), "run.mcp_server_recovered")).To(HaveLen(1))
		})

		It("fails the run on a second miss when the agent's key lacks the grant, and says no key is in it", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "")
			// The gateway lists another server's tool for this agent, not the tracker's.
			liveTools.set([]string{slug(ctx, e) + "-wiki_read"}, nil)
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)

			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
			Expect(getRun(ctx, r).OutcomeReason.String).To(And(ContainSubstring("tracker"), ContainSubstring("does not list")))
			Expect(getRun(ctx, r).OutcomeReason.String).NotTo(ContainSubstring("kgw_"))
			evs := missing(ctx, r)()
			Expect(evs).To(HaveLen(2))
			Expect(payload(evs[1])).To(HaveKeyWithValue("attempt", BeNumerically("==", 2)))
		})

		It("expects an ask-tier tool under the ask target", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "")
			_, err := db.Exec(ctx, `UPDATE mcp_tools SET tier = 'ask' WHERE org_id = $1`, e.org)
			Expect(err).NotTo(HaveOccurred())
			liveTools.set([]string{slug(ctx, e) + "-tracker-ask_create_issue"}, nil)
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(missing(ctx, r)()).To(BeEmpty())
		})

		It("counts a server as missing when only its ask tools are listed, since the ask target is koinon-api's proxy", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "") // one auto tool: create_issue
			tool(ctx, e, "tracker", "close_issue", "ask")
			liveTools.set([]string{slug(ctx, e) + "-tracker-ask_close_issue"}, nil)
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)
			Eventually(missing(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
			Expect(payload(missing(ctx, r)()[0])["servers"]).To(ConsistOf(HaveKeyWithValue("name", "tracker")))
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		})

		It("does not expect tools of a server with none to offer", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "unknown", 0, "")
			_, err := db.Exec(ctx, `UPDATE mcp_tools SET tier = 'never' WHERE org_id = $1`, e.org)
			Expect(err).NotTo(HaveOccurred())
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(missing(ctx, r)()).To(BeEmpty())
		})

		It("relies on the health record when the live check cannot be made", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 0, "")
			liveTools.set(nil, errors.New("koinon-api answered 502"))
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(missing(ctx, r)()).To(BeEmpty())
		})

		It("reports a server once when the health record and the live check both miss it", func(ctx SpecContext) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			allow(ctx, e, "tracker", "healthy", 2, "")
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withLive)
			Eventually(missing(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
			Expect(payload(missing(ctx, r)()[0])["servers"]).To(HaveLen(1))
		})
	})
})

// fakeMCPTools is the live check's stand-in for koinon-api: the tool names the gateway lists to the
// agent, or an error. The real one is mcpcheck.Remote.
type fakeMCPTools struct {
	mu    sync.Mutex
	names []string
	err   error
	n     int
}

func (f *fakeMCPTools) set(names []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names, f.err = names, err
}

func (f *fakeMCPTools) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *fakeMCPTools) Tools(context.Context, pgtype.UUID, pgtype.UUID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return append([]string(nil), f.names...), f.err
}
