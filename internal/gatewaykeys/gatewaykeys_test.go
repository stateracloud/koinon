// SPDX-License-Identifier: Apache-2.0

package gatewaykeys_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// flakyDB fails every call while fail is set.
type flakyDB struct {
	store.DBTX
	fail     atomic.Bool
	failExec atomic.Bool // only writes fail: a demoted, read-only node after a switchover
}

var errDown = errors.New("connection refused")

func (f *flakyDB) Query(ctx context.Context, q string, a ...interface{}) (pgx.Rows, error) {
	if f.fail.Load() {
		return nil, errDown
	}
	return f.DBTX.Query(ctx, q, a...)
}

func (f *flakyDB) Exec(ctx context.Context, q string, a ...interface{}) (pgconn.CommandTag, error) {
	if f.fail.Load() || f.failExec.Load() {
		return pgconn.CommandTag{}, errDown
	}
	return f.DBTX.Exec(ctx, q, a...)
}

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

	It("retires a pending row that has no wrapped private key and makes a new key", func(ctx SpecContext) {
		id, err := admin.Add(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(sec.Delete(ctx, id)).To(Succeed()) // the Put after the row insert failed
		next, err := admin.Add(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(next).NotTo(Equal(id))
		rows, err := gatewaykeys.List(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		states := map[string]gatewaykeys.State{}
		for _, r := range rows {
			states[r.KeyID] = r.State
		}
		Expect(states).To(Equal(map[string]gatewaykeys.State{id: gatewaykeys.Retired, next: gatewaykeys.Pending}))
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

		It("is not ready when the private key of an active row is missing", func(ctx SpecContext) {
			id, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(gatewaykeys.ReportLoaded(ctx, pool, id, "gw-a")).To(Succeed())
			Expect(gatewaykeys.ReportLoaded(ctx, pool, id, "gw-b")).To(Succeed())
			_, err = admin.Activate(ctx)
			Expect(err).NotTo(HaveOccurred())
			l := gatewaykeys.NewLoader(GinkgoT().TempDir(), pool, ws, "gw-a", nil)
			Expect(l.Load(ctx)).NotTo(Succeed())
			ok, why := l.Ready()
			Expect(ok).To(BeFalse())
			Expect(why).To(HaveOccurred())
		})

		It("stays ready while a pending key has no wrapped private key yet, and holds the active key", func(ctx SpecContext) {
			first, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(gatewaykeys.ReportLoaded(ctx, pool, first, "gw-a")).To(Succeed())
			Expect(gatewaykeys.ReportLoaded(ctx, pool, first, "gw-b")).To(Succeed())
			_, err = admin.Activate(ctx)
			Expect(err).NotTo(HaveOccurred())
			dir := GinkgoT().TempDir()
			entries, _, err := sec.Read(ctx)
			Expect(err).NotTo(HaveOccurred())
			enc, err := entries[first].Encode()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, first), enc, 0o600)).To(Succeed())

			By("a rotation that wrote its row and not yet its Secret entry")
			second, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			l := gatewaykeys.NewLoader(dir, pool, ws, "gw-a", nil)
			Expect(l.Load(ctx)).To(Succeed())
			Expect(l.Keyring.Has(first)).To(BeTrue())
			Expect(l.Keyring.Has(second)).To(BeFalse())
			by, err := gatewaykeys.LoadedBy(ctx, pool, second, time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(by).To(BeEmpty(), "a key the gateway does not hold is not reported")
		})

		It("stays ready on its last good keyring while the database fails, and is not ready before a good pass", func(ctx SpecContext) {
			id, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			dir := GinkgoT().TempDir()
			entries, _, err := sec.Read(ctx)
			Expect(err).NotTo(HaveOccurred())
			enc, err := entries[id].Encode()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, id), enc, 0o600)).To(Succeed())

			db := &flakyDB{DBTX: pool}
			l := gatewaykeys.NewLoader(dir, db, ws, "gw-a", nil)
			db.fail.Store(true)
			Expect(l.Load(ctx)).NotTo(Succeed())
			ok, _ := l.Ready()
			Expect(ok).To(BeFalse(), "no good pass yet: nothing to serve")

			db.fail.Store(false)
			Expect(l.Load(ctx)).To(Succeed())
			Expect(l.Keyring.Has(id)).To(BeTrue())

			By("a Postgres switchover")
			db.fail.Store(true)
			Expect(l.Load(ctx)).NotTo(Succeed(), "the caller still logs the error")
			ok, _ = l.Ready()
			Expect(ok).To(BeTrue(), "a transient database error keeps the last good keyring ready")
			Expect(l.Keyring.Has(id)).To(BeTrue())

			By("a read-only node: the list works, the load report fails")
			db.fail.Store(false)
			db.failExec.Store(true)
			Expect(l.Load(ctx)).NotTo(Succeed())
			ok, _ = l.Ready()
			Expect(ok).To(BeTrue(), "a failed load report keeps the pod ready")
		})

		It("does not turn ready again on a database error after a key went missing", func(ctx SpecContext) {
			id, err := admin.Add(ctx)
			Expect(err).NotTo(HaveOccurred())
			dir := GinkgoT().TempDir()
			entries, _, err := sec.Read(ctx)
			Expect(err).NotTo(HaveOccurred())
			enc, err := entries[id].Encode()
			Expect(err).NotTo(HaveOccurred())
			file := filepath.Join(dir, id)
			Expect(os.WriteFile(file, enc, 0o600)).To(Succeed())
			Expect(gatewaykeys.ReportLoaded(ctx, pool, id, "gw-a")).To(Succeed())
			Expect(gatewaykeys.ReportLoaded(ctx, pool, id, "gw-b")).To(Succeed())
			_, err = admin.Activate(ctx)
			Expect(err).NotTo(HaveOccurred())

			db := &flakyDB{DBTX: pool}
			l := gatewaykeys.NewLoader(dir, db, ws, "gw-a", nil)
			Expect(l.Load(ctx)).To(Succeed())
			Expect(os.Remove(file)).To(Succeed())
			Expect(l.Load(ctx)).NotTo(Succeed())
			ok, _ := l.Ready()
			Expect(ok).To(BeFalse(), "the active key is missing")

			db.fail.Store(true)
			Expect(l.Load(ctx)).NotTo(Succeed())
			ok, _ = l.Ready()
			Expect(ok).To(BeFalse(), "a database error is not a verdict on the keys")
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
