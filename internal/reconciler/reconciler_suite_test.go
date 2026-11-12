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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/pricing"
)

var (
	pool *pgxpool.Pool
	q    *store.Queries
	// ring seals the providers' and connections' secrets of every spec; one keyring for the
	// suite, because a pass reads the providers of every org in the shared database.
	ring = secretstoretest.Keyring()
)

func TestReconciler(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Reconciler Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
}, NodeTimeout(3*time.Minute))

// Every spec starts from an empty database, because a pass walks every org.
var _ = BeforeEach(func(ctx SpecContext) {
	pgtest.TruncateOrgs(ctx, pool)
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

// newAgentWithConfig creates and activates an OpenCode agent whose version has config. The agent is
// keep-warm (the reconciler's default cap is two per org), so its sandbox exists after the first
// pass as most specs expect; a spec about the on-demand start or the idle stop uses newOnDemandAgent.
func newAgentWithConfig(ctx context.Context, org, user pgtype.UUID, config string) store.Agent {
	agent := newOnDemandAgent(ctx, org, user, config)
	keepWarm(ctx, org, agent.ID)
	return agent
}

// keepWarm marks an activated agent keep-warm, so its sandbox exists after the first pass.
func keepWarm(ctx context.Context, org, agent pgtype.UUID) {
	GinkgoHelper()
	_, err := q.SetAgentKeepWarm(ctx, store.SetAgentKeepWarmParams{OrgID: org, ID: agent, KeepWarm: true})
	Expect(err).NotTo(HaveOccurred())
}

// coolDown ends an agent's keep-warm request once its sandbox exists, for the specs about the idle
// stop: from then on the sandbox is stopped when idle and starts again on demand.
func coolDown(ctx context.Context, org pgtype.UUID, agents ...pgtype.UUID) {
	GinkgoHelper()
	for _, a := range agents {
		_, err := q.SetAgentKeepWarm(ctx, store.SetAgentKeepWarmParams{OrgID: org, ID: a, KeepWarm: false})
		Expect(err).NotTo(HaveOccurred())
	}
}

// newOnDemandAgent creates and activates an OpenCode agent that is not keep-warm: its sandbox
// starts with the first run, not with the activation (old issue 315).
func newOnDemandAgent(ctx context.Context, org, user pgtype.UUID, config string) store.Agent {
	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
		OrgID: org, DisplayName: "Repo helper with a long display name that is no sandbox name",
		Harness: harness.OpenCode, CreatedBy: user,
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

// queueRun creates a task for agent and queues a run on it.
func queueRun(ctx context.Context, org, user, agent pgtype.UUID) store.Run {
	t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: org, Title: uniq("task"), AgentID: agent, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: org, AgentID: agent, TaskID: t.ID, MaxWallClockSeconds: 1800,
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

// dirtyHome plays a harness that ran in the agent's sandbox and ended: a run goes starting ->
// running -> succeeded, which stamps agent_sandboxes.home_dirtied_at (old issue 329).
func dirtyHome(ctx context.Context, org, user, agent pgtype.UUID) store.Run {
	GinkgoHelper()
	queueRun(ctx, org, user, agent)
	run := claimRun(ctx, org)
	run, err := q.FinishRun(ctx, store.FinishRunParams{Outcome: store.RunStateSucceeded, OrgID: org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	return run
}

// fakeModelGateway issues real keys and records every applied state.
type fakeModelGateway struct {
	mu      sync.Mutex
	issued  []gateway.IssuedKey
	applied []gateway.State
	fail    error
	// hold, when set, makes Apply wait for it to be closed (a slow agentgateway hot reload);
	// entered is closed when an Apply has started waiting.
	hold    chan struct{}
	entered chan struct{}
	// ledgerBudgets makes the fake report that only a limit of zero binds (Kubernetes mode).
	ledgerBudgets bool
	// onApply, when set, sees each state as the gateway receives it, before it is recorded.
	onApply func(gateway.State)
	// cancelAt is when an Apply saw its context cancelled; applyAt are the starts of every Apply.
	cancelAt time.Time
	applyAt  []time.Time
}

func (f *fakeModelGateway) canceledAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelAt
}

func (f *fakeModelGateway) applyTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.applyAt...)
}

func (f *fakeModelGateway) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
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

func (f *fakeModelGateway) Apply(ctx context.Context, s gateway.State) (gateway.Revision, error) {
	f.mu.Lock()
	hold, entered := f.hold, f.entered
	f.applyAt = append(f.applyAt, time.Now())
	f.mu.Unlock()
	if hold != nil {
		if entered != nil {
			close(entered)
		}
		select {
		case <-hold:
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelAt = time.Now()
			f.mu.Unlock()
			return "", ctx.Err()
		}
	}
	if err := s.Validate(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onApply != nil {
		f.onApply(s)
	}
	var rej *gateway.RejectedError
	if f.fail != nil && !errors.As(f.fail, &rej) {
		return "", f.fail
	}
	f.applied = append(f.applied, s)
	// A RejectedError comes with the revision: the rest of the state is live.
	return gateway.Revision(fmt.Sprint(len(f.applied))), f.fail
}

// LedgerEnforcesBudgets makes the fake a Kubernetes-mode gateway when ledgerBudgets is set.
func (f *fakeModelGateway) LedgerEnforcesBudgets() bool { return f.ledgerBudgets }

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
	_, err = prices.New(pool, nil, pricing.Embedded()).SetOverride(ctx, org, actor, prices.OverrideInput{
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
