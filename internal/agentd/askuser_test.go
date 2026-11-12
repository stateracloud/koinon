// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/askuser"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// markQuestion makes the fake harness call ask_user through Koinon's tool server.
const markQuestion = "QUESTION"

// askEnv is an env whose agent can call ask_user on the real tool server.
type askEnv struct {
	*env
	slug  string
	tools *httptest.Server
	// toolsMu guards tools, which serve and a restart test replace.
	toolsMu sync.Mutex
	mu      sync.Mutex
	args    map[string]any
	// cancelReason, when set, makes the harness cancel the task in the "no answer" run.
	cancelReason string
	ids          chan string
}

func newAskEnv(ctx context.Context) *askEnv {
	a := &askEnv{env: newEnv(ctx, harness.OpenCode), ids: make(chan string, 4)}
	Expect(db.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, a.org).Scan(&a.slug)).To(Succeed())
	a.serve()
	a.args = map[string]any{
		"question": "Which region should I deploy to?", "choices": []string{"eu-west-1", "us-east-1"},
		"default": "eu-west-1",
	}
	a.harness.mu.Lock()
	a.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
		a.mu.Lock()
		args, cancelReason := a.args, a.cancelReason
		a.mu.Unlock()
		if cancelReason != "" && strings.Contains(text, "short \"no answer\" run") {
			out, _ := a.callTool(ctx, askuser.ToolCancelTask, map[string]any{"reason": cancelReason})
			return "Canceling. " + out, true
		}
		if !strings.Contains(text, markQuestion) {
			return "", false
		}
		out, id := a.call(ctx, args)
		a.ids <- id
		return "Asked. " + out, true
	}
	a.harness.mu.Unlock()
	return a
}

// serve starts (or restarts, after a kill) the tool server on a fresh connection pool, like a new
// koinon-api process.
func (a *askEnv) serve() *pgxpool.Pool {
	a.toolsMu.Lock()
	old := a.tools
	a.toolsMu.Unlock()
	if old != nil {
		old.Close()
	}
	pool, err := pgxpool.New(context.Background(), db.Config().ConnString())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)
	srv := httptest.NewServer(askuser.New(pool, askuser.Options{Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil))}).Handler())
	a.toolsMu.Lock()
	a.tools = srv
	a.toolsMu.Unlock()
	DeferCleanup(srv.Close)
	return pool
}

func (a *askEnv) toolsURL() string {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	return a.tools.URL
}

func (a *askEnv) call(ctx context.Context, args map[string]any) (text, id string) {
	return a.callTool(ctx, askuser.ToolAskUser, args)
}

func (a *askEnv) callTool(ctx context.Context, tool string, args map[string]any) (text, id string) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-harness", Version: "1"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: a.toolsURL(), HTTPClient: &http.Client{Transport: headerTransport{agent: a.slug + "/" + a.agent.ID.String()}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return "connect: " + err.Error(), ""
	}
	defer func() { _ = s.Close() }()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "call: " + err.Error(), ""
	}
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	var out askuser.Result
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	return strings.Join(parts, "\n"), out.ID
}

func (a *askEnv) question(ctx context.Context, id string) store.Question {
	row, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: a.org, ID: mustUUID(id)})
	Expect(err).NotTo(HaveOccurred())
	return row
}

// resolve runs fn in a transaction on pool.
func resolveIn(ctx context.Context, pool *pgxpool.Pool, fn func(q *store.Queries, b audit.Batch) error) error {
	_, err := audit.InTx(ctx, pool, fn)
	return err
}

var _ = Describe("ask_user", func() {
	var (
		a   *askEnv
		t   store.Task
		r   store.Run
		qid string
	)

	// asked runs the task to its park on the question.
	asked := func(ctx SpecContext) (stop func()) {
		a = newAskEnv(ctx)
		t = a.task(ctx, markQuestion+": deploy the app")
		r = a.enqueue(ctx, t)
		stop = a.pool("agentd-a")
		Eventually(a.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		return stop
	}

	It("parks the run with reason input_required, opens the question and moves the task, together", func(ctx SpecContext) {
		asked(ctx)
		parked := getRun(ctx, r)
		Expect(parked.ParkReason.RunParkReason).To(Equal(store.RunParkReasonInputRequired))
		Expect(parked.LeaseExpiresAt.Valid).To(BeFalse(), "the run is released")
		Expect(parked.ParkExpiresAt.Time).To(BeTemporally("~", time.Now().Add(questions.DefaultTTL), time.Minute))

		row := a.question(ctx, qid)
		Expect(row.State).To(Equal(store.QuestionStateOpen))
		Expect(row.TargetKind).To(Equal(store.NotifyTargetKindUser))
		recipients, err := q.ListActiveQuestionRecipientIDs(ctx, store.ListActiveQuestionRecipientIDsParams{OrgID: a.org, QuestionID: row.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(recipients).To(Equal([]pgtype.UUID{a.user}), "the default addressee is the task's creator")
		Expect(row.DefaultAnswer).To(Equal("eu-west-1"))
		Expect(questions.ChoicesOf(row)).To(Equal([]string{"eu-west-1", "us-east-1"}))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: a.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(store.TaskStateInputRequired))
		Expect(kindsOf(taskEvents(ctx, a.env, t))).To(Equal([]string{"message", "message", questions.KindAsked, "run.parked"}))

		inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: a.org, UserID: a.user})
		Expect(err).NotTo(HaveOccurred())
		Expect(inbox).To(HaveLen(1))
		Consistently(runState(ctx, r)).WithTimeout(500 * time.Millisecond).Should(Equal(store.RunStateParked))
	})

	It("keeps the question invisible while the turn runs", func(ctx SpecContext) {
		a = newAskEnv(ctx)
		release := make(chan struct{})
		a.harness.mu.Lock()
		inner := a.harness.onPrompt
		a.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			out, ok := inner(ctx, text)
			<-release
			return out, ok
		}
		a.harness.mu.Unlock()
		t = a.task(ctx, markQuestion)
		r = a.enqueue(ctx, t)
		a.pool("agentd-a")
		Eventually(a.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateAsked))
		inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: a.org, UserID: a.user})
		Expect(err).NotTo(HaveOccurred())
		Expect(inbox).To(BeEmpty())
		got, _ := q.GetTask(ctx, store.GetTaskParams{OrgID: a.org, ID: t.ID})
		Expect(got.State).NotTo(Equal(store.TaskStateInputRequired))
		close(release)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateOpen))
	})

	It("resumes the run with the answer as the next message, through session/load", func(ctx SpecContext) {
		asked(ctx)
		pool := a.serve()
		var row store.Question
		Expect(resolveIn(ctx, pool, func(q *store.Queries, b audit.Batch) (err error) {
			row, err = questions.Respond(ctx, q, b, a.org, mustUUID(qid), a.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "us-east-1"})
			return err
		})).To(Succeed())
		Expect(row.State).To(Equal(store.QuestionStateAnswered))

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, loads, _, _ := a.harness.snapshot()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(HavePrefix("[Koinon] Your run was paused while a human decided"))
		Expect(prompts[1]).To(ContainSubstring(`the person answered: "us-east-1"`))
		Expect(prompts[1]).NotTo(ContainSubstring("no answer"))
		Expect(prompts[1]).NotTo(ContainSubstring("default was applied"))
		Expect(loads).To(Equal([]string{"ses_1"}))
		Expect(a.question(ctx, qid).DeliveredAt.Valid).To(BeTrue())
		got, _ := q.GetTask(ctx, store.GetTaskParams{OrgID: a.org, ID: t.ID})
		Expect(got.State).To(Equal(store.TaskStateCompleted), "the run that parked and resumed ends its task (old issue 324)")
		Expect(kindsOf(taskEvents(ctx, a.env, t))).To(ContainElement(questions.KindAnswered))
	})

	It("waits for a stopped sandbox, then resumes through session/load", func(ctx SpecContext) {
		asked(ctx)
		a.recordSandbox(ctx, store.SandboxStateStopped, "idle")
		Expect(resolveIn(ctx, db, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, a.org, mustUUID(qid), a.user, questions.Answer{Kind: store.QuestionAnswerKindText, Text: "ap-south-1, please"})
			return err
		})).To(Succeed())
		Consistently(runState(ctx, r)).WithTimeout(time.Second).ShouldNot(Equal(store.RunStateSucceeded))
		a.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, loads, _, _ := a.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`the person answered: "ap-south-1, please"`))
		Expect(loads).To(Equal([]string{"ses_1"}))
	})

	It("applies the default when the question is dismissed, and says so", func(ctx SpecContext) {
		asked(ctx)
		Expect(resolveIn(ctx, db, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Dismiss(ctx, q, b, a.org, mustUUID(qid), a.user)
			return err
		})).To(Succeed())
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := a.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`dismissed the question, so your default was applied: "eu-west-1"`))
		row := a.question(ctx, qid)
		Expect(row.State).To(Equal(store.QuestionStateDismissed))
		Expect(row.AnswerKind.QuestionAnswerKind).To(Equal(store.QuestionAnswerKindDefault))
	})

	It("applies the default on expiry, and says so", func(ctx SpecContext) {
		asked(ctx)
		Expect(resolveIn(ctx, db, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Expire(ctx, q, b, a.org, mustUUID(qid))
			return err
		})).To(Succeed())
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := a.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`nobody answered in time, so your default was applied: "eu-west-1"`))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateExpired))
	})

	It("sets the reminder and the expiry from the defaults, or from the agent's version (TK4)", func(ctx SpecContext) {
		// Pools claim runs across every org, so the first pool stops before the second org exists.
		stop := asked(ctx)
		row := a.question(ctx, qid)
		Expect(row.RemindAt.Time).To(BeTemporally("~", row.CreatedAt.Time.Add(questions.DefaultReminder), time.Minute))
		Expect(row.ExpiresAt.Time).To(BeTemporally("~", row.CreatedAt.Time.Add(questions.DefaultTTL), time.Minute))

		stop()
		a = newAskEnv(ctx)
		_, err := db.Exec(ctx, `UPDATE agent_versions SET config = config || '{"questions":{"reminderAfter":"2h0m0s","expireAfter":"48h0m0s"}}'::jsonb WHERE org_id = $1`, a.org)
		Expect(err).NotTo(HaveOccurred())
		t = a.task(ctx, markQuestion+": deploy the app")
		r = a.enqueue(ctx, t)
		a.pool("agentd-a")
		Eventually(a.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
		row = a.question(ctx, qid)
		Expect(row.RemindAt.Time).To(BeTemporally("~", row.CreatedAt.Time.Add(2*time.Hour), time.Minute))
		Expect(row.ExpiresAt.Time).To(BeTemporally("~", row.CreatedAt.Time.Add(48*time.Hour), time.Minute))
	})

	It("gives one short no-answer run after the sweep expires the question, once across a restart (TK4)", func(ctx SpecContext) {
		asked(ctx)
		notifier := durable.NewNotifier(nil, nil)
		questions.RegisterNotifications(notifier, db)
		week := time.Now().Add(questions.DefaultTTL + time.Minute)

		res, err := questions.RunOrgTimers(ctx, db, notifier, a.org, week)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(questions.SweepResult{Expired: 1}))
		// koinon-api restarts and sweeps again, as does a second replica.
		for range 2 {
			fresh := durable.NewNotifier(nil, nil)
			questions.RegisterNotifications(fresh, a.serve())
			res, err = questions.RunOrgTimers(ctx, db, fresh, a.org, week.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(questions.SweepResult{}))
		}

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := a.harness.snapshot()
		Expect(prompts).To(HaveLen(2), "the one no-answer run, not more")
		Expect(prompts[1]).To(ContainSubstring(`nobody answered in time, so your default was applied: "eu-west-1"`))
		Expect(prompts[1]).To(ContainSubstring("short \"no answer\" run"))
		Expect(prompts[1]).To(ContainSubstring("cancel_task"))
		Expect(prompts[1]).To(ContainSubstring("ask someone else"))
		Expect(kindsOf(taskEvents(ctx, a.env, t))).To(ContainElement(questions.KindExpired))
	})

	It("lets the no-answer run cancel the task, which ends canceled with the reason on its timeline (TK4)", func(ctx SpecContext) {
		asked(ctx)
		a.mu.Lock()
		a.cancelReason = "nobody answered and the region matters"
		a.mu.Unlock()
		notifier := durable.NewNotifier(nil, nil)
		questions.RegisterNotifications(notifier, db)
		_, err := questions.RunOrgTimers(ctx, db, notifier, a.org, time.Now().Add(questions.DefaultTTL+time.Minute))
		Expect(err).NotTo(HaveOccurred())

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: a.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(store.TaskStateCanceled))
		var reason string
		Expect(db.QueryRow(ctx, `SELECT payload->>'reason' FROM task_events WHERE org_id = $1 AND task_id = $2 AND kind = 'status.changed'`,
			a.org, t.ID).Scan(&reason)).To(Succeed())
		Expect(reason).To(Equal("nobody answered and the region matters"))
	})

	It("resumes exactly once when koinon-api is killed between the park and the answer (B8)", func(ctx SpecContext) {
		asked(ctx)
		// The API process dies: its tool server and pool go away. Everything is in Postgres.
		a.tools.Close()
		fresh := a.serve()

		var wg sync.WaitGroup
		results := make(chan error, 4)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- resolveIn(ctx, fresh, func(q *store.Queries, b audit.Batch) error {
					_, err := questions.Respond(ctx, q, b, a.org, mustUUID(qid), a.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "eu-west-1"})
					return err
				})
			}()
		}
		wg.Wait()
		close(results)
		ok, notOpen := 0, 0
		for err := range results {
			switch {
			case err == nil:
				ok++
			case strings.Contains(err.Error(), questions.ErrNotOpen.Error()):
				notOpen++
			default:
				Fail("unexpected error: " + err.Error())
			}
		}
		Expect(ok).To(Equal(1))
		Expect(notOpen).To(Equal(3))

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Consistently(func() int { p, _, _, _ := a.harness.snapshot(); return len(p) }).WithTimeout(time.Second).Should(Equal(2), "one prompt for the question, one for the answer")
		Expect(getRun(ctx, r).Epoch).To(BeEquivalentTo(2), "claimed once more after the park")
	})

	It("refuses a question without a safe default, to a stranger, or with a default outside the choices", func(ctx SpecContext) {
		a = newAskEnv(ctx)
		_, err := db.Exec(ctx, `UPDATE users SET email = 'Owner@Example.Test' WHERE id = $1`, a.user)
		Expect(err).NotTo(HaveOccurred())
		texts := make(chan []string, 1)
		a.harness.mu.Lock()
		a.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			if !strings.Contains(text, markQuestion) {
				return "", false
			}
			var got []string
			for _, args := range []map[string]any{
				{"question": "Post it?"}, // the tool's schema requires a default
				{"question": "Post it?", "default": " "},
				{"question": "Post it?", "default": "no", "to": "nobody@example.test"},
				{"question": "Which?", "choices": []string{"a", "b"}, "default": "c"},
				{"question": "Post it?", "default": "no", "to": "owner@example.test"},
			} {
				out, _ := a.call(ctx, args)
				got = append(got, out)
			}
			texts <- got
			return "done", true
		}
		a.harness.mu.Unlock()
		t = a.task(ctx, markQuestion)
		r = a.enqueue(ctx, t)
		a.pool("agentd-a")
		var got []string
		Eventually(texts).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&got))
		Expect(got[0]).To(ContainSubstring(`missing properties: ["default"]`))
		Expect(got[1]).To(ContainSubstring("default is required"))
		Expect(got[2]).To(ContainSubstring("is not a member of this org"))
		Expect(got[3]).To(ContainSubstring("default must be one of the choices"))
		Expect(got[4]).To(ContainSubstring("question recorded"), "an email names a member, whatever its case")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: a.org, UserID: a.user})
		Expect(err).NotTo(HaveOccurred())
		Expect(inbox).To(HaveLen(1), "only the valid question exists")
	})

	It("asks the person in a chat thread when 'to' is left out", func(ctx SpecContext) {
		a = newAskEnv(ctx)
		a.args = map[string]any{"question": "Post it?", "default": "no"}
		var err error
		t, err = q.CreateTask(ctx, store.CreateTaskParams{
			OrgID: a.org, Title: uniq("chat"), AgentID: a.agent.ID, CreatedByKind: store.TaskCreatorKindTrigger,
			CreatedByTriggerID: services.DirectMessageTriggerID(a.agent.ID),
		})
		Expect(err).NotTo(HaveOccurred())
		a.message(ctx, t, markQuestion)
		r = a.enqueue(ctx, t)
		a.pool("agentd-a")
		Eventually(a.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
		row := a.question(ctx, qid)
		Expect(row.TargetKind).To(Equal(store.NotifyTargetKindUser))
		recipients, err := q.ListActiveQuestionRecipientIDs(ctx, store.ListActiveQuestionRecipientIDsParams{OrgID: a.org, QuestionID: row.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(recipients).To(Equal([]pgtype.UUID{a.user}))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
	})

	It("finishes the run and cancels the question when the agent cancels the task in the same turn", func(ctx SpecContext) {
		a = newAskEnv(ctx)
		a.harness.mu.Lock()
		a.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			if !strings.Contains(text, markQuestion) {
				return "", false
			}
			_, id := a.call(ctx, a.args)
			a.ids <- id
			out, _ := a.callTool(ctx, askuser.ToolCancelTask, map[string]any{"reason": "cannot go on"})
			return "Asked and canceled. " + out, true
		}
		a.harness.mu.Unlock()
		t = a.task(ctx, markQuestion)
		r = a.enqueue(ctx, t)
		a.pool("agentd-a")
		Eventually(a.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
		Eventually(func() store.RunState { return getRun(ctx, r).State }).WithTimeout(racescale.D(10 * time.Second)).
			ShouldNot(Or(Equal(store.RunStateQueued), Equal(store.RunStateStarting), Equal(store.RunStateRunning)))
		Expect(getRun(ctx, r).State).NotTo(Equal(store.RunStateParked))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateCanceled))
		got, _ := q.GetTask(ctx, store.GetTaskParams{OrgID: a.org, ID: t.ID})
		Expect(got.State).To(Equal(store.TaskStateCanceled))
		inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: a.org, UserID: a.user})
		Expect(err).NotTo(HaveOccurred())
		Expect(inbox).To(BeEmpty())
	})

	It("refuses an answer from someone else", func(ctx SpecContext) {
		asked(ctx)
		err := resolveIn(ctx, db, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, a.org, mustUUID(qid), pgtype.UUID{Bytes: [16]byte{9}, Valid: true}, questions.Answer{Kind: store.QuestionAnswerKindText, Text: "x"})
			return err
		})
		Expect(err).To(MatchError(questions.ErrNotAskedTo))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateOpen))
		Expect(runState(ctx, r)()).To(Equal(store.RunStateParked))
	})

	Describe("for a task no person created", func() {
		// trigger makes a task a trigger created (not a chat), with the notify columns given.
		trigger := func(ctx SpecContext, notify func(p *store.CreateTaskParams)) {
			GinkgoHelper()
			a.args = map[string]any{"question": "Ship the report?", "choices": []string{"Yes", "No"}, "default": "No"}
			p := store.CreateTaskParams{
				OrgID: a.org, Title: uniq("scheduled"), AgentID: a.agent.ID, CreatedByKind: store.TaskCreatorKindTrigger,
				CreatedByTriggerID: pgtype.UUID{Bytes: [16]byte{0xc0, 1}, Valid: true},
			}
			if notify != nil {
				notify(&p)
			}
			var err error
			t, err = q.CreateTask(ctx, p)
			Expect(err).NotTo(HaveOccurred())
			a.message(ctx, t, markQuestion)
			r = a.enqueue(ctx, t)
		}
		addUser := func(ctx SpecContext, role store.OrgRole, member bool) pgtype.UUID {
			GinkgoHelper()
			var personal, id pgtype.UUID
			// (A deleted personal org: the manual-owner rule is checked for live orgs only.)
			Expect(db.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ($1, 'Personal', 'personal', now()) RETURNING id`, uniq("org")).Scan(&personal)).To(Succeed())
			Expect(db.QueryRow(ctx, `INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`, uniq("sub"), personal).Scan(&id)).To(Succeed())
			if member {
				_, err := db.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', $3)`, a.org, id, role)
				Expect(err).NotTo(HaveOccurred())
			}
			return id
		}
		recipients := func(ctx SpecContext, id string) []pgtype.UUID {
			GinkgoHelper()
			ids, err := q.ListActiveQuestionRecipientIDs(ctx, store.ListActiveQuestionRecipientIDsParams{OrgID: a.org, QuestionID: mustUUID(id)})
			Expect(err).NotTo(HaveOccurred())
			return ids
		}
		asked := func(ctx SpecContext) string {
			GinkgoHelper()
			a.pool("agentd-a")
			Eventually(a.ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&qid))
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
			return qid
		}
		// toolText is the text the harness's ask_user call got back.
		toolText := make(chan string, 1)
		capture := func() {
			a.harness.mu.Lock()
			a.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
				if !strings.Contains(text, markQuestion) {
					return "", false
				}
				out, id := a.call(ctx, a.args)
				toolText <- out
				a.ids <- id
				return "Asked. " + out, true
			}
			a.harness.mu.Unlock()
		}

		BeforeEach(func(ctx SpecContext) { a = newAskEnv(ctx) })

		It("asks the org's admins when the agent leaves out 'to'", func(ctx SpecContext) {
			trigger(ctx, nil)
			admin := addUser(ctx, store.OrgRoleAdmin, true)
			id := asked(ctx)
			row := a.question(ctx, id)
			Expect(row.TargetKind).To(Equal(store.NotifyTargetKindRole))
			Expect(row.TargetRole.OrgRole).To(Equal(store.OrgRoleAdmin))
			Expect(recipients(ctx, id)).To(ConsistOf(a.user, admin))
		})

		It("asks the trigger's notify person", func(ctx SpecContext) {
			member := addUser(ctx, store.OrgRoleMember, true)
			trigger(ctx, func(p *store.CreateTaskParams) {
				p.NotifyKind = store.NullNotifyTargetKind{NotifyTargetKind: store.NotifyTargetKindUser, Valid: true}
				p.NotifyUserID = member
			})
			id := asked(ctx)
			Expect(recipients(ctx, id)).To(Equal([]pgtype.UUID{member}))
			Expect(a.question(ctx, id).TargetKind).To(Equal(store.NotifyTargetKindUser))
		})

		It("falls back to the admins when the notify person left", func(ctx SpecContext) {
			gone := addUser(ctx, store.OrgRoleMember, false)
			trigger(ctx, func(p *store.CreateTaskParams) {
				p.NotifyKind = store.NullNotifyTargetKind{NotifyTargetKind: store.NotifyTargetKindUser, Valid: true}
				p.NotifyUserID = gone
			})
			capture()
			id := asked(ctx)
			Expect(recipients(ctx, id)).To(Equal([]pgtype.UUID{a.user}))
			Expect(a.question(ctx, id).TargetKind).To(Equal(store.NotifyTargetKindRole))
			Expect(<-toolText).To(ContainSubstring("no longer a member who can answer"))
		})

		It("falls back when the task's creator left", func(ctx SpecContext) {
			creator := addUser(ctx, store.OrgRoleMember, true)
			var err error
			t, err = q.CreateTask(ctx, store.CreateTaskParams{
				OrgID: a.org, Title: uniq("task"), AgentID: a.agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: creator,
			})
			Expect(err).NotTo(HaveOccurred())
			a.message(ctx, t, markQuestion)
			r = a.enqueue(ctx, t)
			_, err = db.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, a.org, creator)
			Expect(err).NotTo(HaveOccurred())
			a.args = map[string]any{"question": "Ship the report?", "default": "No"}
			capture()
			id := asked(ctx)
			Expect(recipients(ctx, id)).To(Equal([]pgtype.UUID{a.user}), "the admins, not an error")
			Expect(<-toolText).To(ContainSubstring("the task's creator is no longer a member who can answer"))
		})
	})
})
