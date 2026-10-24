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
			ProviderKeyDir: "/secrets/providers", URLPolicy: testPolicy,
			Logger: slog.New(slog.DiscardHandler),
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

	It("skips a model whose stored rate cannot be priced and still serves every other org", func(ctx SpecContext) {
		orgA, userA := newOrg(ctx)
		orgB, userB := newOrg(ctx)
		a := addProvider(ctx, orgA, userA, "up", "", "bad-rate")
		b := addProvider(ctx, orgB, userB, "up", "", "fine")
		cleanup(ctx, orgA, a.ID)
		cleanup(ctx, orgB, b.ID)
		// A row from before the stricter validation: seven fraction digits.
		_, err := pool.Exec(ctx, `UPDATE models SET input_rate = '1.1234567' WHERE org_id = $1`, orgA)
		Expect(err).NotTo(HaveOccurred())

		err = r.ReconcileAll(ctx)
		Expect(err).To(HaveOccurred(), "the bad model is reported")
		Expect(err.Error()).To(ContainSubstring("bad-rate"))

		got, ok := providerOf(mg.last(), orgSlug(ctx, orgB))
		Expect(ok).To(BeTrue(), "the other org is served")
		Expect(got.Models).To(HaveLen(1))
		if pa, ok := providerOf(mg.last(), orgSlug(ctx, orgA)); ok {
			Expect(pa.Models).To(BeEmpty())
		}
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

	It("checks the URL policy again at render and serves no provider that now names a denied host", func(ctx SpecContext) {
		orgA, userA := newOrg(ctx)
		orgB, userB := newOrg(ctx)
		bad := addProvider(ctx, orgA, userA, "up", "", "m")
		good := addProvider(ctx, orgB, userB, "up", "", "m")
		cleanup(ctx, orgA, bad.ID)
		cleanup(ctx, orgB, good.ID)
		// Saved while the host was fine, or written around the service layer.
		_, err := pool.Exec(ctx, `UPDATE providers SET url = 'http://169.254.169.254/latest' WHERE id = $1`, bad.ID)
		Expect(err).NotTo(HaveOccurred())

		err = r.ReconcileAll(ctx)
		Expect(err).To(MatchError(ContainSubstring("not allowed")))
		_, okBad := providerOf(mg.last(), orgSlug(ctx, orgA))
		_, okGood := providerOf(mg.last(), orgSlug(ctx, orgB))
		Expect(okBad).To(BeFalse())
		Expect(okGood).To(BeTrue())
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

	Describe("a new key file still reaching the gateway", func() {
		var (
			r     *reconciler.Reconciler
			clock time.Time
			t0    time.Time
		)
		BeforeEach(func() {
			t0 = time.Now()
			clock = t0
			osg := fakegateway.New()
			cc, err := osg.Start()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
			r, err = reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
				Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
				Keys:    ring, ProviderKeys: reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: secretName},
				ProviderKeyDir: "/secrets/providers", URLPolicy: testPolicy, ProviderKeySettle: time.Minute,
				Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return clock },
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("leaves that provider out but still applies the others", func(ctx SpecContext) {
			orgA, userA := newOrg(ctx)
			orgB, userB := newOrg(ctx)
			keyed := addProvider(ctx, orgA, userA, "openrouter", "sk-settle", "m")
			keyless := addProvider(ctx, orgB, userB, "ollama", "", "m")
			cleanup(ctx, orgA, keyed.ID)
			cleanup(ctx, orgB, keyless.ID)

			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(mg.applied).NotTo(BeEmpty(), "one org's settling key must not block the gateway config of every org")
			_, okA := providerOf(mg.last(), orgSlug(ctx, orgA))
			_, okB := providerOf(mg.last(), orgSlug(ctx, orgB))
			Expect(okA).To(BeFalse(), "no previous file to fall back on")
			Expect(okB).To(BeTrue())
			sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sec.Data).To(HaveLen(1), "but the file is already written")

			clock = t0.Add(61 * time.Second)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			_, okA = providerOf(mg.last(), orgSlug(ctx, orgA))
			Expect(okA).To(BeTrue())
		})

		It("keeps serving with the previous file while a rotated key travels", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			p := addProvider(ctx, org, user, "openrouter", "sk-old", "m")
			cleanup(ctx, org, p.ID)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			clock = t0.Add(2 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			slug := orgSlug(ctx, org)
			old, ok := providerOf(mg.last(), slug)
			Expect(ok).To(BeTrue())

			next := "sk-new"
			_, err := svc.UpdateProvider(ctx, org, audit.User(user), p.ID, catalog.ProviderPatch{APIKey: &next})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			during, ok := providerOf(mg.last(), slug)
			Expect(ok).To(BeTrue())
			Expect(during.APIKeyFile).To(Equal(old.APIKeyFile))
			sec, _ := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(sec.Data).To(HaveLen(2), "the old file is not pruned yet")

			clock = t0.Add(4 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			after, _ := providerOf(mg.last(), slug)
			Expect(after.APIKeyFile).NotTo(Equal(old.APIKeyFile))
			sec, _ = kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(sec.Data).To(HaveLen(1))
		})
	})
})
