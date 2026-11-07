// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/declarative"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/sinkclass"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

const ns = "koinon"

var _ = Describe("Importer", func() {
	var (
		kube *fake.Clientset
		svc  *catalog.Service
		mcp  *mcpservers.Service
		imp  *declarative.Importer
		org  pgtype.UUID
		user pgtype.UUID
		slug string
		logs strings.Builder
		n    int
		ring = secretstoretest.Keyring()
	)
	BeforeEach(func(ctx SpecContext) {
		kube = fake.NewClientset()
		svc = catalog.New(pool, ring, nil)
		mcp = mcpservers.New(pool, ring, nil)
		logs.Reset()
		imp = &declarative.Importer{
			Client: kube, Namespace: ns, Pool: pool, Catalog: svc, MCP: mcp, Sealer: ring,
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

	It("imports an mcp-server Secret as a read-only server, disables it when the Secret goes and restores it", func(ctx SpecContext) {
		put(ctx, "tracker", declarative.TypeMCPServer, slug, map[string]string{
			"name": "tracker", "url": "https://mcp.example.com/mcp", "credentialScheme": "Bearer", "credential": "not-a-real-token",
		})
		Expect(imp.Sync(ctx)).To(Succeed())
		list, err := mcp.List(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(HaveLen(1))
		Expect(list[0]).To(HaveField("Source", "secret"))
		Expect(list[0]).To(HaveField("ManagedBySecret", "tracker"))
		Expect(list[0]).To(HaveField("HasCredential", true))
		Expect(list[0]).To(HaveField("CredentialHeader", "Authorization"))
		Expect(list[0]).To(HaveField("CredentialPrefix", "Bearer "))
		ev := auditOf(ctx, "mcp_server.declare")
		Expect(ev).To(HaveLen(1))
		Expect(ev[0].ActorKind).To(Equal("secret"))
		Expect(string(ev[0].Summary)).NotTo(ContainSubstring("not-a-real-token"))
		Expect(logs.String()).NotTo(ContainSubstring("not-a-real-token"))

		By("deleting the Secret disables the server and keeps the row")
		Expect(kube.CoreV1().Secrets(ns).Delete(ctx, "tracker", metav1.DeleteOptions{})).To(Succeed())
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ = mcp.List(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].Removed).To(BeTrue())
		Expect(list[0].DisabledReason).To(Equal(mcpservers.ReasonRemoved))

		By("restoring it brings the server back")
		put(ctx, "tracker", declarative.TypeMCPServer, slug, map[string]string{"name": "tracker", "url": "https://mcp.example.com/mcp"})
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ = mcp.List(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].Removed).To(BeFalse())
		Expect(list[0].DisabledReason).To(BeEmpty())
		Expect(list[0].HasCredential).To(BeFalse())
	})

	It("ignores an invalid mcp-server Secret with a Warning Event that quotes no value", func(ctx SpecContext) {
		put(ctx, "bad", declarative.TypeMCPServer, slug, map[string]string{"name": "Bad Name", "url": "https://mcp.example.com/mcp?key=not-a-real-token"})
		Expect(imp.Sync(ctx)).To(Succeed())
		list, _ := mcp.List(ctx, org)
		Expect(list).To(BeEmpty())
		var msg string
		for _, e := range events(ctx) {
			if e.Type == corev1.EventTypeWarning {
				msg = e.Message
			}
		}
		Expect(msg).To(ContainSubstring("Ignored"))
		Expect(msg).NotTo(ContainSubstring("not-a-real-token"))
	})

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

		It("writes a lost secret again from the Secret at the next sync, at the same version", func(ctx SpecContext) {
			put(ctx, "gh", declarative.TypeConnection, slug, conn())
			Expect(imp.Sync(ctx)).To(Succeed())
			// What keys forget leaves behind (ADR 0085 decision 13): no envelope, and the mark.
			tag, err := pool.Exec(ctx, `UPDATE connections SET secret_envelope = NULL, credential_lost_at = now() WHERE org_id = $1`, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(tag.RowsAffected()).To(BeEquivalentTo(1))
			rows, err := store.New(pool).ListConnections(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			_, err = connections.LoadGitHubApp(ctx, secretstoretest.Keyring(), rows[0])
			Expect(err).To(MatchError(connections.ErrCredentialLost))

			By("the Secret is unchanged, and the next sync stores its key again")
			Expect(imp.Sync(ctx)).To(Succeed())
			again, err := store.New(pool).ListConnections(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(HaveLen(1))
			Expect(again[0].ID).To(Equal(rows[0].ID))
			Expect(again[0].CredentialLostAt.Valid).To(BeFalse())
			Expect(again[0].SecretEnvelope).NotTo(BeEmpty())
			app, err := connections.LoadGitHubApp(ctx, ring, again[0])
			Expect(err).NotTo(HaveOccurred())
			want, err := github.ParsePrivateKey(gh.PEM)
			Expect(err).NotTo(HaveOccurred())
			Expect(app.Key.Fingerprint()).To(Equal(want.Fingerprint()), "the Secret's key, as before it was lost")

			By("once stored, the same version is not written again")
			Expect(imp.Sync(ctx)).To(Succeed())
			Expect(auditOf(ctx, "connection.declare")).To(HaveLen(2), "the first import and the re-write")
		})

		It("stops a removed connection from making a repository internal, at once (ADR 0091, old issue 350)", func(ctx SpecContext) {
			put(ctx, "gh", declarative.TypeConnection, slug, conn())
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, err := store.New(pool).ListConnections(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			cid := rows[0].ID.String()
			_, err = pool.Exec(ctx, `INSERT INTO mcp_servers (org_id, name, url, code_host_connection_id) VALUES ($1, 'tracker', 'https://mcp.example.com/mcp', $2)`, org, cid)
			Expect(err).NotTo(HaveOccurred())
			var linked string
			Expect(pool.QueryRow(ctx, `SELECT code_host_connection_id::text FROM mcp_servers WHERE org_id = $1 AND name = 'tracker'`, org).Scan(&linked)).To(Succeed())

			osvc := orgsettings.NewService(pool)
			now := time.Now()
			Expect(osvc.ReplaceCodeHostRepos(ctx, org, audit.User(user), cid, rows[0].SecretFingerprint, []orgsettings.CodeHostRepo{{Owner: "northwind", Name: "handbook", Visibility: "private"}}, now)).To(Succeed())
			code := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}
			classify := func() sinkclass.Class {
				l, err := osvc.LeakGuard(ctx, org)
				Expect(err).NotTo(HaveOccurred())
				return sinkclass.Classify(code, map[string]string{"owner": "northwind", "repo": "handbook"}, linked, l.Scope(), l, time.Now())
			}
			Expect(classify()).To(Equal(sinkclass.Internal), "a fresh private repository of the linked connection")

			Expect(kube.CoreV1().Secrets(ns).Delete(ctx, "gh", metav1.DeleteOptions{})).To(Succeed())
			Expect(imp.Sync(ctx)).To(Succeed())
			Expect(classify()).To(Equal(sinkclass.ExternalHigh), "the reading is fresh but the connection is removed")
		})

		It("drops the code-host scope when the declared secret rotates, and keeps it when nothing changed (#244)", func(ctx SpecContext) {
			put(ctx, "gh", declarative.TypeConnection, slug, conn())
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, err := store.New(pool).ListConnections(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			cid := rows[0].ID.String()
			osvc := orgsettings.NewService(pool)
			Expect(osvc.ReplaceCodeHostRepos(ctx, org, audit.User(user), cid, rows[0].SecretFingerprint,
				[]orgsettings.CodeHostRepo{{Owner: "northwind", Name: "handbook", Visibility: "private"}}, time.Now())).To(Succeed())
			stored := func() int {
				l, err := osvc.LeakGuard(ctx, org)
				Expect(err).NotTo(HaveOccurred())
				return len(l.CodeHostRepos)
			}
			Expect(stored()).To(Equal(1))

			By("the same Secret again keeps the reading")
			put(ctx, "gh", declarative.TypeConnection, slug, conn())
			Expect(imp.Sync(ctx)).To(Succeed())
			Expect(stored()).To(Equal(1))

			By("another private key is another credential: the old one's readings go")
			other := githubtest.New()
			DeferCleanup(other.Server.Close)
			rotated := conn()
			rotated["privateKey"] = string(other.PEM)
			put(ctx, "gh", declarative.TypeConnection, slug, rotated)
			Expect(imp.Sync(ctx)).To(Succeed())
			Expect(stored()).To(BeZero())
			var readings int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM org_code_host_readings WHERE org_id = $1`, org).Scan(&readings)).To(Succeed())
			Expect(readings).To(BeZero(), "the marker goes with the rows")

			By("a reading taken with the old key is refused")
			err = osvc.ReplaceCodeHostRepos(ctx, org, audit.User(user), cid, rows[0].SecretFingerprint,
				[]orgsettings.CodeHostRepo{{Owner: "northwind", Name: "handbook", Visibility: "private"}}, time.Now())
			Expect(err).To(MatchError(orgsettings.ErrCredentialChanged))
			Expect(stored()).To(BeZero())
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

		It("imports a GitHub Enterprise Server connection with its base URL and CA bundle", func(ctx SpecContext) {
			tlsServer := httptest.NewTLSServer(http.NotFoundHandler()) // a certificate to use as the CA
			DeferCleanup(tlsServer.Close)
			ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}))
			d := conn()
			d["baseUrl"], d["caBundle"] = " https://GHES.example.test:8443/ ", ca
			put(ctx, "gh", declarative.TypeConnection, slug, d)
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, _ := store.New(pool).ListConnections(ctx, org)
			Expect(rows).To(HaveLen(1))
			site, err := codehost.SiteOf(rows[0].Config)
			Expect(err).NotTo(HaveOccurred())
			Expect(site.BaseURL).To(Equal("https://ghes.example.test:8443"))
			Expect(site.CABundle).To(Equal(strings.TrimSpace(ca)))
			var cfg connections.GitHubConfig
			Expect(json.Unmarshal(rows[0].Config, &cfg)).To(Succeed())
			Expect(cfg.AppID).To(BeEquivalentTo(12))
			Expect(cfg.InstallationID).To(BeEquivalentTo(34))

			By("re-applying a changed Secret keeps the host")
			d["name"] = "acme GHES"
			put(ctx, "gh", declarative.TypeConnection, slug, d)
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, _ = store.New(pool).ListConnections(ctx, org)
			site, _ = codehost.SiteOf(rows[0].Config)
			Expect(site.BaseURL).To(Equal("https://ghes.example.test:8443"))
		})

		It("imports a connection's CI directories, one per line or comma-separated", func(ctx SpecContext) {
			d := conn()
			d["ciDirs"] = "./ci/pipelines/\nbuild/ci, ci/pipelines"
			put(ctx, "gh", declarative.TypeConnection, slug, d)
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, _ := store.New(pool).ListConnections(ctx, org)
			Expect(rows).To(HaveLen(1))
			site, err := codehost.SiteOf(rows[0].Config)
			Expect(err).NotTo(HaveOccurred())
			Expect(site.CIDirs).To(Equal([]string{"ci/pipelines", "build/ci"}))
		})

		It("updates the stored CI directories when the Secret's ciDirs changes", func(ctx SpecContext) {
			d := conn()
			d["ciDirs"] = "ci/pipelines"
			put(ctx, "gh", declarative.TypeConnection, slug, d)
			Expect(imp.Sync(ctx)).To(Succeed())
			d["ciDirs"] = "build/ci\ndeploy"
			put(ctx, "gh", declarative.TypeConnection, slug, d)
			Expect(imp.Sync(ctx)).To(Succeed())
			rows, _ := store.New(pool).ListConnections(ctx, org)
			Expect(rows).To(HaveLen(1))
			site, err := codehost.SiteOf(rows[0].Config)
			Expect(err).NotTo(HaveOccurred())
			Expect(site.CIDirs).To(Equal([]string{"build/ci", "deploy"}))
		})

		It("ignores a connection Secret with an unsafe CI directory", func(ctx SpecContext) {
			for _, v := range []string{"../up", "/abs", "."} {
				d := conn()
				d["ciDirs"] = v
				put(ctx, "gh", declarative.TypeConnection, slug, d)
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ := store.New(pool).ListConnections(ctx, org)
				Expect(rows).To(BeEmpty(), v)
			}
		})

		It("ignores a connection Secret with a bad base URL or CA bundle", func(ctx SpecContext) {
			for k, v := range map[string]string{"baseUrl": "http://ghes.example.test", "caBundle": "not pem"} {
				d := conn()
				d[k] = v
				put(ctx, "gh", declarative.TypeConnection, slug, d)
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ := store.New(pool).ListConnections(ctx, org)
				Expect(rows).To(BeEmpty(), k)
			}
		})

		It("ignores a connection Secret whose base URL is a private or internal host", func(ctx SpecContext) {
			for _, v := range []string{"https://10.0.0.1", "https://127.0.0.1:8443", "https://koinon-postgres"} {
				d := conn()
				d["baseUrl"] = v
				put(ctx, "gh", declarative.TypeConnection, slug, d)
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ := store.New(pool).ListConnections(ctx, org)
				Expect(rows).To(BeEmpty(), v)
			}
		})

		Describe("a Gitea bot", func() {
			giteaConn := func() map[string]string {
				return map[string]string{
					"name": "acme Gitea", "connector": "gitea", "baseUrl": " https://Git.example.test/gitea/ ",
					"readToken": "koinon_test_not-a-real-read-token", "writeToken": "koinon_test_not-a-real-write-token",
					"ciDirs": "ci/pipelines",
				}
			}

			It("is imported with its sub-path, both tokens sealed and only fingerprints in the config", func(ctx SpecContext) {
				ring := secretstoretest.Keyring()
				imp.Sealer = ring
				put(ctx, "gt", declarative.TypeConnection, slug, giteaConn())
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ := store.New(pool).ListConnections(ctx, org)
				Expect(rows).To(HaveLen(1))
				c := rows[0]
				Expect(c.Connector).To(Equal("gitea"))
				Expect(c.Source).To(Equal(store.ConfigSourceSecret))
				Expect(string(c.Config)).NotTo(ContainSubstring("not-a-real"))
				Expect(string(c.SecretEnvelope)).NotTo(ContainSubstring("not-a-real"))
				cfg, err := gitea.ParseConfig(c.Config)
				Expect(err).NotTo(HaveOccurred())
				Expect(cfg.BaseURL).To(Equal("https://git.example.test/gitea"))
				Expect(cfg.CIDirs).To(Equal([]string{"ci/pipelines"}))
				Expect(cfg.ReadTokenSetAt).To(BeTemporally("~", time.Now(), time.Minute))

				loaded, err := connections.Load(ctx, ring, c)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(loaded.Fields["readToken"])).To(Equal("koinon_test_not-a-real-read-token"))
				Expect(string(loaded.Fields["writeToken"])).To(Equal("koinon_test_not-a-real-write-token"))
				Expect(providers.Default().MustGet("gitea").Validate(loaded)).To(Succeed())

				ev := auditOf(ctx, "connection.declare")
				Expect(ev).To(HaveLen(1))
				Expect(string(ev[0].Summary)).To(ContainSubstring("fingerprints"))
				Expect(string(ev[0].Summary)).NotTo(ContainSubstring("not-a-real"))
			})

			It("keeps a token's age when the same Secret is applied again with another name, and restarts it for a new token", func(ctx SpecContext) {
				put(ctx, "gt", declarative.TypeConnection, slug, giteaConn())
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ := store.New(pool).ListConnections(ctx, org)
				first, _ := gitea.ParseConfig(rows[0].Config)
				// Backdate the stored dates, as if the tokens were entered long ago.
				old := first
				old.ReadTokenSetAt, old.WriteTokenSetAt = time.Now().Add(-100*24*time.Hour), time.Now().Add(-100*24*time.Hour)
				raw, _ := json.Marshal(old)
				_, err := pool.Exec(ctx, `UPDATE connections SET config = $1 WHERE id = $2`, raw, rows[0].ID)
				Expect(err).NotTo(HaveOccurred())

				d := giteaConn()
				d["name"] = "renamed"
				d["writeToken"] = "koinon_test_not-a-real-write-token-2"
				put(ctx, "gt", declarative.TypeConnection, slug, d)
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ = store.New(pool).ListConnections(ctx, org)
				now, _ := gitea.ParseConfig(rows[0].Config)
				Expect(now.ReadTokenSetAt).To(BeTemporally("~", old.ReadTokenSetAt, time.Second), "same read token keeps its age")
				Expect(now.WriteTokenSetAt).To(BeTemporally("~", time.Now(), time.Minute), "a new write token is new")
			})

			It("unlinks, audited, the MCP servers a new base URL no longer serves, and keeps the others (#117)", func(ctx SpecContext) {
				put(ctx, "gt", declarative.TypeConnection, slug, giteaConn())
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ := store.New(pool).ListConnections(ctx, org)
				Expect(rows).To(HaveLen(1))
				link := func(name, u string) {
					_, err := pool.Exec(ctx, `INSERT INTO mcp_servers (org_id, name, url, code_host_connection_id) VALUES ($1, $2, $3, $4)`, org, name, u, rows[0].ID)
					Expect(err).NotTo(HaveOccurred())
				}
				link("old-host", "https://git.example.test/mcp")
				linked := func(name string) bool {
					var ok bool
					Expect(pool.QueryRow(ctx, `SELECT code_host_connection_id IS NOT NULL FROM mcp_servers WHERE org_id = $1 AND name = $2`, org, name).Scan(&ok)).To(Succeed())
					return ok
				}

				By("the same host keeps both links")
				d := giteaConn()
				d["name"] = "renamed"
				put(ctx, "gt", declarative.TypeConnection, slug, d)
				Expect(imp.Sync(ctx)).To(Succeed())
				Expect(linked("old-host")).To(BeTrue())
				Expect(auditOf(ctx, "mcp_server.data_class.reset")).To(BeEmpty())

				By("a new host unlinks the server at the old one, and keeps one at the new host")
				// Written straight to the table, past the check, as if linked for the move.
				link("new-host", "https://git2.example.test/mcp")
				d["baseUrl"] = "https://git2.example.test"
				put(ctx, "gt", declarative.TypeConnection, slug, d)
				Expect(imp.Sync(ctx)).To(Succeed())
				Expect(linked("old-host")).To(BeFalse())
				Expect(linked("new-host")).To(BeTrue())
				ev := auditOf(ctx, "mcp_server.data_class.reset")
				Expect(ev).To(HaveLen(1))
				Expect(string(ev[0].Summary)).To(ContainSubstring("connection_host_changed"))
				Expect(ev[0].ActorID).To(Equal("gt"), "the Secret that moved the connection")
			})

			It("refuses to change a declared connection's connector, leaving the row as it was", func(ctx SpecContext) {
				put(ctx, "gt", declarative.TypeConnection, slug, giteaConn())
				Expect(imp.Sync(ctx)).To(Succeed())
				put(ctx, "gt", declarative.TypeConnection, slug, conn())
				Expect(imp.Sync(ctx)).To(Succeed())
				rows, _ := store.New(pool).ListConnections(ctx, org)
				Expect(rows).To(HaveLen(1))
				Expect(rows[0].Connector).To(Equal("gitea"))
				cfg, err := gitea.ParseConfig(rows[0].Config)
				Expect(err).NotTo(HaveOccurred())
				Expect(cfg.BaseURL).To(Equal("https://git.example.test/gitea"))
				var found bool
				for _, e := range events(ctx) {
					found = found || strings.Contains(e.Message, "cannot change from gitea to github")
				}
				Expect(found).To(BeTrue())
			})

			DescribeTable("is ignored, without quoting a token, when",
				func(ctx SpecContext, mutate func(map[string]string)) {
					d := giteaConn()
					mutate(d)
					put(ctx, "gt", declarative.TypeConnection, slug, d)
					Expect(imp.Sync(ctx)).To(Succeed())
					rows, _ := store.New(pool).ListConnections(ctx, org)
					Expect(rows).To(BeEmpty())
					for _, e := range events(ctx) {
						Expect(e.Message).NotTo(ContainSubstring("not-a-real"))
					}
					Expect(logs.String()).NotTo(ContainSubstring("not-a-real"))
				},
				Entry("there is no base URL", func(d map[string]string) { delete(d, "baseUrl") }),
				Entry("the base URL is http", func(d map[string]string) { d["baseUrl"] = "http://git.example.test" }),
				Entry("the base URL has a glob in its path", func(d map[string]string) { d["baseUrl"] = "https://git.example.test/*" }),
				Entry("the base URL is an internal host", func(d map[string]string) { d["baseUrl"] = "https://10.0.0.1/gitea" }),
				Entry("a token is missing", func(d map[string]string) { delete(d, "writeToken") }),
				Entry("the tokens are the same", func(d map[string]string) { d["writeToken"] = d["readToken"] }),
				Entry("a token has a space", func(d map[string]string) { d["readToken"] = "koinon_test not-a-real" }),
			)
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
