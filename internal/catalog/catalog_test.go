// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("URLPolicy", func() {
	resolver := func(m map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
		return func(_ context.Context, h string) ([]netip.Addr, error) {
			ips, ok := m[h]
			if !ok {
				return nil, errors.New("no such host")
			}
			var out []netip.Addr
			for _, ip := range ips {
				out = append(out, netip.MustParseAddr(ip))
			}
			return out, nil
		}
	}
	p := catalog.URLPolicy{
		Namespace:    "koinon",
		AllowedHosts: []string{"localhost:11434", "ollama.lab.example.com", "10.7.0.0/24", "fakemodel.fakes.svc:8080", "192.168.7.7", "[fd00::7]:11434"},
		Resolver: resolver(map[string][]string{
			"api.openai.com":          {"104.18.6.192"},
			"ollama.lab.example.com":  {"127.0.0.1"},
			"rebind.example.com":      {"203.0.113.9", "127.0.0.1"},
			"meta.example.com":        {"169.254.169.254"},
			"fakemodel.fakes.svc":     {"10.96.4.4"},
			"other.fakes.svc":         {"10.96.4.5"},
			"internal.example.com":    {"10.1.2.3"},
			"allowed-net.example.com": {"10.7.0.9"},
			"cgnat.example.com":       {"100.64.3.3"},
			"ula.example.com":         {"fd12::1"},
			"v6.example.com":          {"fe80::1"},
		}),
	}
	DescribeTable("refuses a pattern host, strict or not",
		func(ctx SpecContext, u string) {
			for _, check := range []func(context.Context, string) error{p.Check, p.CheckStrict} {
				err := check(ctx, u)
				var inv *catalog.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), u)
				Expect(err.Error()).To(ContainSubstring("DNS exfiltration"))
			}
		},
		Entry("wildcard label", "https://*.example.com/v1"),
		Entry("bare star", "https://*/v1"),
		Entry("inner glob", "https://api.*.example.com"),
	)
	It("refuses a pattern in the platform allow list", func() {
		Expect(catalog.ValidateAllowedHosts([]string{"ollama.lab.example.com:11434", "10.7.0.0/24"})).To(Succeed())
		Expect(catalog.ValidateAllowedHosts([]string{"[fd00::1]", "[fd00::1]:11434", "fd00::1", "fd00::/8"})).To(Succeed())
		for _, bad := range []string{"*.lab.example.com", "[ab].lab.example.com", "[fd00::1]*", "[fd00::1]:[1-9]", "[*]:80"} {
			Expect(catalog.ValidateAllowedHosts([]string{bad})).To(MatchError(ContainSubstring("is a pattern")), bad)
		}
	})
	DescribeTable("denies",
		func(ctx SpecContext, u string) {
			err := p.Check(ctx, u)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), u)
			Expect(err.Error()).To(ContainSubstring("egress.allowedInternalHosts"))
		},
		Entry("localhost", "http://localhost:8080/v1"),
		Entry("loopback", "http://127.0.0.1/v1"),
		Entry("loopback v6", "http://[::1]/v1"),
		Entry("metadata", "http://169.254.169.254/latest"),
		Entry("link-local v6", "http://[fe80::1]/v1"),
		Entry("unspecified", "http://0.0.0.0/v1"),
		Entry("v4-mapped loopback", "http://[::ffff:127.0.0.1]/v1"),
		Entry("a single-label Service name", "http://ollama:11434/v1"),
		Entry("the release's own Service", "http://koinon-api.koinon.svc:8080/v1"),
		Entry("the release's own Service, full name", "http://koinon-api.koinon.svc.cluster.local/v1"),
		Entry("namespace shorthand", "http://koinon-api.koinon/v1"),
		Entry("a name that resolves to loopback", "http://rebind.example.com/v1"),
		Entry("a name that resolves to metadata", "http://meta.example.com/v1"),
		Entry("a name that resolves to link-local v6", "http://v6.example.com/v1"),
		Entry("localhost on another port than the allowed one", "http://localhost:9999/v1"),
		// The forms agentgateway's URL parser reads as IPv4 addresses.
		Entry("octal 0177.0.0.1", "http://0177.0.0.1/"),
		Entry("short 127.1", "http://127.1/"),
		Entry("hex 0x7f.0.0.1", "http://0x7f.0.0.1/"),
		Entry("hex integer 0x7f000001", "http://0x7f000001/"),
		Entry("decimal integer 2130706433", "http://2130706433/"),
		Entry("octal integer 017700000001", "http://017700000001/"),
		Entry("trailing dot 127.0.0.1.", "http://127.0.0.1./"),
		Entry("metadata as an integer", "http://2852039166/"),
		Entry("mixed 0251.0376.0251.0376", "http://0251.0376.0251.0376/"),
		Entry("a numeric last label that is no address, 1.2.3.4.5", "http://1.2.3.4.5/"),
		Entry("a name ending in a number, foo.0x1", "http://foo.0x1/"),
		Entry("a part over 255, 1.2.3.999", "http://1.2.3.999/"),
		// Private, shared and unique-local ranges, including Services' ClusterIPs.
		Entry("a ClusterIP", "http://10.96.0.10:8080/"),
		Entry("RFC 1918 172.16", "http://172.16.0.1/"),
		Entry("RFC 1918 192.168", "http://192.168.1.5/"),
		Entry("CGNAT", "http://100.64.1.1/"),
		Entry("unique-local v6", "http://[fd00::1]/"),
		Entry("a name that resolves to a private address", "http://internal.example.com/v1"),
		Entry("a name that resolves to CGNAT", "http://cgnat.example.com/v1"),
		Entry("a name that resolves to a unique-local address", "http://ula.example.com/v1"),
		Entry("IPv4-compatible ::127.0.0.1", "http://[::127.0.0.1]/"),
		Entry("IPv4-compatible ::7f00:1", "http://[::7f00:1]/"),
		Entry("NAT64 of loopback", "http://[64:ff9b::7f00:1]/"),
		Entry("NAT64 of metadata", "http://[64:ff9b::a9fe:a9fe]/"),
		Entry("local-use NAT64", "http://[64:ff9b:1::7f00:1]/"),
		Entry("6to4 of loopback", "http://[2002:7f00:1::]/"),
		Entry("6to4 of 10.0.0.1", "http://[2002:a00:1::1]/"),
		Entry("another Service's ClusterIP, not allow-listed", "http://other.fakes.svc:8080/v1"),
	)
	DescribeTable("allows",
		func(ctx SpecContext, u string) { Expect(p.Check(ctx, u)).To(Succeed(), u) },
		Entry("a public API", "https://api.openai.com/v1"),
		Entry("a public IP", "http://104.18.6.192/v1"),
		Entry("an allow-listed Service with its port", "http://fakemodel.fakes.svc:8080/v1"),
		Entry("an allow-listed host even though it resolves to loopback", "http://ollama.lab.example.com:11434/v1"),
		Entry("an allow-listed host:port", "http://localhost:11434/v1"),
		Entry("an IP inside an allow-listed CIDR", "http://10.7.0.5:11434/v1"),
		Entry("a name that resolves into an allow-listed CIDR", "http://allowed-net.example.com/v1"),
		Entry("an allow-listed IP", "http://192.168.7.7:11434/v1"),
		Entry("a public IPv6 literal (brackets are not a pattern)", "http://[2606:4700::1111]/v1"),
		Entry("an allow-listed IPv6 literal with its port", "http://[fd00::7]:11434/v1"),
		Entry("a name that does not resolve (checked again at render)", "https://future.example.com/v1"),
	)
	DescribeTable("rejects non-ASCII hosts", func(ctx SpecContext, u string) {
		var inv *catalog.InvalidError
		Expect(errors.As(p.Check(ctx, u), &inv)).To(BeTrue(), u)
		Expect(errors.As(p.CheckStrict(ctx, u), &inv)).To(BeTrue(), u)
	},
		Entry("a non-ASCII name", "http://exämple.com/v1"),
		Entry("fullwidth digits", "http://１２７.0.0.1/"),
	)
	It("fails closed at render when a name cannot be resolved", func(ctx SpecContext) {
		Expect(p.CheckStrict(ctx, "https://future.example.com/v1")).NotTo(Succeed())
		Expect(p.CheckStrict(ctx, "https://api.openai.com/v1")).To(Succeed())
		Expect(p.CheckStrict(ctx, "http://ollama.lab.example.com:11434/v1")).To(Succeed(), "an allow-listed name needs no lookup")
	})
})

var _ = Describe("Service", func() {
	var (
		svc   *catalog.Service
		org   pgtype.UUID
		user  pgtype.UUID
		actor audit.Actor
	)
	BeforeEach(func(ctx SpecContext) {
		svc = catalog.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
	})

	events := func(ctx SpecContext, action string) []store.AuditEvent {
		GinkgoHelper()
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
	create := func(ctx SpecContext, name, key string) catalog.Provider {
		GinkgoHelper()
		p, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{
			Name: name, Type: "openrouter", URL: "https://openrouter.ai/api/v1", APIKey: key,
		})
		Expect(err).NotTo(HaveOccurred())
		return p
	}

	Describe("providers", func() {
		It("seals the key bound to its row and never returns it", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "sk-or-secret")
			Expect(p.HasKey).To(BeTrue())
			raw, _ := json.Marshal(p)
			Expect(string(raw)).NotTo(ContainSubstring("sk-or-secret"))

			var env []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, p.ID).Scan(&env)).To(Succeed())
			Expect(string(env)).NotTo(ContainSubstring("sk-or-secret"))
			var e secretstore.Envelope
			Expect(json.Unmarshal(env, &e)).To(Succeed())
			Expect(e.KEKID).To(Equal("test-1"))
			id := mustUUID(p.ID)
			got, err := catalog.OpenKey(ring, org, id, env)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal("sk-or-secret"))

			By("a copy in another org's row does not open")
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err = catalog.OpenKey(ring, other, id, env)
			Expect(err).To(MatchError(secretstore.ErrOpen))
			_ = otherUser
		})

		It("audits create, update and delete without the key", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "sk-1")
			newKey, newURL := "sk-2", "https://openrouter.ai/api/v1/"
			_, err := svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{APIKey: &newKey, URL: &newURL})
			Expect(err).NotTo(HaveOccurred())
			Expect(svc.DeleteProvider(ctx, org, actor, p.ID)).To(Succeed())

			for _, a := range []string{"provider.create", "provider.update", "provider.delete"} {
				Expect(events(ctx, a)).To(HaveLen(1), a)
			}
			for _, e := range events(ctx, "provider.update") {
				Expect(e.ActorKind).To(Equal("user"))
				Expect(string(e.Summary)).To(ContainSubstring(`"keyChanged": true`))
				Expect(string(e.Summary)).NotTo(ContainSubstring("sk-"))
			}
		})

		It("keeps a key envelope that a rewrap replaced while the update was in flight", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "sk-1")
			id := mustUUID(p.ID)
			var before []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, p.ID).Scan(&before)).To(Succeed())

			// The URL policy's lookup runs between the update's read and its write, like a
			// `keys rewrap` that commits in that window: it re-seals the key into a different envelope.
			var rewrapped []byte
			racing := catalog.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{
				Namespace: "koinon",
				Resolver: func(ctx context.Context, _ string) ([]netip.Addr, error) {
					plain, err := catalog.OpenKey(ring, org, id, before)
					if err != nil {
						return nil, err
					}
					env, err := ring.Seal(catalog.KeyBinding(org, id), []byte(plain))
					if err != nil {
						return nil, err
					}
					if rewrapped, err = json.Marshal(env); err != nil {
						return nil, err
					}
					_, err = pool.Exec(ctx, `UPDATE providers SET api_key = $2 WHERE id = $1`, p.ID, rewrapped)
					return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, err
				},
			})
			newURL := "https://openrouter.ai/api/v1/"
			_, err := racing.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{URL: &newURL})
			Expect(err).NotTo(HaveOccurred())
			Expect(rewrapped).NotTo(BeNil(), "the lookup ran")

			var after []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, p.ID).Scan(&after)).To(Succeed())
			Expect(rewrapped).NotTo(Equal(before))
			Expect(after).To(MatchJSON(rewrapped), "the update wrote back the envelope it read")
			got, err := catalog.OpenKey(ring, org, id, after)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal("sk-1"))
		})

		It("keeps names unique per org, not across orgs", func(ctx SpecContext) {
			create(ctx, "dup", "")
			_, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "dup", Type: "ollama", URL: "http://x.example.com/v1"})
			Expect(err).To(MatchError(catalog.ErrConflict))
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err = svc.CreateProvider(ctx, other, audit.User(otherUser), otherUser, catalog.ProviderInput{Name: "dup", Type: "ollama", URL: "http://x.example.com/v1"})
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable("validates", func(ctx SpecContext, in catalog.ProviderInput) {
			_, err := svc.CreateProvider(ctx, org, actor, user, in)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
		},
			Entry("a bad name", catalog.ProviderInput{Name: "Bad Name", Type: "openai", URL: "https://x.example.com/v1"}),
			Entry("a URL with credentials", catalog.ProviderInput{Name: "a", Type: "openai", URL: "https://user:pw@x/v1"}),
			Entry("a non-http URL", catalog.ProviderInput{Name: "a", Type: "openai", URL: "ftp://x.example.com/v1"}),
			Entry("a bad type", catalog.ProviderInput{Name: "a", Type: "Open AI", URL: "https://x.example.com/v1"}),
		)

		It("refuses an upstream the URL policy denies, on create, update and declare", func(ctx SpecContext) {
			_, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "ssrf", Type: "openai", URL: "http://169.254.169.254/latest"})
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
			p := create(ctx, "fine", "")
			bad := "http://localhost:6379/"
			_, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{URL: &bad})
			Expect(errors.As(err, &inv)).To(BeTrue())
			_, err = svc.ApplyDeclaredProvider(ctx, org, catalog.DeclaredProvider{SecretName: "s", Version: "v", Name: "d", Type: "openai", URL: "http://koinon-api.koinon.svc/v1"})
			Expect(errors.As(err, &inv)).To(BeTrue())
		})

		It("serves nothing of a provider whose key was lost, until the key is entered again", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "sk-lost")
			_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "deepseek/flash"})
			Expect(err).NotTo(HaveOccurred())
			q := store.New(pool)
			// What keys forget does (ADR 0085 decision 13).
			_, err = pool.Exec(ctx, `UPDATE providers SET api_key = NULL, credential_lost_at = now() WHERE id = $1`, p.ID)
			Expect(err).NotTo(HaveOccurred())

			got, err := svc.GetProvider(ctx, org, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CredentialLostAt).NotTo(BeNil())
			Expect(got.HasKey).To(BeFalse())
			served, err := q.ListServedCatalog(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(served).To(BeEmpty())
			providers, err := q.ListServedProviders(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(providers).To(BeEmpty(), "no key file is delivered for it")
			m, err := q.GetModelServing(ctx, store.GetModelServingParams{OrgID: org, Name: "deepseek/flash"})
			Expect(err).NotTo(HaveOccurred())
			Expect(m.CredentialLost).To(BeTrue(), "agentd fails the run with the reason")

			By("a change that sets no key keeps it lost")
			newURL := "https://openrouter.ai/api/v1/"
			got, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{URL: &newURL})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CredentialLostAt).NotTo(BeNil())

			By("entering the key again serves it again")
			key := "sk-again"
			got, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{APIKey: &key})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CredentialLostAt).To(BeNil())
			Expect(got.HasKey).To(BeTrue())
			served, _ = q.ListServedCatalog(ctx, org)
			Expect(served).To(HaveLen(1))
		})

		It("ends the lost state of a keyless provider when its key is removed on purpose", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "sk-lost")
			_, err := pool.Exec(ctx, `UPDATE providers SET api_key = NULL, credential_lost_at = now() WHERE id = $1`, p.ID)
			Expect(err).NotTo(HaveOccurred())
			empty := ""
			got, err := svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{APIKey: &empty})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CredentialLostAt).To(BeNil(), "the admin chose a keyless upstream")
		})

		It("cannot store a key without key-encryption keys", func(ctx SpecContext) {
			noKeys := catalog.New(pool, nil, nil)
			_, err := noKeys.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "k", Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk"})
			Expect(err).To(MatchError(catalog.ErrNoKeys))
			_, err = noKeys.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "keyless", Type: "ollama", URL: "http://x.example.com/v1"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("seals a direct provider's cost API key with its scope, and removes both together", func(ctx SpecContext) {
			p, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{
				Name: "anthropic", Type: "anthropic", URL: "https://api.anthropic.com", APIKey: "koinon_test_not-a-real-token",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.HasCostKey).To(BeFalse())
			invalid := func(err error) {
				GinkgoHelper()
				var inv *catalog.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
			}

			By("a key needs a workspace the cost report can name")
			admin, scope, bad := "koinon_test_not-a-real-admin-key", "wrkspc_01TestWorkspace", "proj_abc"
			_, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostAPIKey: &admin})
			invalid(err)
			_, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostAPIKey: &admin, CostScope: &bad})
			invalid(err)
			_, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostScope: &scope})
			invalid(err)

			got, err := svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostAPIKey: &admin, CostScope: &scope})
			Expect(err).NotTo(HaveOccurred())
			Expect([]any{got.HasCostKey, got.CostScope}).To(Equal([]any{true, scope}))
			raw, _ := json.Marshal(got)
			Expect(string(raw)).NotTo(ContainSubstring(admin))
			var env []byte
			Expect(pool.QueryRow(ctx, `SELECT cost_api_key FROM providers WHERE id = $1`, p.ID).Scan(&env)).To(Succeed())
			Expect(string(env)).NotTo(ContainSubstring(admin))
			id := mustUUID(p.ID)
			plain, err := catalog.OpenCostKey(ring, org, id, env)
			Expect(err).NotTo(HaveOccurred())
			Expect(plain).To(Equal(admin))
			_, err = catalog.OpenKey(ring, org, id, env)
			Expect(err).To(MatchError(secretstore.ErrOpen), "bound to its own column: it never opens as the inference key")
			for _, e := range events(ctx, "provider.update") {
				Expect(string(e.Summary)).NotTo(ContainSubstring(admin))
			}
			since := func() *time.Time {
				GinkgoHelper()
				var t *time.Time
				Expect(pool.QueryRow(ctx, `SELECT cost_reconcile_since FROM providers WHERE id = $1`, p.ID).Scan(&t)).To(Succeed())
				return t
			}
			first := since()
			Expect(first).NotTo(BeNil(), "the reconciliation reads the days after the key was stored")

			By("the default workspace, alone")
			def := "default"
			got, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostScope: &def})
			Expect(err).NotTo(HaveOccurred())
			Expect([]any{got.HasCostKey, got.CostScope}).To(Equal([]any{true, def}))

			By("a replaced key keeps the day the reconciliation starts from")
			rotated := "koinon_test_not-a-real-admin-key-2"
			_, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostAPIKey: &rotated})
			Expect(err).NotTo(HaveOccurred())
			Expect(since()).To(HaveValue(BeTemporally("==", *first)))

			By("its URL can no longer move off the provider's own API while it holds the key")
			elsewhere := "https://anthropic-proxy.example.com"
			_, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{URL: &elsewhere})
			invalid(err)

			By("removing the key removes the scope")
			empty := ""
			got, err = svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostAPIKey: &empty})
			Expect(err).NotTo(HaveOccurred())
			Expect([]any{got.HasCostKey, got.CostScope}).To(Equal([]any{false, ""}))
			Expect(since()).To(BeNil())
		})

		It("takes a cost API key for anthropic and openai providers at their own API only", func(ctx SpecContext) {
			p := create(ctx, "router", "koinon_test_not-a-real-token")
			admin, scope := "koinon_test_not-a-real-admin-key", "proj_abc"
			_, err := svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{CostAPIKey: &admin, CostScope: &scope})
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "OpenRouter's bill comes per request: %v", err)

			o, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{
				Name: "openai", Type: "openai", URL: "https://api.openai.com/v1", APIKey: "koinon_test_not-a-real-token",
			})
			Expect(err).NotTo(HaveOccurred())
			got, err := svc.UpdateProvider(ctx, org, actor, o.ID, catalog.ProviderPatch{CostAPIKey: &admin, CostScope: &scope})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.HasCostKey).To(BeTrue())

			By("an OpenAI scope may name one API key instead of a project")
			key := "key_abc123"
			got, err = svc.UpdateProvider(ctx, org, actor, o.ID, catalog.ProviderPatch{CostScope: &key})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CostScope).To(Equal(key))
			sk := "sk-proj-abc"
			_, err = svc.UpdateProvider(ctx, org, actor, o.ID, catalog.ProviderPatch{CostScope: &sk})
			Expect(errors.As(err, &inv)).To(BeTrue(), "the key's ID, never the key: %v", err)
		})
	})

	Describe("models", func() {
		It("serves a model, refuses rates on it, and allows the guard on OpenRouter only", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "")
			m, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "deepseek/flash", Guard: true})
			Expect(err).NotTo(HaveOccurred())
			rows, err := store.New(pool).ListServedCatalog(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))

			var inv *catalog.InvalidError
			_, err = svc.UpdateModel(ctx, org, actor, m.ID, catalog.ModelPatch{Rates: &catalog.Rates{Input: "0.30", Output: "1.20"}})
			Expect(errors.As(err, &inv)).To(BeTrue(), "rates were replaced by prices")
			Expect(inv.Msg).To(ContainSubstring("price"))
			_, err = svc.UpdateModel(ctx, org, actor, m.ID, catalog.ModelPatch{ClearRates: true})
			Expect(errors.As(err, &inv)).To(BeTrue())

			ollama, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "ollama", Type: "ollama", URL: "http://x.example.com/v1"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: ollama.ID, Name: "q", Guard: true})
			Expect(errors.As(err, &inv)).To(BeTrue())
		})

		It("makes the org's first model its default model and never moves it (old issue 256)", func(ctx SpecContext) {
			q := store.New(pool)
			def := func() string {
				GinkgoHelper()
				d, err := q.GetOrgDefaultModel(ctx, org)
				Expect(err).NotTo(HaveOccurred())
				return d
			}
			Expect(def()).To(BeEmpty())
			z, a := create(ctx, "z-first", ""), create(ctx, "a-second", "")
			_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: z.ID, Name: "zz"})
			Expect(err).NotTo(HaveOccurred())
			Expect(def()).To(Equal("zz"))
			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: a.ID, Name: "aa"})
			Expect(err).NotTo(HaveOccurred())
			Expect(def()).To(Equal("zz"), "a model that sorts first does not take the default")
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.default_model.set'`, org).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))
		})

		Describe("deleting the default model (old issue 256)", func() {
			var q *store.Queries
			def := func(ctx SpecContext) string {
				GinkgoHelper()
				d, err := q.GetOrgDefaultModel(ctx, org)
				Expect(err).NotTo(HaveOccurred())
				return d
			}
			// clearedEvent expects exactly one audit event that moved the default from old to none.
			clearedEvent := func(ctx SpecContext, old string) {
				GinkgoHelper()
				ev := events(ctx, "org.default_model.set")
				Expect(ev).To(HaveLen(2), "the first model set it, the deletion cleared it")
				var last struct{ Before, After map[string]any }
				for _, e := range ev {
					var s struct{ Before, After map[string]any }
					Expect(json.Unmarshal(e.Summary, &s)).To(Succeed())
					if s.After["reason"] == "model deleted" {
						last = s
					}
				}
				Expect(last.Before).To(HaveKeyWithValue("model", old))
				Expect(last.After).To(HaveKeyWithValue("model", ""))
			}
			BeforeEach(func() { q = store.New(pool) })

			It("clears it when the model is deleted, picks no other, and leaves other deletes alone", func(ctx SpecContext) {
				p := create(ctx, "p", "")
				first, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "first"})
				Expect(err).NotTo(HaveOccurred())
				second, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "second"})
				Expect(err).NotTo(HaveOccurred())
				Expect(svc.DeleteModel(ctx, org, actor, second.ID)).To(Succeed())
				Expect(def(ctx)).To(Equal("first"), "deleting another model keeps the default")
				Expect(events(ctx, "org.default_model.set")).To(HaveLen(1))
				_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "third"})
				Expect(err).NotTo(HaveOccurred())
				Expect(svc.DeleteModel(ctx, org, actor, first.ID)).To(Succeed())
				Expect(def(ctx)).To(BeEmpty(), "no other model is picked silently")
				clearedEvent(ctx, "first")
			})

			It("clears it when the provider that serves it is deleted", func(ctx SpecContext) {
				p := create(ctx, "p", "")
				_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "first"})
				Expect(err).NotTo(HaveOccurred())
				Expect(svc.DeleteProvider(ctx, org, actor, p.ID)).To(Succeed())
				Expect(def(ctx)).To(BeEmpty())
				clearedEvent(ctx, "first")
			})

			It("clears it when a declared model is dropped from its Secret", func(ctx SpecContext) {
				d := catalog.DeclaredProvider{
					SecretName: "openrouter-secret", Version: "v1", Name: "openrouter", Type: "openrouter",
					URL: "https://openrouter.ai/api/v1", APIKey: "sk-declared",
					Models: []catalog.DeclaredModel{{Name: "deepseek/flash", Guard: true}},
				}
				_, err := svc.ApplyDeclaredProvider(ctx, org, d)
				Expect(err).NotTo(HaveOccurred())
				Expect(def(ctx)).To(Equal("deepseek/flash"))
				d.Version = "v2"
				d.Models = []catalog.DeclaredModel{{Name: "deepseek/other", Guard: true}}
				_, err = svc.ApplyDeclaredProvider(ctx, org, d)
				Expect(err).NotTo(HaveOccurred())
				Expect(def(ctx)).To(BeEmpty())
				clearedEvent(ctx, "deepseek/flash")
			})
		})

		It("adopts a first model whose name is the longest the catalog accepts", func(ctx SpecContext) {
			p := create(ctx, "long", "")
			name := strings.Repeat("m", 256)
			_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: name})
			Expect(err).NotTo(HaveOccurred())
			var d *string
			Expect(pool.QueryRow(ctx, `SELECT default_model FROM orgs WHERE id = $1`, org).Scan(&d)).To(Succeed())
			Expect(d).NotTo(BeNil())
			Expect(*d).To(Equal(name))
		})

		It("keeps model names unique per org across providers", func(ctx SpecContext) {
			a, b := create(ctx, "a", ""), create(ctx, "b", "")
			_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: a.ID, Name: "m"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: b.ID, Name: "m"})
			Expect(err).To(MatchError(catalog.ErrConflict))
		})

		It("refuses rates when a model is created", func(ctx SpecContext) {
			p := create(ctx, "a", "")
			_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "m", Rates: &catalog.Rates{Input: "1", Output: "1"}})
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
		})

		It("does not find another org's provider or model", func(ctx SpecContext) {
			p := create(ctx, "a", "")
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err := svc.GetProvider(ctx, other, p.ID)
			Expect(err).To(MatchError(catalog.ErrNotFound))
			_, err = svc.CreateModel(ctx, other, audit.User(otherUser), otherUser, catalog.ModelInput{ProviderID: p.ID, Name: "m"})
			Expect(err).To(MatchError(catalog.ErrNotFound))
			Expect(svc.DeleteProvider(ctx, other, audit.User(otherUser), p.ID)).To(MatchError(catalog.ErrNotFound))
		})
	})

	Describe("model formats (ADR 0069)", func() {
		mk := func(ctx SpecContext, name, typ, url string) catalog.Provider {
			GinkgoHelper()
			p, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: name, Type: typ, URL: url})
			Expect(err).NotTo(HaveOccurred())
			return p
		}
		model := func(ctx SpecContext, p catalog.Provider, name, format string) catalog.Model {
			GinkgoHelper()
			m, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: name, Format: format})
			Expect(err).NotTo(HaveOccurred())
			return m
		}

		It("defaults from the table, and an override marks the model and is validated", func(ctx SpecContext) {
			direct := mk(ctx, "anthropic", "anthropic", "https://api.anthropic.com/v1")
			router := create(ctx, "openrouter", "")
			Expect(model(ctx, direct, "claude-haiku-4-5", "")).To(And(HaveField("Format", "anthropic"), HaveField("FormatOverridden", false)))
			Expect(model(ctx, router, "deepseek/flash", "")).To(HaveField("Format", "openai"))
			// OpenRouter's Claude models speak Messages like any other Claude (old issue 242).
			routed := model(ctx, router, "anthropic/claude-haiku-5.5", "")
			Expect(routed).To(And(HaveField("Format", "anthropic"), HaveField("FormatOverridden", false)))

			oai := "openai"
			upd, err := svc.UpdateModel(ctx, org, actor, routed.ID, catalog.ModelPatch{Format: &oai})
			Expect(err).NotTo(HaveOccurred())
			Expect(upd).To(And(HaveField("Format", "openai"), HaveField("FormatOverridden", true)))

			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: direct.ID, Name: "x", Format: "openai"})
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
			Expect(err).To(MatchError(ContainSubstring("does not offer")))
			bad := "gemini"
			_, err = svc.UpdateModel(ctx, org, actor, routed.ID, catalog.ModelPatch{Format: &bad})
			Expect(err).To(MatchError(ContainSubstring("unknown model format")))
			Expect(events(ctx, "model.update")[0].Summary).To(ContainSubstring("formatOverridden"))
		})

		It("re-derives only models that are not overridden, audited as koinon-api", func(ctx SpecContext) {
			router := create(ctx, "openrouter", "")
			follows := model(ctx, router, "deepseek/flash", "")
			overridden := model(ctx, router, "anthropic/claude-a", "anthropic")
			current := model(ctx, router, "plain", "")
			// A default that changed since the row was written: the table says openai, the row anthropic.
			_, err := pool.Exec(ctx, `UPDATE models SET format = 'anthropic' WHERE org_id = $1 AND id = $2`, org, mustUUID(follows.ID))
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE models SET format = 'openai' WHERE org_id = $1 AND id = $2`, org, mustUUID(overridden.ID))
			Expect(err).NotTo(HaveOccurred())

			n, err := svc.RederiveFormats(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeNumerically(">=", 1))
			got := func(m catalog.Model) catalog.Model {
				r, err := svc.GetModel(ctx, org, m.ID)
				Expect(err).NotTo(HaveOccurred())
				return r
			}
			Expect(got(follows)).To(And(HaveField("Format", "openai"), HaveField("FormatOverridden", false)))
			Expect(got(overridden)).To(And(HaveField("Format", "openai"), HaveField("FormatOverridden", true)))
			Expect(got(current).Format).To(Equal("openai"))

			ev := events(ctx, "model.format_rederived")
			Expect(ev).To(HaveLen(1))
			Expect(ev[0].ActorKind).To(Equal("system"))
			Expect(ev[0].ActorID).To(Equal("koinon-api"))
			Expect(ev[0].TargetID).To(Equal(follows.ID))

			n, err = svc.RederiveFormats(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(events(ctx, "model.format_rederived")).To(HaveLen(1), "a second run changes nothing (%d)", n)
		})

		It("leaves the models of a deleted org alone", func(ctx SpecContext) {
			router := create(ctx, "openrouter", "")
			m := model(ctx, router, "deepseek/flash", "")
			_, err := pool.Exec(ctx, `UPDATE models SET format = 'anthropic' WHERE org_id = $1 AND id = $2`, org, mustUUID(m.ID))
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, org)
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.RederiveFormats(ctx)
			Expect(err).NotTo(HaveOccurred())
			var f string
			Expect(pool.QueryRow(ctx, `SELECT format FROM models WHERE org_id = $1 AND id = $2`, org, mustUUID(m.ID)).Scan(&f)).To(Succeed())
			Expect(f).To(Equal("anthropic"))
		})

		It("keeps formats within what a provider's new type offers", func(ctx SpecContext) {
			router := create(ctx, "openrouter", "")
			m := model(ctx, router, "anthropic/claude-a", "anthropic")
			openai := "openai"
			_, err := svc.UpdateProvider(ctx, org, actor, router.ID, catalog.ProviderPatch{Type: &openai})
			Expect(err).To(MatchError(ContainSubstring("does not offer")))
			back := ""
			_, err = svc.UpdateModel(ctx, org, actor, m.ID, catalog.ModelPatch{Format: &back})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.UpdateProvider(ctx, org, actor, router.ID, catalog.ProviderPatch{Type: &openai})
			Expect(err).NotTo(HaveOccurred())
		})

		It("stores the workspace ID for anthropic providers only", func(ctx SpecContext) {
			_, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "o", Type: "openai", URL: "https://api.openai.com/v1", AnthropicWorkspaceID: "wrkspc_01"})
			Expect(err).To(MatchError(ContainSubstring("anthropic providers only")))
			for _, id := range []string{"wrkspc_", "wrkspc_a-b", "ws_01", "wrkspc_01 "} {
				_, err = svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "a", Type: "anthropic", URL: "https://api.anthropic.com/v1", AnthropicWorkspaceID: id})
				Expect(err).To(MatchError(ContainSubstring("wrkspc_")), id)
			}
			p, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "a", Type: "anthropic", URL: "https://api.anthropic.com/v1", AnthropicWorkspaceID: "wrkspc_01Ab"})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.AnthropicWorkspaceID).To(Equal("wrkspc_01Ab"))
			// The database refuses it for other types too, whatever the service does.
			_, err = pool.Exec(ctx, `UPDATE providers SET anthropic_workspace_id = 'wrkspc_01' WHERE org_id = $1 AND id = $2`, org, mustUUID(create(ctx, "other", "").ID))
			Expect(err).To(MatchError(ContainSubstring("providers_anthropic_workspace_id_type")))
		})

		It("applies declared formats: an override is kept, a missing format follows the default", func(ctx SpecContext) {
			d := catalog.DeclaredProvider{
				SecretName: "anthropic-secret", Version: "v1", Name: "anthropic", Type: "anthropic", URL: "https://api.anthropic.com/v1",
				AnthropicWorkspaceID: "wrkspc_01Ab",
				Models:               []catalog.DeclaredModel{{Name: "claude-haiku-4-5"}, {Name: "claude-opus", Format: "anthropic"}},
			}
			_, err := svc.ApplyDeclaredProvider(ctx, org, d)
			Expect(err).NotTo(HaveOccurred())
			models, _ := svc.ListModels(ctx, org)
			Expect(models).To(ConsistOf(
				And(HaveField("Name", "claude-haiku-4-5"), HaveField("Format", "anthropic"), HaveField("FormatOverridden", false)),
				And(HaveField("Name", "claude-opus"), HaveField("Format", "anthropic"), HaveField("FormatOverridden", true)),
			))
			ps, _ := svc.ListProviders(ctx, org)
			Expect(ps[0].AnthropicWorkspaceID).To(Equal("wrkspc_01Ab"))

			bad := d
			bad.Version = "v2"
			bad.Models = []catalog.DeclaredModel{{Name: "m", Format: "openai"}}
			_, err = svc.ApplyDeclaredProvider(ctx, org, bad)
			Expect(err).To(MatchError(ContainSubstring("does not offer")))
			bad.Models = nil
			bad.Type, bad.URL = "openai", "https://api.openai.com/v1"
			_, err = svc.ApplyDeclaredProvider(ctx, org, bad)
			Expect(err).To(MatchError(ContainSubstring("anthropic providers only")))
		})
	})

	Describe("declared by a Secret", func() {
		declared := func(version string, models ...catalog.DeclaredModel) catalog.DeclaredProvider {
			return catalog.DeclaredProvider{
				SecretName: "openrouter-secret", Version: version, Name: "openrouter", Type: "openrouter",
				URL: "https://openrouter.ai/api/v1", APIKey: "sk-declared", Models: models,
			}
		}
		flash := catalog.DeclaredModel{Name: "deepseek/flash", Guard: true}

		It("is read-only here, audited as the Secret, and applied once per version", func(ctx SpecContext) {
			changed, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			changed, err = svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeFalse())

			list, err := svc.ListProviders(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(list).To(HaveLen(1))
			Expect(list[0].Source).To(Equal("secret"))
			Expect(list[0].ManagedBySecret).To(Equal("openrouter-secret"))

			newURL := "https://elsewhere/v1"
			_, err = svc.UpdateProvider(ctx, org, actor, list[0].ID, catalog.ProviderPatch{URL: &newURL})
			Expect(err).To(MatchError(catalog.ErrReadOnly))
			Expect(err.Error()).To(ContainSubstring("openrouter-secret"))
			Expect(svc.DeleteProvider(ctx, org, actor, list[0].ID)).To(MatchError(catalog.ErrReadOnly))
			models, err := svc.ListModels(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(models).To(HaveLen(1))
			_, err = svc.UpdateModel(ctx, org, actor, models[0].ID, catalog.ModelPatch{Guard: new(false)})
			Expect(err).To(MatchError(catalog.ErrReadOnly))
			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: list[0].ID, Name: "extra"})
			Expect(err).To(MatchError(catalog.ErrReadOnly))

			ev := events(ctx, "provider.declare")
			Expect(ev).To(HaveLen(1))
			Expect(ev[0].ActorKind).To(Equal("secret"))
			Expect(ev[0].ActorID).To(Equal("openrouter-secret"))
			Expect(string(ev[0].Summary)).NotTo(ContainSubstring("sk-declared"))
		})

		It("writes the key again from the Secret when it was lost, even at the same version", func(ctx SpecContext) {
			_, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE providers SET api_key = NULL, credential_lost_at = now() WHERE org_id = $1`, org)
			Expect(err).NotTo(HaveOccurred())
			changed, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			list, err := svc.ListProviders(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(list[0].CredentialLostAt).To(BeNil())
			Expect(list[0].HasKey).To(BeTrue())
		})

		It("wins a name collision: the UI entry is disabled with a reason, and back when the Secret goes", func(ctx SpecContext) {
			ui := create(ctx, "openrouter", "sk-ui")
			uiModel, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: ui.ID, Name: "deepseek/flash"})
			Expect(err).NotTo(HaveOccurred())

			_, err = svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())

			got, err := svc.GetProvider(ctx, org, ui.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.DisabledReason).To(Equal(catalog.ReasonShadowedProvider))
			gm, err := svc.GetModel(ctx, org, uiModel.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(gm.DisabledReason).NotTo(BeEmpty())
			served, err := store.New(pool).ListServedCatalog(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(served).To(HaveLen(1))
			Expect(served[0].ModelID.String()).NotTo(Equal(uiModel.ID), "only the declared model is served")

			Expect(svc.RemoveDeclared(ctx, org, "openrouter-secret")).To(Succeed())
			got, err = svc.GetProvider(ctx, org, ui.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.DisabledReason).To(BeEmpty())
			gm, err = svc.GetModel(ctx, org, uiModel.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(gm.DisabledReason).To(BeEmpty())
			list, _ := svc.ListProviders(ctx, org)
			Expect(list).To(HaveLen(2), "the removed declared provider stays")
			Expect(events(ctx, "provider.undeclare")).To(HaveLen(1))
		})

		It("disables, not deletes, what a deleted Secret declared; restoring the Secret restores it; an admin may delete it", func(ctx SpecContext) {
			_, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			Expect(svc.RemoveDeclared(ctx, org, "openrouter-secret")).To(Succeed())
			Expect(svc.RemoveDeclared(ctx, org, "openrouter-secret")).To(Succeed(), "idempotent")
			Expect(events(ctx, "provider.undeclare")).To(HaveLen(1))

			list, _ := svc.ListProviders(ctx, org)
			Expect(list).To(HaveLen(1))
			Expect(list[0].Removed).To(BeTrue())
			Expect(list[0].DisabledReason).To(Equal(catalog.ReasonRemoved))
			models, _ := svc.ListModels(ctx, org)
			Expect(models).To(HaveLen(1))
			Expect(models[0].DisabledReason).To(Equal(catalog.ReasonRemoved))
			served, err := store.New(pool).ListServedCatalog(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(served).To(BeEmpty())
			newURL := "https://elsewhere.example.com/v1"
			_, err = svc.UpdateProvider(ctx, org, actor, list[0].ID, catalog.ProviderPatch{URL: &newURL})
			Expect(err).To(MatchError(catalog.ErrReadOnly))

			By("restoring the Secret, even with the same version, serves it again")
			changed, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			list, _ = svc.ListProviders(ctx, org)
			Expect(list[0].Removed).To(BeFalse())
			Expect(list[0].DisabledReason).To(BeEmpty())
			served, _ = store.New(pool).ListServedCatalog(ctx, org)
			Expect(served).To(HaveLen(1))

			By("a removed entry can be deleted for good, a live one cannot")
			Expect(svc.DeleteProvider(ctx, org, actor, list[0].ID)).To(MatchError(catalog.ErrReadOnly))
			Expect(svc.RemoveDeclared(ctx, org, "openrouter-secret")).To(Succeed())
			Expect(svc.DeleteProvider(ctx, org, actor, list[0].ID)).To(Succeed())
			list, _ = svc.ListProviders(ctx, org)
			Expect(list).To(BeEmpty())
			models, _ = svc.ListModels(ctx, org)
			Expect(models).To(BeEmpty())
		})

		It("updates models with the Secret and drops the ones it no longer lists", func(ctx SpecContext) {
			_, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash, catalog.DeclaredModel{Name: "old"}))
			Expect(err).NotTo(HaveOccurred())
			cheaper := flash
			cheaper.Guard = false
			_, err = svc.ApplyDeclaredProvider(ctx, org, declared("v2", cheaper))
			Expect(err).NotTo(HaveOccurred())
			models, _ := svc.ListModels(ctx, org)
			Expect(models).To(HaveLen(1))
			Expect(models[0].Guard).To(BeFalse())
		})

		It("refuses a provider name another Secret declares, and an invalid model, changing nothing", func(ctx SpecContext) {
			_, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			other := declared("v1")
			other.SecretName = "second"
			_, err = svc.ApplyDeclaredProvider(ctx, org, other)
			Expect(err).To(MatchError(ContainSubstring("already declared by the Secret openrouter-secret")))

			bad := declared("v3", catalog.DeclaredModel{Name: "m", Guard: true})
			bad.Type = "ollama"
			_, err = svc.ApplyDeclaredProvider(ctx, org, bad)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
			list, _ := svc.ListProviders(ctx, org)
			Expect(list[0].Type).To(Equal("openrouter"))
		})
	})
})

func mustUUID(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}
