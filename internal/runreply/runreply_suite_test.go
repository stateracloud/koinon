// SPDX-License-Identifier: Apache-2.0

package runreply_test

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

func TestRunReply(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Runreply Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
})

// newRun is a running run of a new task in a new org.
func newRun(ctx context.Context) (store.Run, pgtype.UUID) {
	GinkgoHelper()
	org, user, _ := pgtest.NewOrg(ctx, pool)
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: pgtest.Uniq("agent"), Harness: harness.OpenCode, CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{"model":"fake/m","instructions":"x"}`), CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: org, Title: "t", AgentID: a.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: a.ID, TaskID: t.ID, MaxWallClockSeconds: 1800})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "test", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	return run, t.ID
}
