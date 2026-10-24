// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/declarative"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

const ns = "koinon"

var _ = Describe("Importer", func() {
	var (
		kube *fake.Clientset
		svc  *catalog.Service
		imp  *declarative.Importer
		org  pgtype.UUID
		user pgtype.UUID
		slug string
		logs strings.Builder
		n    int
	)
	BeforeEach(func(ctx SpecContext) {
		ring := secretstoretest.Keyring()
		kube = fake.NewClientset()
		svc = catalog.New(pool, ring, nil)
		logs.Reset()
		imp = &declarative.Importer{
			Client: kube, Namespace: ns, Pool: pool, Catalog: svc, Sealer: ring,
			Log: slog.New(slog.NewTextHandler(&logs, nil)),
		}
		org, user, slug = pgtest.NewOrg(ctx, pool)
		_ = user
	})

	// put creates or replaces a labelled Secret; the fake clientset has no resourceVersion, so
	// the test sets it.
	put := func(ctx context.Context, name, typ, orgSlug string, data map[string]string) {
		GinkgoHelper()
		n++
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, UID: types.UID("uid-" + name), ResourceVersion: string(rune('a'+n%26)) + name,
				Labels: map[string]string{declarative.LabelSecretType: typ, declarative.LabelOrg: orgSlug},
			},
			Data: map[string][]byte{},
		}
		for k, v := range data {
			sec.Data[k] = []byte(v)
		}
		_, err := kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
		if err != nil {
			_, err = kube.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{})
		}
		Expect(err).NotTo(HaveOccurred())
	}
	provider := func(name string) map[string]string {
		return map[string]string{
			"name": name, "provider": "openrouter", "url": "https://openrouter.ai/api/v1", "apiKey": "sk-declared-key",
			"models": "- name: deepseek/flash\n  guard: true\n  rates: {input: 0.30, output: 1.20}\n- name: free\n  rates: {input: \"0\", output: \"0\"}\n",
		}
	}
	events := func(ctx context.Context) []corev1.Event {
		list, err := kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		return list.Items
	}
	auditOf := func(ctx context.Context, action string) []store.AuditEvent {
		rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		var out []store.AuditEvent
		for _, r := range rows {
			if r.Action == action {
				out = append(out, r)
			}
		}
		return out
	}

	It("imports a model-provider Secret as a read-only provider with models, audited as the Secret", func(ctx SpecContext) {
		put(ctx, "openrouter", declarative.TypeModelProvider, slug, provider("openrouter"))
		Expect(imp.Sync(ctx)).To(Succeed())

		list, err := svc.ListProviders(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(HaveLen(1))
		Expect(list[0]).To(HaveField("Source", "secret"))
		Expect(list[0]).To(HaveField("ManagedBySecret", "openrouter"))
		Expect(list[0]).To(HaveField("HasKey", true))
		models, _ := svc.ListModels(ctx, org)
		Expect(models).To(HaveLen(2))
		for _, m := range models {
			switch m.Name {
			case "deepseek/flash":
				Expect(m.Guard).To(BeTrue())
			}
		}
		By("rates in the Secret are ignored with a Warning Event")
		var warned bool
		for _, e := range events(ctx) {
			if e.Type == corev1.EventTypeWarning && strings.Contains(e.Message, "rates are ignored") {
				warned = true
			}
		}
		Expect(warned).To(BeTrue())
		ev := auditOf(ctx, "provider.declare")
		Expect(ev).To(HaveLen(1))
		Expect(ev[0].ActorKind).To(Equal("secret"))
		Expect(ev[0].ActorID).To(Equal("openrouter"))
		Expect(logs.String()).NotTo(ContainSubstring("sk-declared-key"))

		By("a second pass over the same Secret changes and audits nothing")
		Expect(imp.Sync(ctx)).To(Succeed())
		Expect(auditOf(ctx, "provider.declare")).To(HaveLen(1))
	})

	It("disables the entries when the Secret is deleted, audited as the Secret, and restores them with it", func(ctx SpecContext) {
		put(ctx, "openrouter", declarative.TypeModelProvider, slug, provider("openrouter"))
		Expect(imp.Sync(ctx)).To(Succeed())
		Expect(kube.CoreV1().Secrets(ns).Delete(ctx, "openrouter", metav1.DeleteOptions{})).To(Succeed())
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ := svc.ListProviders(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].Removed).To(BeTrue())
		models, _ := svc.ListModels(ctx, org)
		Expect(models).To(HaveLen(2))
		for _, m := range models {
			Expect(m.DisabledReason).To(Equal(catalog.ReasonRemoved))
		}
		Expect(imp.Sync(ctx)).To(Succeed())
		Expect(auditOf(ctx, "provider.undeclare")).To(HaveLen(1), "not repeated")

		put(ctx, "openrouter", declarative.TypeModelProvider, slug, provider("openrouter"))
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ = svc.ListProviders(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].Removed).To(BeFalse())
		models, _ = svc.ListModels(ctx, org)
		for _, m := range models {
			Expect(m.DisabledReason).To(BeEmpty())
		}
		Expect(kube.CoreV1().Secrets(ns).Delete(ctx, "openrouter", metav1.DeleteOptions{})).To(Succeed())
		Expect(imp.Sync(ctx)).To(Succeed())
		ev := auditOf(ctx, "provider.undeclare")
		Expect(ev).To(HaveLen(2))
		Expect(ev[0].ActorID).To(Equal("openrouter"))
	})

	It("disables the entries when the label no longer selects the Secret", func(ctx SpecContext) {
		put(ctx, "openrouter", declarative.TypeModelProvider, slug, provider("openrouter"))
		Expect(imp.Sync(ctx)).To(Succeed())
		sec, _ := kube.CoreV1().Secrets(ns).Get(ctx, "openrouter", metav1.GetOptions{})
		delete(sec.Labels, declarative.LabelSecretType)
		_, err := kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ := svc.ListProviders(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].Removed).To(BeTrue())
	})

	It("ignores an invalid Secret with a Warning Event and a log line, and keeps what it declared before", func(ctx SpecContext) {
		put(ctx, "openrouter", declarative.TypeModelProvider, slug, provider("openrouter"))
		Expect(imp.Sync(ctx)).To(Succeed())

		bad := provider("openrouter")
		bad["url"] = "ftp://nope"
		bad["apiKey"] = "sk-bad-secret"
		put(ctx, "openrouter", declarative.TypeModelProvider, slug, bad)
		Expect(imp.Sync(ctx)).To(Succeed())

		list, _ := svc.ListProviders(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].URL).To(Equal("https://openrouter.ai/api/v1"))
		var warn *corev1.Event
		for _, e := range events(ctx) {
			if e.Reason == declarative.ReasonInvalid {
				warn = &e
			}
		}
		Expect(warn).NotTo(BeNil())
		Expect(warn.Type).To(Equal(corev1.EventTypeWarning))
		Expect(warn.InvolvedObject.Name).To(Equal("openrouter"))
		Expect(warn.Message).To(ContainSubstring("url"))
		Expect(warn.Message).NotTo(ContainSubstring("sk-bad-secret"))
		Expect(logs.String()).To(ContainSubstring("ignoring an invalid declarative Secret"))
		Expect(logs.String()).NotTo(ContainSubstring("sk-bad-secret"))

		By("the same invalid version is reported once")
		count := len(events(ctx))
		Expect(imp.Sync(ctx)).To(Succeed())
		Expect(events(ctx)).To(HaveLen(count))
	})

	DescribeTable("ignores a Secret that does not parse",
		func(ctx SpecContext, mutate func(map[string]string), wantMsg string) {
			d := provider("p")
			mutate(d)
			put(ctx, "p", declarative.TypeModelProvider, slug, d)
			Expect(imp.Sync(ctx)).To(Succeed())
			list, _ := svc.ListProviders(ctx, org)
			Expect(list).To(BeEmpty())
			var msg string
			for _, e := range events(ctx) {
				if e.Reason == declarative.ReasonInvalid {
					msg = e.Message
				}
			}
			Expect(msg).To(ContainSubstring(wantMsg))
		},
		Entry("a missing name", func(d map[string]string) { delete(d, "name") }, "name is required"),
		Entry("a missing url", func(d map[string]string) { delete(d, "url") }, "url is required"),
		Entry("a missing provider", func(d map[string]string) { delete(d, "provider") }, "provider is required"),
		Entry("models that are not a list", func(d map[string]string) { d["models"] = "name: x" }, "models"),
		Entry("a model with an unknown field", func(d map[string]string) { d["models"] = "- {name: x, price: 3}" }, "models"),
		Entry("the guard on a provider that has none", func(d map[string]string) {
			d["provider"] = "ollama"
		}, "routing guard"),
	)

	It("reports a Secret with a key when Koinon has no key-encryption keys, and still imports the others", func(ctx SpecContext) {
		imp.Catalog = catalog.New(pool, nil, nil)
		imp.Sealer = nil
		put(ctx, "with-key", declarative.TypeModelProvider, slug, provider("with-key"))
		keyless := provider("keyless")
		delete(keyless, "apiKey")
		keyless["provider"] = "ollama"
		keyless["models"] = "- {name: q, rates: {input: \"0\", output: \"0\"}}"
		put(ctx, "keyless", declarative.TypeModelProvider, slug, keyless)
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ := svc.ListProviders(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].Name).To(Equal("keyless"))
		var msg string
		for _, e := range events(ctx) {
			if e.Reason == declarative.ReasonInvalid {
				msg += e.Message
			}
		}
		Expect(msg).To(ContainSubstring("key-encryption keys"))
	})

	It("ignores a Secret for an unknown org", func(ctx SpecContext) {
		put(ctx, "p", declarative.TypeModelProvider, "no-such-org", provider("p"))
		Expect(imp.Sync(ctx)).To(Succeed())
		var msg string
		for _, e := range events(ctx) {
			msg += e.Message
		}
		Expect(msg).To(ContainSubstring("no-such-org"))
	})

	It("lets the oldest Secret win when two declare the same provider", func(ctx SpecContext) {
		put(ctx, "first", declarative.TypeModelProvider, slug, provider("same"))
		sec, _ := kube.CoreV1().Secrets(ns).Get(ctx, "first", metav1.GetOptions{})
		sec.CreationTimestamp = metav1.Now()
		_, err := kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		put(ctx, "second", declarative.TypeModelProvider, slug, provider("same"))
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ := svc.ListProviders(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].ManagedBySecret).To(Equal("second")) // zero creation time sorts first
	})

	It("disables a UI provider of the same name, and enables it again when the Secret goes", func(ctx SpecContext) {
		ui, err := svc.CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{Name: "openrouter", Type: "openrouter", URL: "https://x.example.com/v1"})
		Expect(err).NotTo(HaveOccurred())
		put(ctx, "openrouter", declarative.TypeModelProvider, slug, provider("openrouter"))
		Expect(imp.Sync(ctx)).To(Succeed())
		got, _ := svc.GetProvider(ctx, org, ui.ID)
		Expect(got.DisabledReason).To(Equal(catalog.ReasonShadowedProvider))

		Expect(kube.CoreV1().Secrets(ns).Delete(ctx, "openrouter", metav1.DeleteOptions{})).To(Succeed())
		Expect(imp.Sync(ctx)).To(Succeed())
		got, _ = svc.GetProvider(ctx, org, ui.ID)
		Expect(got.DisabledReason).To(BeEmpty(), "a removed declared provider no longer shadows it")
	})

	Describe("connection Secrets", func() {
		var gh *githubtest.Fake
		BeforeEach(func() {
			gh = githubtest.New()
			DeferCleanup(gh.Server.Close)
		})
		conn := func() map[string]string {
			return map[string]string{
				"name": "acme GitHub", "connector": "github", "appId": "12", "installationId": "34", "privateKey": string(gh.PEM),
			}
		}

		It("imports a GitHub App connection into the store, read-only and audited as the Secret", func(ctx SpecContext) {
			put(ctx, "gh", declarative.TypeConnection, slug, conn())
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, err := store.New(pool).ListConnections(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			c := rows[0]
			Expect(c.Source).To(Equal(store.ConfigSourceSecret))
			Expect(c.ManagedBySecret.String).To(Equal("gh"))
			Expect(c.SecretName.Valid).To(BeFalse())
			Expect(string(c.SecretEnvelope)).NotTo(ContainSubstring("PRIVATE KEY"))
			ev := auditOf(ctx, "connection.declare")
			Expect(ev).To(HaveLen(1))
			Expect(ev[0].ActorID).To(Equal("gh"))
			Expect(string(ev[0].Summary)).To(ContainSubstring("keyFingerprint"))
			Expect(string(ev[0].Summary)).NotTo(ContainSubstring("PRIVATE KEY"))

			Expect(imp.Sync(ctx)).To(Succeed())
			Expect(auditOf(ctx, "connection.declare")).To(HaveLen(1))

			Expect(kube.CoreV1().Secrets(ns).Delete(ctx, "gh", metav1.DeleteOptions{})).To(Succeed())
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, _ = store.New(pool).ListConnections(ctx, org)
			Expect(rows).To(HaveLen(1), "the connection stays, for the audit log and the agents bound to it")
			Expect(rows[0].RemovedAt.Valid).To(BeTrue())
			Expect(auditOf(ctx, "connection.undeclare")).To(HaveLen(1))
			Expect(imp.Sync(ctx)).To(Succeed())
			Expect(auditOf(ctx, "connection.undeclare")).To(HaveLen(1), "not repeated")

			By("using it fails with a reason that names the Secret")
			_, err = connections.LoadGitHubApp(ctx, secretstoretest.Keyring(), rows[0])
			Expect(err).To(MatchError(connections.ErrRemoved))
			Expect(err.Error()).To(ContainSubstring(`Secret gh`))

			By("restoring the Secret restores the same connection")
			put(ctx, "gh", declarative.TypeConnection, slug, conn())
			Expect(imp.Sync(ctx)).To(Succeed())
			again, _ := store.New(pool).ListConnections(ctx, org)
			Expect(again).To(HaveLen(1))
			Expect(again[0].ID).To(Equal(rows[0].ID))
			Expect(again[0].RemovedAt.Valid).To(BeFalse())
		})

		It("can be read back with the keys", func(ctx SpecContext) {
			ring := secretstoretest.Keyring()
			imp.Sealer = ring
			put(ctx, "gh", declarative.TypeConnection, slug, conn())
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, _ := store.New(pool).ListConnections(ctx, org)
			app, err := connections.LoadGitHubApp(ctx, ring, rows[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(app.AppID).To(Equal(int64(12)))
			Expect(app.InstallationID).To(Equal(int64(34)))
		})

		It("ignores a connection Secret with a bad key without quoting it", func(ctx SpecContext) {
			d := conn()
			d["privateKey"] = "-----BEGIN RSA PRIVATE KEY-----\nTOPSECRETSTUFF\n-----END RSA PRIVATE KEY-----\n"
			put(ctx, "gh", declarative.TypeConnection, slug, d)
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, _ := store.New(pool).ListConnections(ctx, org)
			Expect(rows).To(BeEmpty())
			for _, e := range events(ctx) {
				Expect(e.Message).NotTo(ContainSubstring("TOPSECRETSTUFF"))
			}
			Expect(logs.String()).NotTo(ContainSubstring("TOPSECRETSTUFF"))
			Expect(logs.String()).To(ContainSubstring("ignoring an invalid declarative Secret"))
		})
	})
})
