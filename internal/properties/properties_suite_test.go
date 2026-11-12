// SPDX-License-Identifier: Apache-2.0

package properties_test

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

func TestProperties(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Properties Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
})

// newOrg makes an org with a user.
func newOrg(ctx context.Context) (org, user pgtype.UUID) {
	GinkgoHelper()
	org, user, _ = pgtest.NewOrg(ctx, pool)
	return org, user
}

// newAgent makes an active agent.
func newAgent(ctx context.Context, org, user pgtype.UUID) pgtype.UUID {
	GinkgoHelper()
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: pgtest.Uniq("agent"), Harness: harness.OpenCode, CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{"model":"fake/m","instructions":"x"}`), CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	return a.ID
}

// newTask makes a task for the agent.
func newTask(ctx context.Context, org, user, agent pgtype.UUID) store.Task {
	GinkgoHelper()
	t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: org, Title: "t", AgentID: agent, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
	Expect(err).NotTo(HaveOccurred())
	return t
}
