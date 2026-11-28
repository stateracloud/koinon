// SPDX-License-Identifier: Apache-2.0

package actornames_test

import (
	"context"
	"sync"
	"testing"
	"time"

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

// rows counts the ledger rows of atespace.
func rows(ctx context.Context, p *pgxpool.Pool, atespace string) int {
	GinkgoHelper()
	var n int
	Expect(p.QueryRow(ctx, `SELECT count(*) FROM substrate_actor_names WHERE atespace = $1`, atespace).Scan(&n)).To(Succeed())
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
		wsA, wsB := pgtest.Uniq("kw"), pgtest.Uniq("kw")
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
		Expect(rows(ctx, p, wsA)).To(Equal(1))
		fresh, err = b.ClaimActorName(ctx, wsB, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(BeTrue(), "another atespace")
		Expect(rows(ctx, p, wsB)).To(Equal(1))
	})

	It("gives exactly one of many concurrent claims of a name", func(ctx SpecContext) {
		p := newPool(ctx)
		l := actornames.New(p)
		ws := pgtest.Uniq("kw")
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
		Expect(rows(ctx, p, ws)).To(Equal(1))
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
		ws := pgtest.Uniq("kw")
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
