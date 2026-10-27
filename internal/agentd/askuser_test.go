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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/askuser"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/questions"
)

// markQuestion makes the fake harness call ask_user through Koinon's tool server.
const markQuestion = "QUESTION"

// askEnv is an env whose agent can call ask_user on the real tool server.
type askEnv struct {
	*env
	slug  string
	tools *httptest.Server
	mu    sync.Mutex
	args  map[string]any
	ids   chan string
}

func newAskEnv(ctx context.Context) *askEnv {
	a := &askEnv{env: newEnv(ctx, store.AgentFlavorOpencode), ids: make(chan string, 4)}
	Expect(db.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, a.org).Scan(&a.slug)).To(Succeed())
	a.serve()
	a.args = map[string]any{
		"question": "Which region should I deploy to?", "choices": []string{"eu-west-1", "us-east-1"},
		"default": "eu-west-1",
	}
	a.harness.mu.Lock()
	a.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
		if !strings.Contains(text, markQuestion) {
			return "", false
		}
		a.mu.Lock()
		args := a.args
		a.mu.Unlock()
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
	if a.tools != nil {
		a.tools.Close()
	}
	pool, err := pgxpool.New(context.Background(), db.Config().ConnString())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)
	a.tools = httptest.NewServer(askuser.New(pool, askuser.Options{Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil))}).Handler())
	DeferCleanup(a.tools.Close)
	return pool
}

func (a *askEnv) call(ctx context.Context, args map[string]any) (text, id string) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-harness", Version: "1"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: a.tools.URL, HTTPClient: &http.Client{Transport: headerTransport{agent: a.slug + "/" + a.agent.ID.String()}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return "connect: " + err.Error(), ""
	}
	defer func() { _ = s.Close() }()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: askuser.ToolAskUser, Arguments: args})
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
func resolveIn(ctx context.Context, pool *pgxpool.Pool, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return fn(store.New(tx)) })
}

var _ = Describe("ask_user", func() {
	var (
		a   *askEnv
		t   store.Task
		r   store.Run
		qid string
	)

	// asked runs the task to its park on the question.
	asked := func(ctx SpecContext) {
		a = newAskEnv(ctx)
		t = a.task(ctx, markQuestion+": deploy the app")
		r = a.enqueue(ctx, t)
		a.pool("agentd-a")
		Eventually(a.ids).WithTimeout(10 * time.Second).Should(Receive(&qid))
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateParked))
	}

	It("parks the run with reason input_required, opens the question and moves the task, together", func(ctx SpecContext) {
		asked(ctx)
		parked := getRun(ctx, r)
		Expect(parked.ParkReason.RunParkReason).To(Equal(store.RunParkReasonInputRequired))
		Expect(parked.LeaseExpiresAt.Valid).To(BeFalse(), "the run is released")
		Expect(parked.ParkExpiresAt.Time).To(BeTemporally("~", time.Now().Add(questions.DefaultTTL), time.Minute))

		row := a.question(ctx, qid)
		Expect(row.State).To(Equal(store.QuestionStateOpen))
		Expect(row.AskedTo).To(Equal(a.user), "the default addressee is the task's creator")
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
		Eventually(a.ids).WithTimeout(10 * time.Second).Should(Receive(&qid))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateAsked))
		inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: a.org, UserID: a.user})
		Expect(err).NotTo(HaveOccurred())
		Expect(inbox).To(BeEmpty())
		got, _ := q.GetTask(ctx, store.GetTaskParams{OrgID: a.org, ID: t.ID})
		Expect(got.State).NotTo(Equal(store.TaskStateInputRequired))
		close(release)
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateParked))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateOpen))
	})

	It("resumes the run with the answer as the next message, through session/load", func(ctx SpecContext) {
		asked(ctx)
		pool := a.serve()
		var row store.Question
		Expect(resolveIn(ctx, pool, func(q *store.Queries) (err error) {
			row, err = questions.Respond(ctx, q, a.org, mustUUID(qid), a.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "us-east-1"})
			return err
		})).To(Succeed())
		Expect(row.State).To(Equal(store.QuestionStateAnswered))

		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		prompts, loads, _, _ := a.harness.snapshot()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(HavePrefix("[Koinon] Your run was paused while a human decided"))
		Expect(prompts[1]).To(ContainSubstring(`the person answered: "us-east-1"`))
		Expect(prompts[1]).NotTo(ContainSubstring("default was applied"))
		Expect(loads).To(Equal([]string{"ses_1"}))
		Expect(a.question(ctx, qid).DeliveredAt.Valid).To(BeTrue())
		got, _ := q.GetTask(ctx, store.GetTaskParams{OrgID: a.org, ID: t.ID})
		Expect(got.State).To(Equal(store.TaskStateWorking))
		Expect(kindsOf(taskEvents(ctx, a.env, t))).To(ContainElement(questions.KindAnswered))
	})

	It("waits for a stopped sandbox, then resumes through session/load", func(ctx SpecContext) {
		asked(ctx)
		a.recordSandbox(ctx, store.SandboxStateStopped, "idle")
		Expect(resolveIn(ctx, db, func(q *store.Queries) error {
			_, err := questions.Respond(ctx, q, a.org, mustUUID(qid), a.user, questions.Answer{Kind: store.QuestionAnswerKindText, Text: "ap-south-1, please"})
			return err
		})).To(Succeed())
		Consistently(runState(ctx, r)).WithTimeout(time.Second).ShouldNot(Equal(store.RunStateSucceeded))
		a.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))
		prompts, loads, _, _ := a.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`the person answered: "ap-south-1, please"`))
		Expect(loads).To(Equal([]string{"ses_1"}))
	})

	It("applies the default when the question is dismissed, and says so", func(ctx SpecContext) {
		asked(ctx)
		Expect(resolveIn(ctx, db, func(q *store.Queries) error {
			_, err := questions.Dismiss(ctx, q, a.org, mustUUID(qid), a.user)
			return err
		})).To(Succeed())
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := a.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`dismissed the question, so your default was applied: "eu-west-1"`))
		row := a.question(ctx, qid)
		Expect(row.State).To(Equal(store.QuestionStateDismissed))
		Expect(row.AnswerKind.QuestionAnswerKind).To(Equal(store.QuestionAnswerKindDefault))
	})

	It("applies the default on expiry, and says so", func(ctx SpecContext) {
		asked(ctx)
		Expect(resolveIn(ctx, db, func(q *store.Queries) error {
			_, err := questions.Expire(ctx, q, a.org, mustUUID(qid))
			return err
		})).To(Succeed())
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := a.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring(`nobody answered in time, so your default was applied: "eu-west-1"`))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateExpired))
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
				results <- resolveIn(ctx, fresh, func(q *store.Queries) error {
					_, err := questions.Respond(ctx, q, a.org, mustUUID(qid), a.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "eu-west-1"})
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

		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
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
		Eventually(texts).WithTimeout(10 * time.Second).Should(Receive(&got))
		Expect(got[0]).To(ContainSubstring(`missing properties: ["default"]`))
		Expect(got[1]).To(ContainSubstring("default is required"))
		Expect(got[2]).To(ContainSubstring("is not a member of this org"))
		Expect(got[3]).To(ContainSubstring("default must be one of the choices"))
		Expect(got[4]).To(ContainSubstring("question recorded"), "an email names a member, whatever its case")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateParked))
		inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: a.org, UserID: a.user})
		Expect(err).NotTo(HaveOccurred())
		Expect(inbox).To(HaveLen(1), "only the valid question exists")
	})

	It("refuses an answer from someone else", func(ctx SpecContext) {
		asked(ctx)
		err := resolveIn(ctx, db, func(q *store.Queries) error {
			_, err := questions.Respond(ctx, q, a.org, mustUUID(qid), pgtype.UUID{Bytes: [16]byte{9}, Valid: true}, questions.Answer{Kind: store.QuestionAnswerKindText, Text: "x"})
			return err
		})
		Expect(err).To(MatchError(questions.ErrNotAskedTo))
		Expect(a.question(ctx, qid).State).To(Equal(store.QuestionStateOpen))
		Expect(runState(ctx, r)()).To(Equal(store.RunStateParked))
	})
})
