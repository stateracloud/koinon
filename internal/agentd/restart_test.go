// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/askuser"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// procSet is one set of Koinon processes on the shared Postgres: a koinon-api tool server and an
// agentd pool, each on its own connection pool. The other sets and the fake gateway are shared.
type procSet struct {
	name     string
	conns    *pgxpool.Pool
	tools    *httptest.Server
	stopPool func()
	killed   bool
}

// startSet starts a set. handler builds the set's tool server on the set's own connections.
func (e *env) startSet(name string, handler func(conns *pgxpool.Pool) http.Handler, mutate ...func(*agentd.Config)) *procSet {
	conns, err := pgxpool.New(context.Background(), db.Config().ConnString())
	Expect(err).NotTo(HaveOccurred())
	s := &procSet{name: name, conns: conns, tools: httptest.NewServer(handler(conns))}
	s.stopPool = e.poolOn(conns, name, mutate...)
	DeferCleanup(s.kill)
	return s
}

// kill ends the set as a crash does: its tool server drops its connections, its agentd pool stops
// without a word to anyone, and its connections close. All state is in Postgres.
func (s *procSet) kill() {
	if s.killed {
		return
	}
	s.killed = true
	s.tools.CloseClientConnections()
	s.tools.Close()
	s.stopPool()
	s.conns.Close()
}

// ownerLog records which pool owned the run at each prompt.
type ownerLog struct {
	mu     sync.Mutex
	run    store.Run
	owners []string
	// onOwner, if set, is called with the owner of each prompt before the prompt is answered, so
	// the test can point the harness at the tool server of the set that drives the turn.
	onOwner func(owner string)
}

func (o *ownerLog) watch(r store.Run) {
	o.mu.Lock()
	o.run = r
	o.mu.Unlock()
}

func (o *ownerLog) record(ctx context.Context) {
	o.mu.Lock()
	r := o.run
	o.mu.Unlock()
	row, err := q.GetRun(ctx, store.GetRunParams{OrgID: r.OrgID, ID: r.ID})
	owner := ""
	if err == nil {
		owner = row.OwnerID.String
	}
	o.mu.Lock()
	o.owners = append(o.owners, owner)
	route := o.onOwner
	o.mu.Unlock()
	if route != nil {
		route(owner)
	}
}

func (o *ownerLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.owners...)
}

// setOf returns the set among sets whose pool owns owner ("<set>/<worker>"), and the other.
func setOf(owner string, a, b *procSet) (victim, survivor *procSet) {
	if strings.HasPrefix(owner, a.name+"/") {
		return a, b
	}
	return b, a
}

// resumeBlock makes the first resume prompt (one carrying marker) tell blocked who drives it and
// then hang until its pool dies, like markHang: a crash while the lease is held.
type resumeBlock struct {
	marker  string
	blocked chan string
	once    sync.Once
}

func (b *resumeBlock) wrap(owners *ownerLog, inner func(context.Context, string) (string, bool)) func(context.Context, string) (string, bool) {
	return func(ctx context.Context, text string) (string, bool) {
		owners.record(ctx)
		hang := false
		if strings.Contains(text, b.marker) {
			b.once.Do(func() { hang = true })
		}
		if hang {
			owners.mu.Lock()
			n := len(owners.owners)
			owner := owners.owners[n-1]
			owners.mu.Unlock()
			b.blocked <- owner
			<-ctx.Done()
			return "", false
		}
		return inner(ctx, text)
	}
}

var _ = Describe("Restarts with two process sets on one Postgres (B8)", func() {
	keepLease := func(c *agentd.Config) { c.KeepLeaseOnShutdown = true }

	// githubSets wires a ghEnv to two sets; each set's tools serve the turns its pool drives.
	githubSets := func(ctx context.Context, g *ghEnv, owners *ownerLog, mutate ...func(*agentd.Config)) (a, b *procSet, svcs *sync.Map) {
		log := slog.New(slog.NewTextHandler(GinkgoWriter, nil))
		svcs = &sync.Map{}
		mk := func(conns *pgxpool.Pool) http.Handler {
			tools, err := githubtools.New(conns, githubtools.Options{Logger: log})
			Expect(err).NotTo(HaveOccurred())
			return tools.Handler()
		}
		a, b = g.startSet("set-a", mk, mutate...), g.startSet("set-b", mk, mutate...)
		for _, s := range []*procSet{a, b} {
			svcs.Store(s.name, approvals.New(s.conns, approvals.Options{
				Keys: g.ring, GitHub: github.NewClient(github.Options{BaseURL: g.gh.URL()}), Logger: log,
			}))
		}
		owners.onOwner = func(owner string) {
			s, _ := setOf(owner, a, b)
			g.toolsMu.Lock()
			g.tools = s.tools
			g.toolsMu.Unlock()
		}
		return a, b, svcs
	}

	approveTwice := func(ctx context.Context, g *ghEnv, svc *approvals.Service, id string) {
		row, err := q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: g.org, ID: mustUUID(id)})
		Expect(err).NotTo(HaveOccurred())
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				_, _ = svc.Approve(ctx, g.org, row.ID, g.user, row.ArgsSha256, "")
			}()
		}
		wg.Wait()
	}

	It("resumes a run parked on an approval exactly once while both sets race to claim it", func(ctx SpecContext) {
		g := newGitHubEnv(ctx)
		ids := g.script()
		owners := &ownerLog{}
		g.harness.mu.Lock()
		inner := g.harness.onPrompt
		g.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			owners.record(ctx)
			return inner(ctx, text)
		}
		g.harness.mu.Unlock()
		a, b, svcs := githubSets(ctx, g, owners)

		t := g.task(ctx, markAsk+": fix the typo")
		r := g.enqueue(ctx, t)
		owners.watch(r)
		var id string
		Eventually(ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&id))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		Expect(owners.snapshot()).To(HaveLen(1))

		// Both sets stay alive: the approver decides through one, two decisions race, only one
		// counts, and both pools poll for the resumed run.
		svcAny, _ := svcs.Load(b.name)
		approveTwice(ctx, g, svcAny.(*approvals.Service), id)

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Consistently(func() int { p, _, _, _ := g.harness.snapshot(); return len(p) }).WithTimeout(time.Second).Should(Equal(2),
			"one prompt for the task, one for the resume")
		prompts, loads, _, _ := g.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring("approved and executed by Koinon"))
		Expect(loads).To(Equal([]string{"ses_1"}))
		Expect(owners.snapshot()[1]).To(Or(HavePrefix(a.name+"/"), HavePrefix(b.name+"/")))
		Expect(getRun(ctx, r).Epoch).To(BeEquivalentTo(2))
		Expect(getRun(ctx, r).Takeovers).To(BeZero())
		Expect(g.gh.Snapshot("widgets").Pulls).To(HaveLen(1), "executed once")
	})

	It("takes over a resume that crashed with its lease held, after the lease expires", func(ctx SpecContext) {
		g := newGitHubEnv(ctx)
		ids := g.script()
		owners := &ownerLog{}
		hang := &resumeBlock{marker: "approved and executed by Koinon", blocked: make(chan string, 1)}
		g.harness.mu.Lock()
		g.harness.onPrompt = hang.wrap(owners, g.harness.onPrompt)
		g.harness.mu.Unlock()
		a, b, svcs := githubSets(ctx, g, owners, keepLease)

		t := g.task(ctx, markAsk+": fix the typo")
		r := g.enqueue(ctx, t)
		owners.watch(r)
		var id string
		Eventually(ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&id))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		svcAny, _ := svcs.Load(a.name)
		approveTwice(ctx, g, svcAny.(*approvals.Service), id)

		// Whichever set claimed the resume dies inside the prompt, holding the lease.
		var claimant string
		Eventually(hang.blocked).WithTimeout(racescale.D(15 * time.Second)).Should(Receive(&claimant))
		victim, survivor := setOf(claimant, a, b)
		Expect(getRun(ctx, r).State).To(Equal(store.RunStateRunning))
		victim.kill()

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateSucceeded))
		final := getRun(ctx, r)
		Expect(final.OwnerID.String).To(HavePrefix(survivor.name + "/"))
		Expect(final.Epoch).To(BeEquivalentTo(3), "claimed, resumed after the park, taken over")
		Expect(final.Takeovers).To(BeEquivalentTo(1))
		prompts, _, _, _ := g.harness.snapshot()
		Expect(prompts[len(prompts)-1]).To(ContainSubstring(agentd.ResumeNote))
		Expect(g.gh.Snapshot("widgets").Pulls).To(HaveLen(1), "executed once, not again by the takeover")
	})

	It("resumes a run parked on ask_user exactly once while both sets race to claim it", func(ctx SpecContext) {
		a0 := newAskEnv(ctx)
		owners := &ownerLog{}
		a0.harness.mu.Lock()
		inner := a0.harness.onPrompt
		a0.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			owners.record(ctx)
			return inner(ctx, text)
		}
		a0.harness.mu.Unlock()
		a, b := askSets(a0, owners)

		t := a0.task(ctx, markQuestion+": deploy the app")
		r := a0.enqueue(ctx, t)
		owners.watch(r)
		var qid string
		Eventually(a0.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		Expect(owners.snapshot()).To(HaveLen(1))

		answerThrice(ctx, a0, b.conns, qid)

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Consistently(func() int { p, _, _, _ := a0.harness.snapshot(); return len(p) }).WithTimeout(time.Second).Should(Equal(2),
			"one prompt for the question, one for the answer")
		prompts, loads, _, _ := a0.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`the person answered: "us-east-1"`))
		Expect(loads).To(Equal([]string{"ses_1"}))
		Expect(owners.snapshot()[1]).To(Or(HavePrefix(a.name+"/"), HavePrefix(b.name+"/")))
		Expect(getRun(ctx, r).Epoch).To(BeEquivalentTo(2))
		Expect(getRun(ctx, r).Takeovers).To(BeZero())
	})

	It("takes over an ask_user resume that crashed with its lease held", func(ctx SpecContext) {
		a0 := newAskEnv(ctx)
		owners := &ownerLog{}
		hang := &resumeBlock{marker: `the person answered: "us-east-1"`, blocked: make(chan string, 1)}
		a0.harness.mu.Lock()
		a0.harness.onPrompt = hang.wrap(owners, a0.harness.onPrompt)
		a0.harness.mu.Unlock()
		a, b := askSets(a0, owners, keepLease)

		t := a0.task(ctx, markQuestion+": deploy the app")
		r := a0.enqueue(ctx, t)
		owners.watch(r)
		var qid string
		Eventually(a0.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		answerThrice(ctx, a0, a.conns, qid)

		var claimant string
		Eventually(hang.blocked).WithTimeout(racescale.D(15 * time.Second)).Should(Receive(&claimant))
		victim, survivor := setOf(claimant, a, b)
		victim.kill()

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateSucceeded))
		final := getRun(ctx, r)
		Expect(final.OwnerID.String).To(HavePrefix(survivor.name + "/"))
		Expect(final.Epoch).To(BeEquivalentTo(3))
		Expect(final.Takeovers).To(BeEquivalentTo(1))
		prompts, _, _, _ := a0.harness.snapshot()
		Expect(prompts[len(prompts)-1]).To(ContainSubstring(agentd.ResumeNote))
		answered, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: a0.org, ID: mustUUID(qid)})
		Expect(err).NotTo(HaveOccurred())
		Expect(answered.State).To(Equal(store.QuestionStateAnswered), "answered once")
	})
})

// askSets starts two sets with an ask_user tool server each; the harness uses the tools of the set
// that drives each turn.
func askSets(a0 *askEnv, owners *ownerLog, mutate ...func(*agentd.Config)) (a, b *procSet) {
	log := slog.New(slog.NewTextHandler(GinkgoWriter, nil))
	mk := func(conns *pgxpool.Pool) http.Handler {
		return askuser.New(conns, askuser.Options{Logger: log}).Handler()
	}
	a, b = a0.startSet("set-a", mk, mutate...), a0.startSet("set-b", mk, mutate...)
	owners.onOwner = func(owner string) {
		s, _ := setOf(owner, a, b)
		a0.toolsMu.Lock()
		a0.tools = s.tools
		a0.toolsMu.Unlock()
	}
	return a, b
}

// answerThrice answers the question from three goroutines on conns; exactly one counts.
func answerThrice(ctx context.Context, a0 *askEnv, conns *pgxpool.Pool, qid string) {
	results := make(chan error, 3)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- resolveIn(ctx, conns, func(q *store.Queries, b audit.Batch) error {
				_, err := questions.Respond(ctx, q, b, a0.org, mustUUID(qid), a0.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "us-east-1"})
				return err
			})
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		}
	}
	Expect(ok).To(Equal(1))
}
