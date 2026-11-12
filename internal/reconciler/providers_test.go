// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
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
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
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
			Prices:         prices.NewResolver(pricing.Embedded()),
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
				_ = svc.DeleteProvider(ctx, org, audit.System("koinon-test:spec"), id)
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
	// servedRows reads gateway_served_models of org: model name to key file ("" for none).
	servedRows := func(ctx SpecContext, org pgtype.UUID) map[string]string {
		GinkgoHelper()
		rows, err := pool.Query(ctx, `SELECT m.name, coalesce(s.key_file, '') FROM gateway_served_models s
			JOIN models m ON m.org_id = s.org_id AND m.id = s.model_id WHERE s.org_id = $1`, org)
		Expect(err).NotTo(HaveOccurred())
		out := map[string]string{}
		for rows.Next() {
			var name, file string
			Expect(rows.Scan(&name, &file)).To(Succeed())
			out[name] = file
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return out
	}

	It("gives an org that serves a priced model its model-check key: restricted, budgeted, in a Secret koinon-api reads", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		p := addProvider(ctx, org, user, "openrouter", "sk-upstream-secret", "deepseek/flash")
		cleanup(ctx, org, p.ID)
		slug := orgSlug(ctx, org)
		check := gateway.AgentRef{Org: slug, Name: gateway.ModelCheckAgentName}

		Expect(r.ReconcileAll(ctx)).To(Succeed())

		st := mg.last()
		var key gateway.Key
		for _, k := range st.Keys {
			if k.Agent == check {
				key = k
			}
		}
		Expect(key.ID).NotTo(BeEmpty())
		Expect(st.Models[check]).To(Equal([]string{"deepseek/flash"}))
		Expect(st.AllowedModels(check, st.ServedModels(slug))).To(Equal([]string{"deepseek/flash"}))
		l := st.Limits[check]
		Expect(l.Unit).To(Equal(gateway.USD))
		Expect(l.Amount).To(Equal(money.MustParse("0.10")), "the system key's small daily budget")
		Expect(l.Window).To(Equal(24 * time.Hour))
		sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, mcpservers.ModelCheckSecretName(org), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(gateway.HashKey(string(sec.Data[mcpservers.ExecKeyDataKey]))).To(Equal(key.Hash))
		Expect(fmt.Sprintf("%+v", st)).NotTo(ContainSubstring(string(sec.Data[mcpservers.ExecKeyDataKey])))

		By("the same key is kept, and an org's spend today shrinks the budget")
		_, err = pool.Exec(ctx, `INSERT INTO spend_entries (org_id, principal, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, 'system', 'r-1', $2, 'deepseek/flash', 1, 1, 0.04, true, now())`, org, key.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		again := mg.last()
		Expect(again.Limits[check].Amount).To(Equal(money.MustParse("0.06")))
		for _, k := range again.Keys {
			if k.Agent == check {
				Expect(k.ID).To(Equal(key.ID))
			}
		}

		By("a spent budget leaves the amount at zero, which takes the key out in both modes")
		_, err = pool.Exec(ctx, `INSERT INTO spend_entries (org_id, principal, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, 'system', 'r-2', $2, 'deepseek/flash', 1, 1, 0.07, true, now())`, org, key.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.last().Limits[check].Amount).To(BeZero())
	})

	It("issues no model-check key to an org that serves no priced model", func(ctx SpecContext) {
		org, _ := newOrg(ctx)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		slug := orgSlug(ctx, org)
		for _, k := range mg.last().Keys {
			Expect(k.Agent).NotTo(Equal(gateway.AgentRef{Org: slug, Name: gateway.ModelCheckAgentName}))
		}
		_, err := kube.CoreV1().Secrets(namespace).Get(ctx, mcpservers.ModelCheckSecretName(org), metav1.GetOptions{})
		Expect(err).To(HaveOccurred())
	})

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

	It("leaves a model on an ollama-type provider at ollama.com unpriced, never at zero (issue 310)", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		actor := audit.User(user)
		p, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "cloud", Type: "ollama", URL: "https://ollama.com"})
		Expect(err).NotTo(HaveOccurred())
		cleanup(ctx, org, p.ID)
		_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "gpt-oss:120b"})
		Expect(err).NotTo(HaveOccurred())

		Expect(r.ReconcileAll(ctx)).To(Succeed())
		if got, ok := providerOf(mg.last(), orgSlug(ctx, org)); ok {
			for _, m := range got.Models {
				Expect(m.Price).To(BeNil(), "unpriced, never at a zero price")
			}
		}
		Expect(servedRows(ctx, org)).NotTo(HaveKey("gpt-oss:120b"))
	})

	It("skips a model whose stored price cannot be used and still serves every other org", func(ctx SpecContext) {
		orgA, userA := newOrg(ctx)
		orgB, userB := newOrg(ctx)
		a := addProvider(ctx, orgA, userA, "up", "", "bad-rate")
		b := addProvider(ctx, orgB, userB, "up", "", "fine")
		cleanup(ctx, orgA, a.ID)
		cleanup(ctx, orgB, b.ID)
		// An org price that no longer validates: seven fraction digits, from before the stricter rule.
		_, err := pool.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by) VALUES ($1, 'old', '{"provider":"openai","match":{"glob":"bad-rate"},"validFrom":"2020-01-01","prices":{"input":"1.1234567","output":"1"},"tokenizer":"bytes","ratio":"3.0","maxOutput":10}', now(), 'test')`, orgA)
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

	It("renders each model's format and its provider's workspace ID (ADR 0069)", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		actor := audit.User(user)
		const workspace = "wrkspc_01ExampleExample"
		direct, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{
			Name: "claude", Type: "anthropic", URL: "http://fakemodel.example.com:8080/v1", AnthropicWorkspaceID: workspace,
		})
		Expect(err).NotTo(HaveOccurred())
		cleanup(ctx, org, direct.ID)
		other := addProvider(ctx, org, user, "up", "", "plain-model")
		cleanup(ctx, org, other.ID)
		_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: direct.ID, Name: "claude-haiku-test"})
		Expect(err).NotTo(HaveOccurred())
		_, err = prices.New(pool, nil, pricing.Embedded()).SetOverride(ctx, org, actor, prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "price-claude-haiku-test", Provider: "anthropic", Match: pricing.Match{Glob: "claude-haiku-test"},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(r.ReconcileAll(ctx)).To(Succeed())

		st := mg.last()
		Expect(st.Validate()).To(Succeed())
		formats := map[string]modelformat.Format{}
		workspaces := map[string]string{}
		for _, p := range st.Providers {
			if p.Org != orgSlug(ctx, org) {
				continue
			}
			workspaces[p.Name] = p.AnthropicWorkspaceID
			for _, m := range p.Models {
				formats[m.Name] = m.Format
			}
		}
		Expect(formats).To(Equal(map[string]modelformat.Format{"claude-haiku-test": modelformat.Anthropic, "plain-model": modelformat.OpenAI}))
		Expect(workspaces).To(Equal(map[string]string{"claude": workspace, "up": ""}))
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

	Describe("the models the gateway serves (old issue 174)", func() {
		It("records them once applied and removes them when the provider is disabled or deleted", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			keyed := addProvider(ctx, org, user, "openrouter", "sk-served", "keyed-model")
			keyless := addProvider(ctx, org, user, "ollama", "", "keyless-model")
			cleanup(ctx, org, keyed.ID, keyless.ID)

			Expect(r.ReconcileAll(ctx)).To(Succeed())
			got := servedRows(ctx, org)
			Expect(got).To(HaveLen(2))
			Expect(got).To(HaveKeyWithValue("keyless-model", ""))
			Expect(got["keyed-model"]).To(HavePrefix(keyed.ID + "-"))
			Expect(got["keyed-model"]).NotTo(ContainSubstring("sk-"))

			_, err := pool.Exec(ctx, `UPDATE providers SET disabled_reason = 'test' WHERE org_id = $1 AND id = $2`, org, keyless.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(servedRows(ctx, org)).To(HaveKey("keyed-model"))
			Expect(servedRows(ctx, org)).NotTo(HaveKey("keyless-model"))

			Expect(svc.DeleteProvider(ctx, org, audit.System("koinon-test:spec"), keyed.ID)).To(Succeed())
			Expect(servedRows(ctx, org)).To(BeEmpty(), "deleting the provider deletes its rows")
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(servedRows(ctx, org)).To(BeEmpty())
		})

		It("shows saved, serving and failed with a reason on the provider (issue 17)", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			p := addProvider(ctx, org, user, "openrouter", "sk-served", "status-model")
			cleanup(ctx, org, p.ID)
			status := func() catalog.Serving {
				GinkgoHelper()
				got, err := svc.GetProvider(ctx, org, p.ID)
				Expect(err).NotTo(HaveOccurred())
				return got.Serving
			}
			Expect(status().State).To(Equal(catalog.ServingSaved), "nothing is applied yet")

			By("a gateway that cannot be reached fails it with that reason")
			mg.fail = errors.New("gateway unreachable")
			Expect(r.ReconcileAll(ctx)).To(HaveOccurred())
			Expect(status()).To(Equal(catalog.Serving{State: catalog.ServingFailed, Reason: catalog.ReasonUnreachable, Detail: status().Detail}))

			By("a refused config fails it with the gateway's reason")
			mg.fail = &gateway.ConfigRejectedError{Revision: "abc", Reason: "bad budget"}
			Expect(r.ReconcileAll(ctx)).To(HaveOccurred())
			Expect(status()).To(Equal(catalog.Serving{State: catalog.ServingFailed, Reason: catalog.ReasonGatewayRejects, Detail: "bad budget"}))

			By("an applied state serves it and clears the failure")
			mg.fail = nil
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(status()).To(Equal(catalog.Serving{State: catalog.ServingServing}))

			By("a later failure does not fail a provider that is fully served")
			mg.fail = errors.New("gateway unreachable")
			Expect(r.ReconcileAll(ctx)).To(HaveOccurred())
			Expect(status().State).To(Equal(catalog.ServingServing))
			mg.fail = nil
		})

		It("records a model without a price as not served", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			p := addProvider(ctx, org, user, "up", "", "priced")
			cleanup(ctx, org, p.ID)
			_, err := svc.CreateModel(ctx, org, audit.User(user), user, catalog.ModelInput{ProviderID: p.ID, Name: "unpriced-model"})
			Expect(err).NotTo(HaveOccurred())

			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(servedRows(ctx, org)).To(HaveKey("priced"))
			Expect(servedRows(ctx, org)).NotTo(HaveKey("unpriced-model"))
		})

		It("keeps the rows when the gateway did not apply the state", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			p := addProvider(ctx, org, user, "up", "", "m")
			cleanup(ctx, org, p.ID)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(servedRows(ctx, org)).To(HaveKey("m"))

			mg.mu.Lock()
			mg.fail = errors.New("the reload did not complete")
			mg.mu.Unlock()
			DeferCleanup(func() { mg.mu.Lock(); mg.fail = nil; mg.mu.Unlock() })
			_, err := pool.Exec(ctx, `UPDATE providers SET disabled_reason = 'test' WHERE org_id = $1 AND id = $2`, org, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.ReconcileAll(ctx)).NotTo(Succeed())
			Expect(servedRows(ctx, org)).To(HaveKey("m"), "what the gateway serves is unknown, so nothing changes")
		})

		It("keeps the rows of an org whose model objects the gateway rejected, and records nothing new", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			old := addProvider(ctx, org, user, "up", "", "old-model")
			cleanup(ctx, org, old.ID)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(servedRows(ctx, org)).To(HaveKey("old-model"))

			added := addProvider(ctx, org, user, "other", "", "new-model")
			cleanup(ctx, org, added.ID)
			mg.mu.Lock()
			mg.fail = &gateway.RejectedError{Rejected: []gateway.Rejection{{Org: orgSlug(ctx, org), Kind: "AgentgatewayModel", Name: "m-1", Reason: "refused"}}}
			mg.mu.Unlock()
			DeferCleanup(func() { mg.mu.Lock(); mg.fail = nil; mg.mu.Unlock() })
			Expect(r.ReconcileAll(ctx)).NotTo(Succeed())
			Expect(servedRows(ctx, org)).To(Equal(map[string]string{"old-model": ""}))
		})
	})

	Describe("a new key file still reaching the gateway", func() {
		var (
			r     *reconciler.Reconciler
			clock time.Time
			t0    time.Time
			// newR is a fresh reconciler, as after a restart or a change of leader: nothing in memory.
			newR func() *reconciler.Reconciler
		)
		BeforeEach(func() {
			t0 = time.Now()
			clock = t0
			osg := fakegateway.New()
			cc, err := osg.Start()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
			newR = func() *reconciler.Reconciler {
				GinkgoHelper()
				r, err := reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
					Prices:  prices.NewResolver(pricing.Embedded()),
					Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
					Keys:    ring, ProviderKeys: reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: secretName},
					ProviderKeyDir: "/secrets/providers", URLPolicy: testPolicy, ProviderKeySettle: time.Minute,
					Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return clock },
				})
				Expect(err).NotTo(HaveOccurred())
				return r
			}
			r = newR()
		})

		It("keeps serving with the recorded previous file when the key was rotated while the reconciler was down", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			p := addProvider(ctx, org, user, "openrouter", "sk-old", "m")
			cleanup(ctx, org, p.ID)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			clock = t0.Add(2 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			slug := orgSlug(ctx, org)
			old, ok := providerOf(mg.last(), slug)
			Expect(ok).To(BeTrue())
			Expect(servedRows(ctx, org)).To(HaveKeyWithValue("m", path.Base(old.APIKeyFile)))

			// The reconciler is down while the key changes; a new process comes up.
			next := "sk-new"
			_, err := svc.UpdateProvider(ctx, org, audit.User(user), p.ID, catalog.ProviderPatch{APIKey: &next})
			Expect(err).NotTo(HaveOccurred())
			restarted := newR()
			Expect(restarted.ReconcileAll(ctx)).To(Succeed())
			during, ok := providerOf(mg.last(), slug)
			Expect(ok).To(BeTrue(), "the provider is not dropped while its new key travels")
			Expect(during.APIKeyFile).To(Equal(old.APIKeyFile))
			Expect(servedRows(ctx, org)).To(HaveKeyWithValue("m", path.Base(old.APIKeyFile)), "runs do not wait")
			sec, _ := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(sec.Data).To(HaveLen(2), "the old file is kept")

			clock = t0.Add(4 * time.Minute)
			Expect(restarted.ReconcileAll(ctx)).To(Succeed())
			after, _ := providerOf(mg.last(), slug)
			Expect(after.APIKeyFile).NotTo(Equal(old.APIKeyFile))
			Expect(servedRows(ctx, org)).To(HaveKeyWithValue("m", path.Base(after.APIKeyFile)))
			sec, _ = kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(sec.Data).To(HaveLen(1))
		})

		It("drops the provider after a restart when the recorded file is gone", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			p := addProvider(ctx, org, user, "openrouter", "sk-old", "m")
			cleanup(ctx, org, p.ID)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			clock = t0.Add(2 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(servedRows(ctx, org)).To(HaveKey("m"))

			next := "sk-new"
			_, err := svc.UpdateProvider(ctx, org, audit.User(user), p.ID, catalog.ProviderPatch{APIKey: &next})
			Expect(err).NotTo(HaveOccurred())
			// Someone emptied the provider keys Secret meanwhile.
			sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			sec.Data = nil
			_, err = kube.CoreV1().Secrets(namespace).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(newR().ReconcileAll(ctx)).To(Succeed())
			_, ok := providerOf(mg.last(), orgSlug(ctx, org))
			Expect(ok).To(BeFalse(), "the gateway would refuse a config naming a missing file")
			Expect(servedRows(ctx, org)).To(BeEmpty())
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
			Expect(servedRows(ctx, orgA)).To(BeEmpty(), "a run waits until the provider is served")
			Expect(servedRows(ctx, orgB)).To(HaveKey("m"))
			sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sec.Data).To(HaveLen(1), "but the file is already written")

			clock = t0.Add(61 * time.Second)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			_, okA = providerOf(mg.last(), orgSlug(ctx, orgA))
			Expect(okA).To(BeTrue())
			Expect(servedRows(ctx, orgA)).To(HaveKeyWithValue("m", HavePrefix(keyed.ID+"-")))
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
			Expect(servedRows(ctx, org)).To(HaveKeyWithValue("m", path.Base(old.APIKeyFile)), "still served, with the previous key")
			sec, _ := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(sec.Data).To(HaveLen(2), "the old file is not pruned yet")

			clock = t0.Add(4 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			after, _ := providerOf(mg.last(), slug)
			Expect(after.APIKeyFile).NotTo(Equal(old.APIKeyFile))
			Expect(servedRows(ctx, org)).To(HaveKeyWithValue("m", path.Base(after.APIKeyFile)))
			sec, _ = kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(sec.Data).To(HaveLen(1))
		})
	})
})
