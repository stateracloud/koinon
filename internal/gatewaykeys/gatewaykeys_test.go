// SPDX-License-Identifier: Apache-2.0

package gatewaykeys_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the gateway key lifecycle", func() {
	var (
		pool  gatewaykeys.Pool
		sec   gatewaykeys.SecretStore
		admin gatewaykeys.Admin
		ws    secretstore.Wrappers
		now   time.Time
	)
	BeforeEach(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		sec = gatewaykeys.SecretStore{Client: fake.NewClientset(), Namespace: "koinon", Name: "rel-gateway-recipient-key"}
		var err error
		ws, err = secretstore.NewKeyAdminWrappers(secretstore.StaticBoundOnly(secretstore.BoundOnly{}), secretstore.Local{})
		Expect(err).NotTo(HaveOccurred())
		now = time.Now()
		admin = gatewaykeys.Admin{DB: pool, Secrets: sec, Wrap: ws, Replicas: 2, Now: func() time.Time { return now }}
	})

	It("adds a pending key once and writes its wrapped private key", func(ctx SpecContext) {
		id, err := admin.Add(ctx)
		Expect(err).NotTo(HaveOccurred())
		again, err := admin.Add(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(id))
		rows, err := gatewaykeys.List(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].State).To(Equal(gatewaykeys.Pending))
		entries, ok, err := sec.Read(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(entries).To(HaveKey(id))
	})

	It("refuses a Secret entry that no row names", func(ctx SpecContext) {
		Expect(sec.Put(ctx, "gk-0123456789abcdef", secretstore.StoredKEK{Backend: secretstore.BackendLocal, Wrapped: make([]byte, 32)})).To(Succeed())
		_, err := admin.Add(ctx)
		Expect(err).To(MatchError(gatewaykeys.ErrForeignSecret))
	})

	It("activates only after every gateway process reported the key, and retires the old key", func(ctx SpecContext) {
		first, err := admin.Add(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.Activate(ctx)
		Expect(err).To(MatchError(gatewaykeys.ErrNotLoaded))
		Expect(gatewaykeys.ReportLoaded(ctx, pool, first, "gw-a")).To(Succeed())
		Expect(gatewaykeys.ReportLoaded(ctx, pool, first, "gw-b")).To(Succeed())
		_, err = admin.Activate(ctx)
		Expect(err).NotTo(HaveOccurred())

		second, err := admin.Add(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(second).NotTo(Equal(first))
		Expect(gatewaykeys.ReportLoaded(ctx, pool, second, "gw-a")).To(Succeed())
		Expect(gatewaykeys.ReportLoaded(ctx, pool, second, "gw-b")).To(Succeed())
		_, err = admin.Activate(ctx)
		Expect(err).NotTo(HaveOccurred())

		rows, err := gatewaykeys.List(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		states := map[string]gatewaykeys.State{}
		for _, r := range rows {
			states[r.KeyID] = r.State
		}
		Expect(states).To(Equal(map[string]gatewaykeys.State{first: gatewaykeys.Retiring, second: gatewaykeys.Active}))

		By("keeping the old key until the wait is over")
		retired, kept, err := admin.Retire(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(retired).To(BeEmpty())
		Expect(kept).To(HaveKey(first))

		By("retiring it after the wait and deleting its wrapped private key")
		now = now.Add(time.Hour)
		retired, _, err = admin.Retire(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(retired).To(ConsistOf(first))
		entries, _, err := sec.Read(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries).To(HaveKey(second))
	})

	It("never deletes or reopens a key row", func(ctx SpecContext) {
		id, err := admin.Add(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `DELETE FROM gateway_recipient_keys WHERE key_id = $1`, id)
		Expect(err).To(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE gateway_recipient_keys SET state = 'retiring' WHERE key_id = $1`, id)
		Expect(err).To(HaveOccurred())
	})

	Describe("the gateway's load", func() {
		It("unwraps each key, reports it, and exposes only the custody it has", func(ctx SpecContext) {
			id, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			dir := GinkgoT().TempDir()
			entries, _, err := sec.Read(ctx)
			Expect(err).NotTo(HaveOccurred())
			enc, err := entries[id].Encode()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, id), enc, 0o600)).To(Succeed())

			reg := prometheus.NewRegistry()
			l := gatewaykeys.NewLoader(dir, pool, ws, "gw-a", gatewaykeys.NewMetrics(reg))
			Expect(l.Load(ctx)).To(Succeed())
			Expect(l.Keyring.Has(id)).To(BeTrue())
			ok, _ := l.Ready()
			Expect(ok).To(BeTrue())
			by, err := gatewaykeys.LoadedBy(ctx, pool, id, time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(by).To(ConsistOf("gw-a"))
			Expect(testutil.CollectAndCount(reg, "koinon_gateway_recipient_key_custody")).To(Equal(2))
		})

		It("is not ready when a row's private key is missing", func(ctx SpecContext) {
			_, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			l := gatewaykeys.NewLoader(GinkgoT().TempDir(), pool, ws, "gw-a", nil)
			Expect(l.Load(ctx)).NotTo(Succeed())
			ok, why := l.Ready()
			Expect(ok).To(BeFalse())
			Expect(why).To(HaveOccurred())
		})

		It("is ready with no key rows yet", func(ctx SpecContext) {
			l := gatewaykeys.NewLoader(GinkgoT().TempDir(), pool, ws, "gw-a", nil)
			Expect(l.Load(ctx)).To(Succeed())
		})
	})

	Describe("koinon-api's public key", func() {
		It("is the active key only", func(ctx SpecContext) {
			var p gatewaykeys.PublicKeys
			Expect(p.Refresh(ctx, pool)).To(Succeed())
			_, err := p.Current()
			Expect(err).To(MatchError(gatewaykeys.ErrNoActiveKey))
			id, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Refresh(ctx, pool)).To(Succeed())
			_, err = p.Current()
			Expect(err).To(MatchError(gatewaykeys.ErrNoActiveKey))
			Expect(gatewaykeys.ReportLoaded(ctx, pool, id, "a")).To(Succeed())
			Expect(gatewaykeys.ReportLoaded(ctx, pool, id, "b")).To(Succeed())
			_, err = admin.Activate(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Refresh(ctx, pool)).To(Succeed())
			k, err := p.Current()
			Expect(err).NotTo(HaveOccurred())
			Expect(k.ID()).To(Equal(id))
		})
	})
})
