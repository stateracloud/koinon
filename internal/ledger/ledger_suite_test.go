// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
)

// postgresImage is the minimum supported Postgres version (ADR 0047).
const postgresImage = "postgres:18-alpine"

var (
	db  *pgxpool.Pool
	q   *store.Queries
	ing *ledger.Ingester
)

func TestLedger(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ledger")
}

func dockerAvailable(ctx context.Context) error {
	p, err := testcontainers.NewDockerProvider()
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	return p.Health(ctx)
}

// needDocker skips the spec without a container runtime. Decoding and limit specs run anyway.
func needDocker() {
	if db == nil {
		Skip("Docker is not available")
	}
}

var _ = BeforeSuite(func(ctx SpecContext) {
	if err := dockerAvailable(ctx); err != nil {
		GinkgoWriter.Println("Docker is not available; Postgres specs are skipped:", err)
		return
	}
	ctr, err := postgres.Run(ctx, postgresImage, postgres.WithDatabase("koinon"), postgres.BasicWaitStrategies())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())
	db, err = pgxpool.New(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(db.Close)

	sqlDB := stdlib.OpenDBFromPool(db)
	DeferCleanup(sqlDB.Close)
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.Up(ctx)
	Expect(err).NotTo(HaveOccurred())
	q = store.New(db)
	ing = &ledger.Ingester{DB: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
func newEnv(ctx context.Context, slug, agentID, spendUSD string, maxTokens int64) *env {
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
		`INSERT INTO agents (org_id, id, display_name, flavor, created_by)
		 VALUES ($1, coalesce(nullif($2, '')::uuid, uuidv7()), $3, 'opencode', $4) RETURNING id`,
		e.org, agentID, uniq("agent"), e.user).Scan(&id)).To(Succeed())
	cfg := fmt.Sprintf(`{"instructions":"","limits":{"run":{"maxDuration":"30m","maxTokens":%d},"spendUSD":%q}}`, maxTokens, spendUSD)
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

func (e *env) ticket(ctx context.Context) store.Ticket {
	t, err := q.CreateTicket(ctx, store.CreateTicketParams{
		OrgID: e.org, Title: uniq("ticket"), AgentID: e.agent.ID, CreatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	return t
}

// startRun queues a run for t and moves it to running as of startedAt, as agentd would.
func (e *env) startRun(ctx context.Context, t store.Ticket, maxTokens int64, startedAt time.Time) store.Run {
	r, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: e.org, AgentID: e.agent.ID, TicketID: t.ID, MaxWallClockSeconds: 1800, MaxTokens: maxTokens,
	})
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
