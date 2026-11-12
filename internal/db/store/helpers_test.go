// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

const (
	lease     = 60
	maxTokens = 200_000
)

// fixture is one org with an owner, an active agent and a task for it.
type fixture struct {
	org, user pgtype.UUID
	agent     store.Agent
	version   store.AgentVersion
	task      store.Task
}

// newOrg creates a team org with a manual owner.
func newOrg(ctx context.Context) (org, user pgtype.UUID) {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Personal', 'personal') RETURNING id`,
			uniq("org")).Scan(&org); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			uniq("sub"), org).Scan(&user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`,
			org, user)
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	return org, user
}

// newAgent creates a draft agent in org with one version and activates it.
func newAgent(ctx context.Context, org, user pgtype.UUID) (store.Agent, store.AgentVersion) {
	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
		OrgID: org, DisplayName: uniq("agent"), Harness: "opencode", CreatedBy: user,
	})
	Expect(err).NotTo(HaveOccurred())
	version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
		OrgID: org, AgentID: agent.ID, Config: []byte(`{"instructions":"help"}`), CreatedBy: user,
	})
	Expect(err).NotTo(HaveOccurred())
	agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{
		OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user,
	})
	Expect(err).NotTo(HaveOccurred())
	return agent, version
}

func newTask(ctx context.Context, org, user, agent pgtype.UUID) store.Task {
	t, err := q.CreateTask(ctx, store.CreateTaskParams{
		OrgID: org, Title: uniq("task"), AgentID: agent, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user,
	})
	Expect(err).NotTo(HaveOccurred())
	return t
}

func newFixture(ctx context.Context) fixture {
	var f fixture
	f.org, f.user = newOrg(ctx)
	f.agent, f.version = newAgent(ctx, f.org, f.user)
	f.task = newTask(ctx, f.org, f.user, f.agent.ID)
	return f
}

// enqueue queues a run of agent for task.
func enqueue(ctx context.Context, org, agent, task pgtype.UUID) store.Run {
	r, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: org, AgentID: agent, TaskID: task, MaxWallClockSeconds: 1800,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(r.State).To(Equal(store.RunStateQueued))
	return r
}

func claim(ctx context.Context, org pgtype.UUID, owner string) (store.Run, error) {
	return q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: owner, LeaseSeconds: lease})
}

func mustClaim(ctx context.Context, org pgtype.UUID, owner string) store.Run {
	r, err := claim(ctx, org, owner)
	Expect(err).NotTo(HaveOccurred())
	Expect(r.State).To(Equal(store.RunStateStarting))
	return r
}

func markRunning(ctx context.Context, r store.Run) store.Run {
	r, err := q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func finish(ctx context.Context, r store.Run, outcome store.RunState) (store.Run, error) {
	return q.FinishRun(ctx, store.FinishRunParams{
		OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch, Outcome: outcome,
		OutcomeReason: pgtype.Text{String: "test", Valid: true},
	})
}

func park(ctx context.Context, r store.Run) (store.Run, error) {
	return q.ParkRun(ctx, store.ParkRunParams{
		OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch, ParkReason: store.RunParkReasonApproval,
	})
}

func cancel(ctx context.Context, r store.Run) (store.Run, error) {
	return q.CancelRun(ctx, store.CancelRunParams{OrgID: r.OrgID, ID: r.ID, OutcomeReason: "user canceled"})
}

func getRun(ctx context.Context, r store.Run) store.Run {
	got, err := q.GetRun(ctx, store.GetRunParams{OrgID: r.OrgID, ID: r.ID})
	Expect(err).NotTo(HaveOccurred())
	return got
}

// runIn returns a run of a fresh fixture in the given state, reached through
// the queries.
func runIn(ctx context.Context, state store.RunState) store.Run {
	f := newFixture(ctx)
	r := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
	var err error
	switch state {
	case store.RunStateQueued:
		return r
	case store.RunStateCanceled:
		r, err = cancel(ctx, r)
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	r = mustClaim(ctx, f.org, "agentd-a")
	if state == store.RunStateStarting {
		return r
	}
	r = markRunning(ctx, r)
	switch state {
	case store.RunStateRunning:
	case store.RunStateParked:
		r, err = park(ctx, r)
	default:
		r, err = finish(ctx, r, state)
	}
	Expect(err).NotTo(HaveOccurred())
	Expect(r.State).To(Equal(state))
	return r
}

// expireLease moves the run's lease into the past, as if its owner died.
func expireLease(ctx context.Context, r store.Run) {
	_, err := pool.Exec(ctx,
		`UPDATE runs SET lease_expires_at = now() - interval '1 second' WHERE org_id = $1 AND id = $2`,
		r.OrgID, r.ID)
	Expect(err).NotTo(HaveOccurred())
}

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func numeric(s string) pgtype.Numeric {
	var n pgtype.Numeric
	Expect(n.Scan(s)).To(Succeed())
	return n
}
