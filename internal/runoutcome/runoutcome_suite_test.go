// SPDX-License-Identifier: Apache-2.0

package runoutcome_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var (
	pool *pgxpool.Pool
	q    *store.Queries
)

func TestRunOutcome(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Runoutcome Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
})

// world is a run of a new task of a new agent in a new org, and the Owner of it.
type world struct {
	org, user, agent, task pgtype.UUID
	run                    store.Run
	own                    *runoutcome.Owner
	turn                   store.RunTurn
	metrics                *counts
}

// counts records what the Owner's metrics were told.
type counts struct {
	finished []store.RunState
	requeued int
	capped   []string
	// writeFailed is each run event Record gave up on, as "class/reason".
	writeFailed []string
}

func (c *counts) Finished(s store.RunState) { c.finished = append(c.finished, s) }
func (c *counts) Requeued()                 { c.requeued++ }
func (c *counts) SandboxWaitCap(cause execution.SandboxWaitCause) {
	c.capped = append(c.capped, string(cause))
}

func (c *counts) EventWriteFailed(class runoutcome.EventClass, reason runoutcome.WriteFailure) {
	c.writeFailed = append(c.writeFailed, class.String()+"/"+string(reason))
}

// newWorld claims a run; running moves it to running, otherwise it stays starting.
func newWorld(ctx context.Context, running bool) *world {
	GinkgoHelper()
	w := &world{metrics: &counts{}}
	w.org, w.user, _ = pgtest.NewOrg(ctx, pool)
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: w.org, DisplayName: pgtest.Uniq("agent"), Harness: harness.OpenCode, CreatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: w.org, AgentID: a.ID, Config: []byte(`{"model":"fake/m","instructions":"x"}`), CreatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: w.org, ID: a.ID, VersionID: v.ID, ActivatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: w.org, Title: "t", AgentID: a.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: w.user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: w.org, AgentID: a.ID, TaskID: t.ID, MaxWallClockSeconds: 1800})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	if running {
		run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch})
		Expect(err).NotTo(HaveOccurred())
	}
	w.agent, w.task, w.run = a.ID, t.ID, run
	w.own = runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: run, Metrics: w.metrics})
	return w
}

// openTurn starts the run's trace turn row.
func (w *world) openTurn(ctx context.Context) runoutcome.Turn {
	GinkgoHelper()
	t, err := q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: w.org, RunID: w.run.ID, TraceID: []byte("0123456789abcdef")})
	Expect(err).NotTo(HaveOccurred())
	w.turn = t
	return runoutcome.Turn{ID: t.ID, Reason: "failed"}
}

func (w *world) turnRow(ctx context.Context) store.RunTurn {
	GinkgoHelper()
	t, err := q.GetRunTurn(ctx, store.GetRunTurnParams{OrgID: w.org, RunID: w.run.ID, Seq: w.turn.Seq})
	Expect(err).NotTo(HaveOccurred())
	return t
}

func (w *world) current(ctx context.Context) store.Run {
	GinkgoHelper()
	r, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: w.run.ID})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func (w *world) taskState(ctx context.Context) store.TaskState {
	GinkgoHelper()
	t, err := q.GetTask(ctx, store.GetTaskParams{OrgID: w.org, ID: w.task})
	Expect(err).NotTo(HaveOccurred())
	return t.State
}

// says is the agent's message, as the harness's chunk run event.
func (w *world) says(ctx context.Context, owner *runoutcome.Owner, text string) {
	GinkgoHelper()
	Expect(owner.RecordRaw(ctx, "acp.agent_message_chunk",
		[]byte(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+text+`"}}`))).To(Succeed())
}

// runKinds are the kinds of the run's events, in order.
func (w *world) runKinds(ctx context.Context) []string {
	GinkgoHelper()
	evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: w.org, RunID: w.run.ID, MaxEvents: 1000})
	Expect(err).NotTo(HaveOccurred())
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// taskEvents are the task's events, in order.
func (w *world) taskEvents(ctx context.Context) []store.TaskEvent {
	GinkgoHelper()
	evs, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: w.org, TaskID: w.task, MaxEvents: 1000})
	Expect(err).NotTo(HaveOccurred())
	return evs
}

func (w *world) taskKinds(ctx context.Context) []string {
	GinkgoHelper()
	var kinds []string
	for _, e := range w.taskEvents(ctx) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// pendingApproval is an ask-tier request of the run that nobody decided.
func (w *world) pendingApproval(ctx context.Context) store.ApprovalRequest {
	GinkgoHelper()
	// The table checks args_sha256 against the SHA-256 of args_json.
	const args = `{}`
	sum := sha256.Sum256([]byte(args))
	req, err := q.CreateApprovalRequest(ctx, store.CreateApprovalRequestParams{
		OrgID: w.org, TaskID: w.task, RunID: w.run.ID, AgentID: w.agent,
		Connector: "github", Tool: "comment_on_issue", Target: "github:acme/widgets",
		ArgsJson: args, ArgsSha256: hex.EncodeToString(sum[:]), Risk: []byte(`{}`),
		Decision: store.ApprovalDecisionAsk, Status: store.ApprovalStatusPending,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	Expect(err).NotTo(HaveOccurred())
	return req
}
