// SPDX-License-Identifier: Apache-2.0

package actornames_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/actornames"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func TestActorNames(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Actor names Suite")
}

var (
	tmpl *pgtest.Template
	dsn  string // a migrated database of the current spec
)

var _ = BeforeSuite(func(ctx context.Context) {
	tmpl = pgtest.StartTemplate(ctx)
})

var _ = BeforeEach(func(ctx context.Context) {
	dsn = tmpl.DSN(ctx)
})

// newPool opens a pool of its own on the spec's database: two pools are two processes.
func newPool(ctx context.Context) *pgxpool.Pool {
	GinkgoHelper()
	p, err := pgxpool.New(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(p.Close)
	return p
}

// newOrg makes a team org and its workspace record, as the reconciler does, and returns the org and its
// atespace.
func newOrg(ctx context.Context, p *pgxpool.Pool) (pgtype.UUID, string) {
	GinkgoHelper()
	org, _, _ := pgtest.NewOrg(ctx, p)
	ws := execution.WorkspaceName(org.Bytes)
	_, err := p.Exec(ctx, `INSERT INTO org_workspaces (org_id, workspace) VALUES ($1, $2)`, org, ws)
	Expect(err).NotTo(HaveOccurred())
	return org, ws
}

// rows counts the ledger rows of org.
func rows(ctx context.Context, p *pgxpool.Pool, org pgtype.UUID) int {
	GinkgoHelper()
	var n int
	Expect(p.QueryRow(ctx, `SELECT count(*) FROM substrate_actor_names WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
	return n
}

// img is a fake actor image pinned by digest, built from two parts as in the substrate specs
// (imageref_test.go), so no line holds a whole image reference.
const (
	digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	img    = "img" + digest
)

var _ = Describe("the database ledger of actor names (issue #1092)", func() {
	It("claims a name once per atespace, also from another process", func(ctx SpecContext) {
		p := newPool(ctx)
		a, b := actornames.New(p), actornames.New(newPool(ctx))
		_, wsA := newOrg(ctx, p)
		_, wsB := newOrg(ctx, p)
		name := pgtest.Uniq("kt")
		fresh, err := a.ClaimActorName(ctx, wsA, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(BeTrue())
		fresh, err = a.ClaimActorName(ctx, wsA, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(BeFalse(), "the same ledger")
		fresh, err = b.ClaimActorName(ctx, wsA, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(BeFalse(), "a ledger of another process")
		fresh, err = b.ClaimActorName(ctx, wsB, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(BeTrue(), "the atespace of another org")
	})

	It("refuses to claim in an atespace that no org has", func(ctx SpecContext) {
		fresh, err := actornames.New(newPool(ctx)).ClaimActorName(ctx, "kw-no-such-org", "kt-1")
		Expect(err).To(MatchError(ContainSubstring("no org has the workspace")))
		Expect(fresh).To(BeFalse())
	})

	It("drops the rows of a deleted org, and keeps the rows of another org", func(ctx SpecContext) {
		p := newPool(ctx)
		l := actornames.New(p)
		orgA, wsA := newOrg(ctx, p)
		orgB, wsB := newOrg(ctx, p)
		name := pgtest.Uniq("kt")
		for _, ws := range []string{wsA, wsB} {
			fresh, err := l.ClaimActorName(ctx, ws, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(fresh).To(BeTrue(), ws)
		}
		Expect(rows(ctx, p, orgA)).To(Equal(1))

		// The org goes for good, with its owner, its membership and its workspace record.
		err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
			for _, q := range []string{
				`DELETE FROM org_memberships WHERE org_id = $1`,
				`DELETE FROM users WHERE personal_org_id = $1`,
				`DELETE FROM org_workspaces WHERE org_id = $1`,
				`DELETE FROM orgs WHERE id = $1`,
			} {
				if _, err := tx.Exec(ctx, q, orgA); err != nil {
					return err
				}
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows(ctx, p, orgA)).To(BeZero(), "the rows go with the org")
		Expect(rows(ctx, p, orgB)).To(Equal(1), "another org's row of the same name stays")
		fresh, err := l.ClaimActorName(ctx, wsB, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(BeFalse(), "the other org still holds its claim")
	})

	It("gives exactly one of many concurrent claims of a name", func(ctx SpecContext) {
		p := newPool(ctx)
		l := actornames.New(p)
		_, ws := newOrg(ctx, p)
		name := pgtest.Uniq("kt")
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			wins int
		)
		for range 16 {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				fresh, err := l.ClaimActorName(ctx, ws, name)
				Expect(err).NotTo(HaveOccurred())
				if fresh {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		Expect(wins).To(Equal(1))
	})

	It("makes a driver refuse the name of a deleted actor after a restart, on the database alone", func(ctx SpecContext) {
		cl := fakeate.New()
		control, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cl.Stop)
		DeferCleanup(control.Close)
		DeferCleanup(router.Close)
		cfg := func(l substrate.ActorNameLedger) substrate.Config {
			return substrate.Config{
				PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond,
				DefaultTier: harness.TierDev, GuestSigner: fakeate.Signer(), ActorNames: l,
				GatewayRoutes: []substrate.GatewayRoute{substrate.ModelRoute(fakeate.GatewayHost, fakeate.GatewayPort)},
			}
		}
		p := newPool(ctx)
		_, ws := newOrg(ctx, p)
		name := pgtest.Uniq("kt")
		spec := execution.SandboxSpec{Name: name, Image: img, CPULimit: "500m", MemoryLimit: "512Mi"}

		first := substrate.New(control, router, cfg(actornames.New(p)))
		_, err = first.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = first.CreateSandbox(ctx, ws, spec)
		Expect(err).NotTo(HaveOccurred())
		_, err = first.DeleteSandbox(ctx, ws, name)
		Expect(err).NotTo(HaveOccurred())

		By("a new driver on a new connection: nothing of the first is in memory")
		restarted := substrate.New(control, router, cfg(actornames.New(newPool(ctx))))
		_, err = restarted.CreateSandbox(ctx, ws, spec)
		Expect(err).To(MatchError(execution.ErrNameReused))
		_, err = restarted.GetSandbox(ctx, ws, name)
		Expect(err).To(MatchError(execution.ErrNotFound))
		Expect(cl.Calls("CreateActor")).To(Equal(1))

		By("a new name works")
		spec.Name = restarted.NewActorName("kt")
		_, err = restarted.CreateSandbox(ctx, ws, spec)
		Expect(err).NotTo(HaveOccurred())
	})
})
