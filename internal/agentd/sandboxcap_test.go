// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// The cap on the total wait for a sandbox (old issue 160; ADR 0066 amendment 2026-10-28).
var _ = Describe("the sandbox wait cap", func() {
	addAdmin := func(ctx context.Context, e *env) pgtype.UUID {
		GinkgoHelper()
		var personal, id pgtype.UUID
		// (A deleted personal org: the manual-owner rule is checked for live orgs only.)
		Expect(db.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ($1, 'Personal', 'personal', now()) RETURNING id`, uniq("org")).Scan(&personal)).To(Succeed())
		Expect(db.QueryRow(ctx, `INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			uniq("sub"), personal).Scan(&id)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'admin')`, e.org, id)
		Expect(err).NotTo(HaveOccurred())
		return id
	}
	count := func(ctx context.Context, query string, args ...any) int {
		GinkgoHelper()
		var n int
		Expect(db.QueryRow(ctx, query, args...).Scan(&n)).To(Succeed())
		return n
	}
	waitStart := func(ctx context.Context, r store.Run) pgtype.Timestamptz {
		GinkgoHelper()
		var t pgtype.Timestamptz
		Expect(db.QueryRow(ctx, `SELECT sandbox_wait_started_at FROM runs WHERE org_id = $1 AND id = $2`, r.OrgID, r.ID).Scan(&t)).To(Succeed())
		return t
	}

	It("fails a run whose sandbox never becomes ready within the cap, names the cause and marks the agent unhealthy", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.recordSandboxWith(ctx, store.SandboxStateStarting, "ImagePullBackOff", `Back-off pulling image "registry.example.test/x:1"`)
		admin := addAdmin(ctx, e)
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		clock := &fakeClock{t: time.Now()}
		m := agentd.NewMetrics(prometheus.NewRegistry())
		e.pool("agentd-a", func(c *agentd.Config) {
			c.SandboxTimeout, c.SandboxWaitCap, c.Now, c.Metrics = 5*time.Minute, 20*time.Minute, clock.Now, m
		})
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		clock.awaitWait()
		clock.Advance(5*time.Minute + time.Second)
		Eventually(func() []string { return kinds(events(ctx, r)) }).WithTimeout(racescale.D(10 * time.Second)).Should(ContainElement("run.requeued"))

		Eventually(func() bool { g := getRun(ctx, r); return g.Epoch >= 2 && g.State == store.RunStateStarting }).WithTimeout(racescale.D(10 * time.Second)).Should(BeTrue())
		clock.Advance(16 * time.Minute)
		// The cap ran out while the run was queued: its claim gets one nudge and one poll first, which
		// the clock passes in small steps.
		Eventually(func() store.RunState {
			clock.Advance(10 * time.Second)
			return getRun(ctx, r).State
		}).WithTimeout(racescale.D(10 * time.Second)).WithPolling(100 * time.Millisecond).Should(Equal(store.RunStateFailed))

		failed := getRun(ctx, r)
		Expect(failed.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassSandboxUnavailable))
		Expect(failed.OutcomeReason.String).To(ContainSubstring("the sandbox never became ready in 20m"))
		Expect(failed.OutcomeReason.String).To(ContainSubstring("the image could not be pulled"))
		Expect(failed.OutcomeReason.String).To(ContainSubstring(`(starting: ImagePullBackOff: Back-off pulling image "registry.example.test/x:1")`),
			"the detail carries the recorded reason and message")
		var failEvent map[string]any
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "run.failed" {
				Expect(json.Unmarshal(ev.Payload, &failEvent)).To(Succeed())
			}
		}
		Expect(failEvent).To(HaveKeyWithValue("sandboxCause", "image_pull"))
		Expect(failEvent).To(HaveKeyWithValue("failureClass", "sandbox_unavailable"))

		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: e.org, ID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.UnhealthyAt.Valid).To(BeTrue())
		Expect(a.UnhealthyReason.String).To(ContainSubstring("never became ready"))
		for _, u := range []pgtype.UUID{e.user, admin} {
			Expect(count(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND user_id = $2 AND kind = 'agent.unhealthy' AND dedupe_key LIKE 'agent.unhealthy:%'`, e.org, u)).To(Equal(1))
		}
		Expect(count(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.mark_unhealthy' AND summary->'after'->>'cause' = 'sandbox_unavailable'`, e.org)).To(Equal(1))
		Expect(kindsOf(taskEvents(ctx, e, t))).To(ContainElement("agent.unhealthy"))
		Expect(testutil.ToFloat64(m.SandboxWaitCap.WithLabelValues("image_pull"))).To(Equal(1.0))
	})

	It("reports when the sandbox row first named the cause, apart from the cap (issue 573)", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.recordSandbox(ctx, store.SandboxStateStarting, "")
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) {
			c.SandboxTimeout, c.SandboxWaitCap, c.Now = 30*time.Minute, 10*time.Minute, clock.Now
		})
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		clock.awaitWait()
		clock.Advance(3 * time.Minute)
		e.recordSandboxWith(ctx, store.SandboxStateStarting, "ErrImagePull", `pulling image "registry.example.test/x:1"`)
		// Polls run on real time; let the loop read the new row before the cap runs out.
		reads := func() int { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.reads }
		before := reads()
		Eventually(reads).WithTimeout(racescale.D(10 * time.Second)).Should(BeNumerically(">=", before+6))
		clock.Advance(7*time.Minute + time.Second)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))

		var failEvent map[string]any
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "run.failed" {
				Expect(json.Unmarshal(ev.Payload, &failEvent)).To(Succeed())
			}
		}
		Expect(failEvent).To(HaveKeyWithValue("sandboxCause", "image_pull"))
		Expect(failEvent).To(HaveKey("causeSeenMs"))
		Expect(failEvent["causeSeenMs"]).To(BeNumerically("~", (3*time.Minute).Milliseconds(), 1),
			"the time to the cause, not the 10 minute cap")
	})

	It("uses the org's sandbox wait cap, which comes before the queue wait", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.recordSandbox(ctx, store.SandboxStateStarting, "")
		capd := 10 * time.Minute
		Expect(orgsettings.Set(ctx, q, e.org, orgsettings.Override{SandboxWaitCap: &capd})).To(Succeed())
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) { c.SandboxTimeout, c.Now = 30*time.Minute, clock.Now })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		clock.awaitWait()
		clock.Advance(10*time.Minute + time.Second)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(kinds(events(ctx, r))).NotTo(ContainElement("run.requeued"))
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "run.failed" {
				Expect(string(ev.Payload)).NotTo(ContainSubstring("causeSeenMs"), "no cause was named, so no time to it")
			}
		}
		Expect(getRun(ctx, r).FailureClass.RunFailureClass).To(Equal(store.RunFailureClassSandboxUnavailable))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("never became ready in 10m"))
	})

	It("starts a fresh wait after the agent is resumed", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.recordSandbox(ctx, store.SandboxStateStarting, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		clock := &fakeClock{t: time.Now()}
		stop := e.pool("agentd-a", func(c *agentd.Config) {
			c.SandboxTimeout, c.SandboxWaitCap, c.Now = 30*time.Minute, 10*time.Minute, clock.Now
		})
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		clock.awaitWait()
		clock.Advance(11 * time.Minute)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		stop()

		// Another run of the agent was waiting too. services.ResumeAgent clears the waits next to the
		// resume; agentd's tests do not import services, so the same two queries run here (the
		// services side is covered in internal/api).
		queued := e.enqueue(ctx, e.task(ctx, "hello again"))
		_, err := db.Exec(ctx, `UPDATE runs SET sandbox_wait_started_at = now() - interval '2 hours' WHERE org_id = $1 AND id = $2`, e.org, queued.ID)
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ResumeAgent(ctx, store.ResumeAgentParams{OrgID: e.org, ID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ClearSandboxWaitForAgent(ctx, store.ClearSandboxWaitForAgentParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(waitStart(ctx, queued).Valid).To(BeFalse())
	})

	It("does not count time queued behind a sibling that had the sandbox", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		sibling := e.enqueue(ctx, e.task(ctx, "sibling"))
		waiting := e.enqueue(ctx, e.task(ctx, "waiting"))
		// The waiting run was queued behind the sibling with a wait that started long ago.
		_, err := db.Exec(ctx, `UPDATE runs SET sandbox_wait_started_at = now() - interval '2 hours' WHERE org_id = $1 AND id = $2`, e.org, waiting.ID)
		Expect(err).NotTo(HaveOccurred())
		// The sandbox stops being ready once the sibling has used it, before the next claim.
		e.harness.set(func(h *fakeHarness) {
			h.onPrompt = func(ctx context.Context, _ string) (string, bool) {
				defer GinkgoRecover()
				e.recordSandbox(ctx, store.SandboxStateStarting, "")
				return "", false
			}
		})
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) {
			c.Workers, c.SandboxTimeout, c.SandboxWaitCap, c.Now = 1, 5*time.Minute, 20*time.Minute, clock.Now
		})
		Eventually(runState(ctx, sibling)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		Eventually(runState(ctx, waiting)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateStarting))
		// The clock goes forward in steps until the queue wait passes: five minutes of it, not the
		// two hours the stale start would have counted.
		Eventually(func() []string {
			clock.Advance(time.Minute)
			return kinds(events(ctx, waiting))
		}).WithTimeout(racescale.D(10 * time.Second)).WithPolling(100 * time.Millisecond).Should(ContainElement("run.requeued"))
		Expect(getRun(ctx, waiting).State).NotTo(Equal(store.RunStateFailed))
		Expect(getRun(ctx, waiting).FailureClass.Valid).To(BeFalse())
		Expect(waitStart(ctx, waiting).Time).To(BeTemporally("~", clock.Now(), 30*time.Minute), "a fresh wait, not the one from two hours ago")
	})

	It("clears the start of a taken-over running run once the sandbox is ready", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "dead-owner", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE runs SET sandbox_wait_started_at = now() - interval '10 minutes', lease_expires_at = now() - interval '1 minute'
			WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(waitStart(ctx, r).Valid).To(BeFalse())
	})

	It("gives a claim one nudge before the cap", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.recordSandbox(ctx, store.SandboxStateStarting, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		_, err := db.Exec(ctx, `UPDATE runs SET sandbox_wait_started_at = now() - interval '2 hours' WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) {
			c.SandboxTimeout, c.SandboxWaitCap, c.Now, c.ReconcileNudge = 5*time.Minute, time.Hour, clock.Now, time.Second
		})
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		clock.awaitWait()
		Consistently(runState(ctx, r)).WithTimeout(500*time.Millisecond).Should(Equal(store.RunStateStarting),
			"the cap ran out while the run was queued, but the claim gets its nudge and its poll first")
		clock.Advance(2 * time.Second)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).FailureClass.RunFailureClass).To(Equal(store.RunFailureClassSandboxUnavailable))
	})
})
