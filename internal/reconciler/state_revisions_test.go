// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("Reconciler and the State stream", func() {
	It("stores the applied state as a revision, only when it changed, and still applies as before", func(ctx SpecContext) {
		osg := fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		mg := &fakeModelGateway{}
		cfg := reconciler.Config{
			Prices:  prices.NewResolver(pricing.Embedded()),
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			Logger: slog.New(slog.DiscardHandler),
		}
		r, err := reconciler.New(pool, openshell.New(conn), mg, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())

		org, user := newOrg(ctx)
		newAgentWithConfig(ctx, org, user, `{"instructions":"help"}`)

		Expect(r.ReconcileAll(ctx)).To(Succeed())
		first, err := staterev.Latest(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(first).NotTo(BeNil(), "the pass stored a revision")
		applied := mg.last()
		want := staterev.Snapshot(applied)
		want.Revision = first.Revision
		Expect(proto.Equal(first, want)).To(BeTrue(), "the stored revision is the state the gateway was applied")
		Expect(first.Keys).NotTo(BeEmpty())

		By("no key material in the row")
		var text string
		Expect(pool.QueryRow(ctx, "SELECT snapshot::text FROM gateway_state_revisions WHERE revision = $1", first.Revision).Scan(&text)).To(Succeed())
		for _, k := range mg.issuedKeys() {
			Expect(text).NotTo(ContainSubstring(k.Material))
		}

		By("a pass that changes nothing stores nothing")
		var before int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM gateway_state_revisions").Scan(&before)).To(Succeed())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		var after int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM gateway_state_revisions").Scan(&after)).To(Succeed())
		Expect(after).To(Equal(before))
	})
})

// stuckDB is a State store that never answers until the caller gives up, or fails at once.
type stuckDB struct{ err error }

func (d stuckDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if d.err != nil {
		return nil, d.err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

var _ = Describe("Reconciler when the State stream store is slow or broken", func() {
	newR := func(ctx SpecContext, mg *fakeModelGateway, db staterev.DB) *reconciler.Reconciler {
		osg := fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		cfg := reconciler.Config{
			Prices:  prices.NewResolver(pricing.Embedded()),
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			Logger:          slog.New(slog.DiscardHandler),
			StateRevisionDB: db,
		}
		r, err := reconciler.New(pool, openshell.New(conn), mg, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
		return r
	}

	for name, db := range map[string]stuckDB{"fails": {err: errors.New("the store is down")}, "blocks": {}} {
		It("still ends the pass with the Apply done when the publish "+name, func(ctx SpecContext) {
			mg := &fakeModelGateway{}
			r := newR(ctx, mg, db)
			org, user := newOrg(ctx)
			newAgentWithConfig(ctx, org, user, `{"instructions":"help"}`)
			start := time.Now()
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(mg.last().Keys).NotTo(BeEmpty(), "the gateway was applied")
			Expect(time.Since(start)).To(BeNumerically("<", 30*time.Second))
		}, SpecTimeout(2*time.Minute))
	}

	It("stores a revision without the key of an agent whose model limit is invalid", func(ctx SpecContext) {
		mg := &fakeModelGateway{}
		r2 := newR(ctx, mg, nil)
		bad := gateway.AgentRef{Org: "badlimit-org", Name: "bad"}
		good := gateway.AgentRef{Org: "badlimit-org", Name: "good"}
		kb, _ := gateway.NewKey(bad)
		kg, _ := gateway.NewKey(good)
		var s gateway.State
		s.AddKey(kb.Key)
		s.AddKey(kg.Key)
		s.SetLimit(bad, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.0000000001")})
		r2.PublishState(ctx, s)
		u, err := staterev.Latest(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		var ids []string
		for _, k := range u.Keys {
			ids = append(ids, k.Id)
		}
		Expect(ids).To(ContainElement(kg.ID))
		Expect(ids).NotTo(ContainElement(kb.ID))
	})
})
