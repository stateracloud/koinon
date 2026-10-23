// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/reconciler"
)

// Provider delivery (plan item PV1): the reconciler opens each org's provider keys into one
// Secret for the gateway and renders the providers and models of every org.
var _ = Describe("Provider delivery", func() {
	const secretName = "koinon-gateway-providers"
	var (
		mg   *fakeModelGateway
		kube *fake.Clientset
		r    *reconciler.Reconciler
		svc  *catalog.Service
	)

	BeforeEach(func(ctx SpecContext) {
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		mg = &fakeModelGateway{}
		kube = fake.NewClientset()
		svc = catalog.New(pool, ring, nil)
		r, err = reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: secretName},
			ProviderKeyDir: "/secrets/providers",
			Logger:         slog.New(slog.DiscardHandler),
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// cleanup removes providers so the shared database stays free of keys other specs' configs
	// have no key store for.
	cleanup := func(ctx SpecContext, org pgtype.UUID, ids ...string) {
		DeferCleanup(func(ctx SpecContext) {
			for _, id := range ids {
				_ = svc.DeleteProvider(ctx, org, audit.System(), id)
			}
		})
	}
	orgSlug := func(ctx SpecContext, org pgtype.UUID) string {
		var s string
		Expect(pool.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, org).Scan(&s)).To(Succeed())
		return s
	}
	providerOf := func(st gateway.State, org string) (gateway.Provider, bool) {
		for _, p := range st.Providers {
			if p.Org == org {
				return p, true
			}
		}
		return gateway.Provider{}, false
	}

	It("writes the opened key to the gateway Secret and renders its file, never the key", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		p := addProvider(ctx, org, user, "openrouter", "sk-upstream-secret", "deepseek/flash")
		cleanup(ctx, org, p.ID)

		Expect(r.ReconcileAll(ctx)).To(Succeed())

		st := mg.last()
		got, ok := providerOf(st, orgSlug(ctx, org))
		Expect(ok).To(BeTrue())
		Expect(got.Models).To(HaveLen(1))
		Expect(got.Models[0].Price).To(Equal(&gateway.Price{Input: "1", Output: "2"}))
		Expect(got.APIKeyFile).To(HavePrefix("/secrets/providers/" + p.ID + "-"))
		Expect(fmt.Sprintf("%+v", st)).NotTo(ContainSubstring("sk-upstream-secret"))

		sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		name := got.APIKeyFile[len("/secrets/providers/"):]
		Expect(string(sec.Data[name])).To(Equal("sk-upstream-secret"))
	})

	It("uses a new file name for a new key and prunes the old file", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		p := addProvider(ctx, org, user, "openrouter", "sk-old", "m")
		cleanup(ctx, org, p.ID)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		slug := orgSlug(ctx, org)
		first, _ := providerOf(mg.last(), slug)

		next := "sk-new"
		_, err := svc.UpdateProvider(ctx, org, audit.User(user), p.ID, catalog.ProviderPatch{APIKey: &next})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())

		second, _ := providerOf(mg.last(), slug)
		Expect(second.APIKeyFile).NotTo(Equal(first.APIKeyFile))
		sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec.Data).NotTo(HaveKey(first.APIKeyFile[len("/secrets/providers/"):]))
		Expect(string(sec.Data[second.APIKeyFile[len("/secrets/providers/"):]])).To(Equal("sk-new"))
	})

	It("renders every org's providers and models apart, with the same model name", func(ctx SpecContext) {
		orgA, userA := newOrg(ctx)
		orgB, userB := newOrg(ctx)
		a := addProvider(ctx, orgA, userA, "up", "", "shared-name")
		b := addProvider(ctx, orgB, userB, "up", "", "shared-name")
		cleanup(ctx, orgA, a.ID)
		cleanup(ctx, orgB, b.ID)

		Expect(r.ReconcileAll(ctx)).To(Succeed())

		st := mg.last()
		pa, okA := providerOf(st, orgSlug(ctx, orgA))
		pb, okB := providerOf(st, orgSlug(ctx, orgB))
		Expect(okA && okB).To(BeTrue())
		Expect(pa.Models[0].Name).To(Equal("shared-name"))
		Expect(pb.Models[0].Name).To(Equal("shared-name"))
		Expect(pa.APIKeyFile).To(BeEmpty()) // keyless
		Expect(st.Validate()).To(Succeed())
	})

	It("does not serve a provider whose key cannot be opened, and serves the rest", func(ctx SpecContext) {
		orgA, userA := newOrg(ctx)
		orgB, userB := newOrg(ctx)
		bad := addProvider(ctx, orgA, userA, "up", "sk-a", "m")
		good := addProvider(ctx, orgB, userB, "up", "sk-b", "m")
		cleanup(ctx, orgA, bad.ID)
		cleanup(ctx, orgB, good.ID)
		// A key sealed for another row does not open here (the AAD binds the row).
		_, err := pool.Exec(ctx, `UPDATE providers SET api_key = (SELECT api_key FROM providers WHERE org_id = $2 AND id = $3) WHERE org_id = $1 AND id = $4`,
			orgA, orgB, good.ID, bad.ID)
		Expect(err).NotTo(HaveOccurred())

		err = r.ReconcileAll(ctx)
		Expect(err).To(MatchError(ContainSubstring("not served")))
		Expect(err.Error()).NotTo(ContainSubstring("sk-"))
		st := mg.last()
		_, okBad := providerOf(st, orgSlug(ctx, orgA))
		_, okGood := providerOf(st, orgSlug(ctx, orgB))
		Expect(okBad).To(BeFalse())
		Expect(okGood).To(BeTrue())
		Expect(slices.ContainsFunc(st.Providers, func(p gateway.Provider) bool { return p.Org == orgSlug(ctx, orgB) })).To(BeTrue())
	})

	It("holds back the config for a new key file until the file has had time to reach the gateway", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		p := addProvider(ctx, org, user, "openrouter", "sk-settle", "m")
		cleanup(ctx, org, p.ID)

		t0 := time.Now()
		clock := t0
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		mg := &fakeModelGateway{}
		r, err := reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:    ring, ProviderKeys: reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: secretName},
			ProviderKeyDir: "/secrets/providers", ProviderKeySettle: time.Minute,
			Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return clock },
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.applied).To(BeEmpty(), "the config naming the new file is not applied at once")
		sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec.Data).To(HaveLen(1), "but the file is already written")

		clock = t0.Add(30 * time.Second)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.applied).To(BeEmpty())

		clock = t0.Add(61 * time.Second)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.applied).NotTo(BeEmpty())
	})
})
