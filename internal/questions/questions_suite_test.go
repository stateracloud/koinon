// SPDX-License-Identifier: Apache-2.0

package questions_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var (
	pool *pgxpool.Pool
	q    *store.Queries
)

func TestQuestions(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Questions Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
})

// world is an org with an owner, an agent, a task and a running run.
type world struct {
	org, user, agent, task pgtype.UUID
	run                    store.Run
}

func newWorld(ctx context.Context) world {
	GinkgoHelper()
	w := world{}
	w.org, w.user, _ = pgtest.NewOrg(ctx, pool)
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: w.org, DisplayName: pgtest.Uniq("agent"), Harness: harness.OpenCode, CreatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: w.org, AgentID: a.ID, Config: []byte(`{"model":"fake/m","instructions":"x"}`), CreatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: w.org, ID: a.ID, VersionID: v.ID, ActivatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	w.agent = a.ID
	t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: w.org, Title: "t", AgentID: a.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: w.user})
	Expect(err).NotTo(HaveOccurred())
	w.task = t.ID
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: w.org, AgentID: a.ID, TaskID: t.ID, MaxWallClockSeconds: 600})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	w.run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	return w
}

// member adds another user to the org.
func (w world) member(ctx context.Context) pgtype.UUID {
	GinkgoHelper()
	_, id, _ := pgtest.NewOrg(ctx, pool)
	_, err := pool.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'member')`, w.org, id)
	Expect(err).NotTo(HaveOccurred())
	return id
}
