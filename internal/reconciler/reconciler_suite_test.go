// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
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

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/pricing"
)

// postgresImage is the minimum supported Postgres version (ADR 0047).
const postgresImage = "postgres:18-alpine"

var (
	pool *pgxpool.Pool
	q    *store.Queries
	// ring seals the providers' and connections' secrets of every spec; one keyring for the
	// suite, because a pass reads the providers of every org in the shared database.
	ring = secretstoretest.Keyring()
)

func TestReconciler(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Reconciler Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("koinon"),
		postgres.BasicWaitStrategies(),
	)
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

// Every spec starts from an empty database, because a pass walks every org.
var _ = BeforeEach(func(ctx SpecContext) {
	_, err := pool.Exec(ctx, `TRUNCATE orgs CASCADE`)
	Expect(err).NotTo(HaveOccurred())
})

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// newOrg creates an org with a manual owner.
func newOrg(ctx context.Context) (org, user pgtype.UUID) {
	return newOrgWithID(ctx, "")
}

// newOrgWithID is newOrg with a fixed org UUID; empty lets the database choose one.
func newOrgWithID(ctx context.Context, id string) (org, user pgtype.UUID) {
	return newOrgWith(ctx, id, uniq("org"))
}

// newOrgWithSlug is newOrg with a fixed slug.
func newOrgWithSlug(ctx context.Context, slug string) (org, user pgtype.UUID) {
	return newOrgWith(ctx, "", slug)
}

func newOrgWith(ctx context.Context, id, slug string) (org, user pgtype.UUID) {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (id, slug, display_name, kind)
			 VALUES (coalesce(nullif($2, '')::uuid, uuidv7()), $1, 'Personal', 'personal') RETURNING id`,
			slug, id).Scan(&org); err != nil {
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

// newAgent creates and activates an OpenCode agent.
func newAgent(ctx context.Context, org, user pgtype.UUID) store.Agent {
	return newAgentWithConfig(ctx, org, user, `{"instructions":"help"}`)
}

// newAgentWithConfig creates and activates an OpenCode agent whose version has config.
func newAgentWithConfig(ctx context.Context, org, user pgtype.UUID, config string) store.Agent {
	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
		OrgID: org, DisplayName: "Repo helper with a long display name that is no sandbox name",
		Flavor: store.AgentFlavorOpencode, CreatedBy: user,
	})
	Expect(err).NotTo(HaveOccurred())
	version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
		OrgID: org, AgentID: agent.ID, Config: []byte(config), CreatedBy: user,
	})
	Expect(err).NotTo(HaveOccurred())
	agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{
		OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user,
	})
	Expect(err).NotTo(HaveOccurred())
	return agent
}

// queueRun creates a ticket for agent and queues a run on it.
func queueRun(ctx context.Context, org, user, agent pgtype.UUID) store.Run {
	t, err := q.CreateTicket(ctx, store.CreateTicketParams{OrgID: org, Title: uniq("ticket"), AgentID: agent, CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: org, AgentID: agent, TicketID: t.ID, MaxWallClockSeconds: 1800, MaxTokens: 200_000,
	})
	Expect(err).NotTo(HaveOccurred())
	return run
}

// claimRun takes the org's next run as agentd would and marks it running.
func claimRun(ctx context.Context, org pgtype.UUID) store.Run {
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "agentd-test", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	return run
}

// fakeModelGateway issues real keys and records every applied state.
type fakeModelGateway struct {
	mu      sync.Mutex
	issued  []gateway.IssuedKey
	applied []gateway.State
	fail    error
}

func (f *fakeModelGateway) IssueKey(_ context.Context, agent gateway.AgentRef) (gateway.IssuedKey, error) {
	k, err := gateway.NewKey(agent)
	if err != nil {
		return k, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued = append(f.issued, k)
	return k, nil
}

func (f *fakeModelGateway) Apply(_ context.Context, s gateway.State) (gateway.Revision, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var rej *gateway.RejectedError
	if f.fail != nil && !errors.As(f.fail, &rej) {
		return "", f.fail
	}
	f.applied = append(f.applied, s)
	// A RejectedError comes with the revision: the rest of the state is live.
	return gateway.Revision(fmt.Sprint(len(f.applied))), f.fail
}

func (f *fakeModelGateway) Usage() gateway.UsageReader { return gateway.NoUsage{} }

func (f *fakeModelGateway) issuedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issued)
}

func (f *fakeModelGateway) last() gateway.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	Expect(f.applied).NotTo(BeEmpty())
	return f.applied[len(f.applied)-1]
}

// syncBuffer is a log sink safe for the Run goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// addProvider gives the org an OpenAI-compatible provider with one priced model, as the
// admin API would.
func addProvider(ctx context.Context, org, user pgtype.UUID, name, key, model string) catalog.Provider {
	GinkgoHelper()
	svc := catalog.New(pool, ring, nil)
	actor := audit.User(user)
	p, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{
		Name: name, Type: "openai", URL: "http://fakemodel.example.com:8080/v1", APIKey: key,
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{
		ProviderID: p.ID, Name: model,
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = prices.New(pool, nil).SetOverride(ctx, org, actor, prices.OverrideInput{
		Entry: pricing.Entry{
			ID: "price-" + strings.NewReplacer("/", "-", "_", "-").Replace(strings.ToLower(model)), Provider: "openai", Match: pricing.Match{Glob: model},
			Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}},
			Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
		},
		ValidFrom: time.Now().Add(-time.Hour),
	})
	Expect(err).NotTo(HaveOccurred())
	return p
}

// publicDNS resolves every name to a public address, so specs do not depend on real DNS.
var testPolicy = catalog.URLPolicy{
	Resolver: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	},
}
