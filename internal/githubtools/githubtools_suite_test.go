// SPDX-License-Identifier: Apache-2.0

package githubtools_test

import (
	"context"
	"fmt"
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
)

// postgresImage is the minimum supported Postgres version (ADR 0047).
const postgresImage = "postgres:18-alpine"

var (
	pool *pgxpool.Pool
	q    *store.Queries
)

func TestGitHubTools(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GitHub Tools Suite")
}

// needDB skips specs that need Postgres when Docker is not available, so the
// pure validation specs still run.
var dbErr error

var _ = BeforeSuite(func(ctx SpecContext) {
	p, err := testcontainers.NewDockerProvider()
	if err == nil {
		err = p.Health(ctx)
		_ = p.Close()
	}
	if err != nil {
		dbErr = err
		return
	}
	ctr, err := postgres.Run(ctx, postgresImage, postgres.WithDatabase("koinon"), postgres.BasicWaitStrategies())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())
	pool, err = pgxpool.New(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	DeferCleanup(db.Close)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.Up(ctx)
	Expect(err).NotTo(HaveOccurred())
	q = store.New(pool)
}, NodeTimeout(3*time.Minute))

func needDB() {
	if dbErr != nil {
		Skip("Docker is not available: " + dbErr.Error())
	}
}

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// fixture is an org with an active agent whose run is running.
type fixture struct {
	org     store.Org
	agent   store.Agent
	run     store.Run
	agentID string // the gateway's agent header value
}

// newFixture creates an org, an active agent with config and a running run.
func newFixture(ctx context.Context, config string) fixture {
	GinkgoHelper()
	var f fixture
	var orgID, user pgtype.UUID
	slug := uniq("org")
	Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Team', 'personal') RETURNING id`, slug).Scan(&orgID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			uniq("sub"), orgID).Scan(&user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, orgID, user)
		return err
	})).To(Succeed())
	var err error
	f.org, err = q.GetOrgBySlug(ctx, slug)
	Expect(err).NotTo(HaveOccurred())
	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: orgID, DisplayName: "helper", Flavor: store.AgentFlavorOpencode, CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: orgID, AgentID: agent.ID, Config: []byte(config), CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	f.agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: orgID, ID: agent.ID, VersionID: v.ID, ActivatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	t, err := q.CreateTicket(ctx, store.CreateTicketParams{OrgID: orgID, Title: "fix the typo", AgentID: f.agent.ID, CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: orgID, AgentID: f.agent.ID, TicketID: t.ID, MaxWallClockSeconds: 1800, MaxTokens: 200_000})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: orgID, OwnerID: "agentd-test", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	f.run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: orgID, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	f.agentID = slug + "/" + f.agent.ID.String()
	return f
}
