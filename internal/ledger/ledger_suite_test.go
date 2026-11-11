// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
	"github.com/stateracloud/koinon/pricing"
)

var (
	db  *pgxpool.Pool
	q   *store.Queries
	ing *ledger.Ingester
	// priceList is the default list the specs run against.
	priceList pricing.ListSource
)

func TestLedger(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ledger")
}

// needDocker skips the spec without a container runtime (or fails it under KOINON_REQUIRE=docker).
// Decoding and limit specs run anyway.
func needDocker() { testenv.Require(testenv.Docker) }

var _ = BeforeSuite(func(ctx SpecContext) {
	// The specs assert exact money values, so they price against their own fixed list, never the
	// embedded default one that changes with every provider price update.
	list, err := os.ReadFile("testdata/prices.yaml")
	Expect(err).NotTo(HaveOccurred())
	l, rs, err := pricing.ParseList(list)
	Expect(err).NotTo(HaveOccurred())
	priceList = pricing.Static(l, rs)

	if err := testenv.Docker.Err(); err != nil {
		GinkgoWriter.Println("Docker is not available; Postgres specs are skipped:", err)
		return
	}
	// The ledger is the crash-safety surface of billing: it runs on production durability settings.
	db = pgtest.Start(ctx, pgtest.Durable())
	q = store.New(db)
	ing = &ledger.Ingester{DB: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Prices: prices.NewResolver(priceList)}
}, NodeTimeout(3*time.Minute))

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// env is one org with one active agent whose version has the given spend limit.
type env struct {
	org   pgtype.UUID
	slug  string
	user  pgtype.UUID
	agent store.Agent
}

// newEnv creates the org and agent. agentID may be empty for a generated ID.
func newEnv(ctx context.Context, slug, agentID, spendUSD string) *env {
	e := &env{slug: slug}
	// The org needs a manual owner by commit.
	Expect(pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Ledger', 'personal') RETURNING id`, slug).Scan(&e.org); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			uniq("sub"), e.org).Scan(&e.user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, e.org, e.user)
		return err
	})).To(Succeed())
	var id pgtype.UUID
	Expect(db.QueryRow(ctx,
		`INSERT INTO agents (org_id, id, display_name, harness, created_by)
		 VALUES ($1, coalesce(nullif($2, '')::uuid, uuidv7()), $3, 'opencode', $4) RETURNING id`,
		e.org, agentID, uniq("agent"), e.user).Scan(&id)).To(Succeed())
	cfg := fmt.Sprintf(`{"instructions":"","limits":{"run":{"maxDuration":"30m","maxModelRequests":200,"maxSpendUSD":"5.00"},"spendUSD":%q}}`, spendUSD)
	version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
		OrgID: e.org, AgentID: id, Config: []byte(cfg), CreatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	e.agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{
		OrgID: e.org, ID: id, VersionID: version.ID, ActivatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) {
		_, err := db.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'spec ended',
			finished_at = now(), ready_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL
			WHERE org_id = $1 AND state NOT IN ('succeeded', 'failed', 'canceled')`, e.org)
		Expect(err).NotTo(HaveOccurred())
	})
	return e
}

func (e *env) ref() gateway.AgentRef {
	return gateway.AgentRef{Org: e.slug, Name: e.agent.ID.String()}
}

func (e *env) task(ctx context.Context) store.Task {
	t, err := q.CreateTask(ctx, store.CreateTaskParams{
		OrgID: e.org, Title: uniq("task"), AgentID: e.agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	return t
}

// startRun queues a run for t and moves it to running as of startedAt, as agentd would.
func (e *env) startRun(ctx context.Context, t store.Task, startedAt time.Time) store.Run {
	return e.startLimitedRun(ctx, t, runLimits{}, startedAt)
}

// runLimits are the request and spend limits of a test run; zero is none.
type runLimits struct {
	requests int64
	spendUSD string
}

// startLimitedRun is startRun for a run queued with limits (ADR 0081).
func (e *env) startLimitedRun(ctx context.Context, t store.Task, l runLimits, startedAt time.Time) store.Run {
	p := store.CreateRunParams{OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: 1800}
	if l.requests > 0 {
		p.MaxModelRequests = pgtype.Int8{Int64: l.requests, Valid: true}
	}
	if l.spendUSD != "" {
		p.MaxSpendUsd = pgtype.Text{String: l.spendUSD, Valid: true}
	}
	r, err := q.CreateRun(ctx, p)
	Expect(err).NotTo(HaveOccurred())
	_, err = db.Exec(ctx, `UPDATE runs SET state = 'starting', owner_id = 'test', lease_expires_at = now() + interval '1 hour',
		ready_at = NULL, started_at = $3 WHERE org_id = $1 AND id = $2`, e.org, r.ID, startedAt)
	Expect(err).NotTo(HaveOccurred())
	_, err = db.Exec(ctx, `UPDATE runs SET state = 'running' WHERE org_id = $1 AND id = $2`, e.org, r.ID)
	Expect(err).NotTo(HaveOccurred())
	return e.run(ctx, r.ID)
}

// finishRun ends a run as succeeded at finishedAt.
func (e *env) finishRun(ctx context.Context, id pgtype.UUID, finishedAt time.Time) {
	_, err := db.Exec(ctx, `UPDATE runs SET state = 'succeeded', finished_at = $3, lease_expires_at = NULL
		WHERE org_id = $1 AND id = $2`, e.org, id, finishedAt)
	Expect(err).NotTo(HaveOccurred())
}

func (e *env) run(ctx context.Context, id pgtype.UUID) store.Run {
	r, err := q.GetRun(ctx, store.GetRunParams{OrgID: e.org, ID: id})
	Expect(err).NotTo(HaveOccurred())
	return r
}

// entry is a priced request by the env's agent at t.
func (e *env) entry(t time.Time, cost string, in, out int64) ledger.Entry {
	return ledger.Entry{
		RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: "fake/model",
		InputTokens: in, OutputTokens: out, CostUSD: cost, Time: t,
	}
}

func (e *env) spendEntries(ctx context.Context) int {
	var n int
	Expect(db.QueryRow(ctx, `SELECT count(*) FROM spend_entries WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
	return n
}
