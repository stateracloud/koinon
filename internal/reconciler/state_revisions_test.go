// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"log/slog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/harness"
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
