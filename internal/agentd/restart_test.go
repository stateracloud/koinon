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

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/askuser"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/questions"
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
func (e *env) startSet(name string, handler func(conns *pgxpool.Pool) http.Handler) *procSet {
	conns, err := pgxpool.New(context.Background(), db.Config().ConnString())
	Expect(err).NotTo(HaveOccurred())
	s := &procSet{name: name, conns: conns, tools: httptest.NewServer(handler(conns))}
	s.stopPool = e.poolOn(conns, name)
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
	o.mu.Unlock()
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

var _ = Describe("Restarts with two process sets on one Postgres (B8)", func() {
	It("resumes a run parked on an approval exactly once after the set that parked it is killed", func(ctx SpecContext) {
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

		log := slog.New(slog.NewTextHandler(GinkgoWriter, nil))
		var svcs sync.Map
		mk := func(conns *pgxpool.Pool) http.Handler {
			tools, err := githubtools.New(conns, githubtools.Options{Logger: log})
			Expect(err).NotTo(HaveOccurred())
			return tools.Handler()
		}
		a, b := g.startSet("set-a", mk), g.startSet("set-b", mk)
		for _, s := range []*procSet{a, b} {
			svcs.Store(s.name, approvals.New(s.conns, approvals.Options{
				Keys: g.ring, GitHub: github.NewClient(github.Options{BaseURL: g.gh.URL()}), Logger: log,
			}))
		}
		g.toolsMu.Lock()
		g.tools = a.tools
		g.toolsMu.Unlock()

		t := g.task(ctx, markAsk+": fix the typo")
		r := g.enqueue(ctx, t)
		owners.watch(r)
		var id string
		Eventually(ids).WithTimeout(10 * time.Second).Should(Receive(&id))
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateParked))
		first := owners.snapshot()
		Expect(first).To(HaveLen(1))

		// The set that drove the turn dies while the run waits for the approver.
		victim, survivor := setOf(first[0], a, b)
		victim.kill()
		g.toolsMu.Lock()
		g.tools = survivor.tools
		g.toolsMu.Unlock()
		Consistently(runState(ctx, r)).WithTimeout(500 * time.Millisecond).Should(Equal(store.RunStateParked))

		// The approver decides through the other set; two decisions race, only one counts.
		svcAny, _ := svcs.Load(survivor.name)
		svc := svcAny.(*approvals.Service)
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

		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))
		Consistently(func() int { p, _, _, _ := g.harness.snapshot(); return len(p) }).WithTimeout(time.Second).Should(Equal(2),
			"one prompt for the task, one for the resume")
		prompts, loads, _, _ := g.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring("approved and executed by Koinon"))
		Expect(loads).To(Equal([]string{"ses_1"}))
		Expect(owners.snapshot()[1]).To(HavePrefix(survivor.name+"/"), "the survivor resumed it")
		Expect(getRun(ctx, r).Epoch).To(BeEquivalentTo(2))
		Expect(g.gh.Snapshot("widgets").Pulls).To(HaveLen(1), "executed once")
	})

	It("resumes a run parked on ask_user exactly once after the set that parked it is killed", func(ctx SpecContext) {
		a0 := newAskEnv(ctx)
		owners := &ownerLog{}
		a0.harness.mu.Lock()
		inner := a0.harness.onPrompt
		a0.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			owners.record(ctx)
			return inner(ctx, text)
		}
		a0.harness.mu.Unlock()

		log := slog.New(slog.NewTextHandler(GinkgoWriter, nil))
		mk := func(conns *pgxpool.Pool) http.Handler {
			return askuser.New(conns, askuser.Options{Logger: log}).Handler()
		}
		a, b := a0.startSet("set-a", mk), a0.startSet("set-b", mk)
		a0.toolsMu.Lock()
		a0.tools = a.tools
		a0.toolsMu.Unlock()

		t := a0.task(ctx, markQuestion+": deploy the app")
		r := a0.enqueue(ctx, t)
		owners.watch(r)
		var qid string
		Eventually(a0.ids).WithTimeout(10 * time.Second).Should(Receive(&qid))
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateParked))
		first := owners.snapshot()
		Expect(first).To(HaveLen(1))

		victim, survivor := setOf(first[0], a, b)
		victim.kill()
		a0.toolsMu.Lock()
		a0.tools = survivor.tools
		a0.toolsMu.Unlock()
		Consistently(runState(ctx, r)).WithTimeout(500 * time.Millisecond).Should(Equal(store.RunStateParked))

		results := make(chan error, 3)
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- resolveIn(ctx, survivor.conns, func(q *store.Queries) error {
					_, err := questions.Respond(ctx, q, a0.org, mustUUID(qid), a0.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "us-east-1"})
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

		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))
		Consistently(func() int { p, _, _, _ := a0.harness.snapshot(); return len(p) }).WithTimeout(time.Second).Should(Equal(2),
			"one prompt for the question, one for the answer")
		prompts, loads, _, _ := a0.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`the person answered: "us-east-1"`))
		Expect(loads).To(Equal([]string{"ses_1"}))
		Expect(owners.snapshot()[1]).To(HavePrefix(survivor.name + "/"))
		Expect(getRun(ctx, r).Epoch).To(BeEquivalentTo(2))
	})
})
