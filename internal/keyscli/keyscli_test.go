// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tracedest"
)

const ns = "koinon"

var _ = Describe("koinon-api keys", func() {
	var (
		kube *fake.Clientset
		ks   keyscli.KubeStore
		tool *keyscli.Tool
		out  *bytes.Buffer
		now  time.Time
		org  pgtype.UUID
		user pgtype.UUID
	)

	newKey := func() []byte {
		k := make([]byte, secretstore.KeySize)
		_, _ = rand.Read(k)
		return k
	}
	// ring is a keyring over the current key set, as the servers hold it.
	ring := func(ctx SpecContext) *secretstore.Keyring {
		GinkgoHelper()
		set, err := ks.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		r, err := secretstore.NewKeyring(set.Keys, set.Active)
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	usage := func(ctx SpecContext) map[string]int {
		GinkgoHelper()
		u, err := tool.Usage(ctx)
		Expect(err).NotTo(HaveOccurred())
		return u
	}

	BeforeEach(func(ctx SpecContext) {
		// Every spec starts with no values, so counts are exact. (The audit log refuses TRUNCATE,
		// so pgtest runs it with triggers off.)
		pgtest.TruncateOrgs(ctx, pool)
		now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		kube = fake.NewClientset()
		first := "kek-20260101-aaaa"
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
			Data: map[string][]byte{
				first:    secretstore.EncodeKey(newKey()),
				"active": []byte(first),
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ks = keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek"}
		out = &bytes.Buffer{}
		tool = &keyscli.Tool{Pool: pool, Store: ks, Out: out, Now: func() time.Time { return now }}
		org, user, _ = pgtest.NewOrg(ctx, pool)
	})

	seed := func(ctx SpecContext, n int) []string {
		GinkgoHelper()
		svc := catalog.New(pool, ring(ctx), nil)
		var ids []string
		for i := range n {
			p, err := svc.CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
				Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-" + string(rune('a'+i)),
			})
			Expect(err).NotTo(HaveOccurred())
			ids = append(ids, p.ID)
		}
		return ids
	}
	// seedMCP adds a remote MCP server whose credential is sealed under the active KEK.
	seedMCP := func(ctx SpecContext) pgtype.UUID {
		GinkgoHelper()
		svc := mcpservers.New(pool, ring(ctx), nil).WithURLPolicy(catalog.URLPolicy{Namespace: ns})
		s, err := svc.Create(ctx, org, audit.User(user), user, mcpservers.Input{
			Name: pgtest.Uniq("m"), URL: "https://mcp.example.com/mcp", CredentialPrefix: "Bearer ", CredentialValue: "fake-mcp-token",
		})
		Expect(err).NotTo(HaveOccurred())
		var id pgtype.UUID
		Expect(id.Scan(s.ID)).To(Succeed())
		return id
	}
	openMCP := func(ctx SpecContext, r *secretstore.Keyring, id pgtype.UUID) {
		GinkgoHelper()
		var env []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, id).Scan(&env)).To(Succeed())
		got, err := mcpservers.OpenCredential(r, org, id, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("fake-mcp-token"))
	}
	openAll := func(ctx SpecContext, r *secretstore.Keyring, ids []string) {
		GinkgoHelper()
		for i, id := range ids {
			var env []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, id).Scan(&env)).To(Succeed())
			var pid pgtype.UUID
			Expect(pid.Scan(id)).To(Succeed())
			got, err := catalog.OpenKey(r, org, pid, env)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal("sk-" + string(rune('a'+i))))
		}
	}

	It("lists the KEKs with the values each wraps", func(ctx SpecContext) {
		seed(ctx, 3)
		Expect(tool.Status(ctx)).To(Succeed())
		Expect(out.String()).To(And(ContainSubstring("kek-20260101-aaaa"), ContainSubstring("active"), ContainSubstring("3")))
		Expect(out.String()).To(ContainSubstring("active for 295 of 365 days"))
	})

	It("warns after the active period and when a value's KEK is missing", func(ctx SpecContext) {
		ids := seed(ctx, 1)
		now = now.AddDate(1, 0, 0)
		Expect(tool.Status(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("WARNING: the active KEK kek-20260101-aaaa has been active for"))

		out.Reset()
		set, _ := ks.Load(ctx)
		_, err := pool.Exec(ctx, `UPDATE providers SET api_key = jsonb_set(api_key, '{kek_id}', '"gone"') WHERE id = $1`, ids[0])
		Expect(err).NotTo(HaveOccurred())
		_ = set
		Expect(tool.Status(ctx)).To(Succeed())
		Expect(out.String()).To(And(ContainSubstring("MISSING"), ContainSubstring("which is not in the key set")))
	})

	It("rotates: add, activate, rewrap without decrypting payloads, then retire", func(ctx SpecContext) {
		ids := seed(ctx, 2)
		mcp := seedMCP(ctx)
		old := "kek-20260101-aaaa"
		before := map[string]json.RawMessage{}
		for _, id := range ids {
			var env []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, id).Scan(&env)).To(Succeed())
			before[id] = env
		}

		Expect(tool.Add(ctx, "")).To(Succeed())
		set, _ := ks.Load(ctx)
		Expect(set.Keys).To(HaveLen(2))
		var next string
		for id := range set.Keys {
			if id != old {
				next = id
			}
		}
		Expect(next).To(MatchRegexp(`^kek-20261023-[0-9a-f]{4}$`))
		Expect(set.Active).To(Equal(old), "add does not activate")

		Expect(tool.Activate(ctx, next)).To(Succeed())
		set, _ = ks.Load(ctx)
		Expect(set.Active).To(Equal(next))
		Expect(set.ActivatedAt).To(BeTemporally("==", now))

		Expect(tool.Retire(ctx, old)).To(MatchError(ContainSubstring("was activated")), "pods may not wrap with the new KEK yet")
		now = now.Add(6 * time.Minute)
		Expect(tool.Retire(ctx, old)).To(MatchError(ContainSubstring("3 value(s) still use")), "the MCP credential counts")
		Expect(tool.Retire(ctx, next)).To(MatchError(ContainSubstring("active KEK")))

		Expect(tool.Rewrap(ctx)).To(Succeed())
		Expect(usage(ctx)).To(Equal(map[string]int{next: 3}))
		for _, id := range ids {
			var after []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, id).Scan(&after)).To(Succeed())
			var a, b secretstore.Envelope
			Expect(json.Unmarshal(before[id], &a)).To(Succeed())
			Expect(json.Unmarshal(after, &b)).To(Succeed())
			Expect(b.Ciphertext).To(Equal(a.Ciphertext), "the payload is untouched")
			Expect(b.Nonce).To(Equal(a.Nonce))
			Expect(b.WrappedDEK).NotTo(Equal(a.WrappedDEK))
		}

		Expect(tool.Retire(ctx, old)).To(Succeed())
		set, _ = ks.Load(ctx)
		Expect(set.Keys).To(HaveLen(1))
		openAll(ctx, ring(ctx), ids)
		openMCP(ctx, ring(ctx), mcp)

		By("rewrap is idempotent")
		out.Reset()
		Expect(tool.Rewrap(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("0 value(s) rewrapped"))
	})

	It("reencrypt seals every value under a new data key, connections and MCP credentials included", func(ctx SpecContext) {
		ids := seed(ctx, 2)
		mcp := seedMCP(ctx)
		r := ring(ctx)
		connID := pgtype.UUID{Bytes: [16]byte{1, 2, 3}, Valid: true}
		env, err := connections.SealFields(r, org, connID, map[string][]byte{connections.GitHubPrivateKeyField: []byte("pem")})
		Expect(err).NotTo(HaveOccurred())
		_, err = store.New(pool).CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: connID, Connector: "github", DisplayName: "c", Config: []byte(`{}`),
			SecretEnvelope: env, Source: store.ConfigSourceUi,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(usage(ctx)).To(Equal(map[string]int{"kek-20260101-aaaa": 4}))
		var mcpBefore []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, mcp).Scan(&mcpBefore)).To(Succeed())

		var before []byte
		Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, ids[0]).Scan(&before)).To(Succeed())
		Expect(tool.Reencrypt(ctx)).To(Succeed())
		var after []byte
		Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, ids[0]).Scan(&after)).To(Succeed())
		var a, b secretstore.Envelope
		Expect(json.Unmarshal(before, &a)).To(Succeed())
		Expect(json.Unmarshal(after, &b)).To(Succeed())
		Expect(b.Ciphertext).NotTo(Equal(a.Ciphertext))
		Expect(b.WrappedDEK).NotTo(Equal(a.WrappedDEK))
		openAll(ctx, r, ids)
		row, err := store.New(pool).GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: connID})
		Expect(err).NotTo(HaveOccurred())
		fields, err := connections.OpenFields(r, row)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fields[connections.GitHubPrivateKeyField])).To(Equal("pem"))
		var mcpAfter []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, mcp).Scan(&mcpAfter)).To(Succeed())
		var ma, mb secretstore.Envelope
		Expect(json.Unmarshal(mcpBefore, &ma)).To(Succeed())
		Expect(json.Unmarshal(mcpAfter, &mb)).To(Succeed())
		Expect(mb.Ciphertext).NotTo(Equal(ma.Ciphertext), "the MCP credential is sealed again")
		openMCP(ctx, r, mcp)
	})

	It("rewraps and reencrypts trace destination credentials, the platform's single row included", func(ctx SpecContext) {
		r := ring(ctx)
		seal := func(b secretstore.Binding) []byte {
			env, err := r.Seal(b, []byte(`{"api_key":"koinon_test_not-a-real-token"}`))
			Expect(err).NotTo(HaveOccurred())
			raw, err := json.Marshal(env)
			Expect(err).NotTo(HaveOccurred())
			return raw
		}
		var id pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO trace_destinations (org_id, name, backend, endpoint_url) VALUES ($1, 'dd', 'datadog', 'https://otlp.example.com') RETURNING id`, org).Scan(&id)).To(Succeed())
		_, err := pool.Exec(ctx, `UPDATE trace_destinations SET credentials = $3 WHERE org_id = $1 AND id = $2`, org, id, seal(tracedest.Binding(org, id)))
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url, credentials) VALUES ('datadog', 'https://otlp.example.com', $1)
			ON CONFLICT (singleton) DO UPDATE SET credentials = EXCLUDED.credentials`, seal(tracedest.PlatformBinding()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _, _ = pool.Exec(ctx, `DELETE FROM platform_trace_destination`) })
		Expect(usage(ctx)).To(Equal(map[string]int{"kek-20260101-aaaa": 2}))

		Expect(tool.Add(ctx, "")).To(Succeed())
		set, _ := ks.Load(ctx)
		for k := range set.Keys {
			if k != "kek-20260101-aaaa" {
				Expect(tool.Activate(ctx, k)).To(Succeed())
			}
		}
		Expect(tool.Rewrap(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("2 value(s) rewrapped"))
		Expect(tool.Reencrypt(ctx)).To(Succeed())
		var raw []byte
		Expect(pool.QueryRow(ctx, `SELECT credentials FROM platform_trace_destination`).Scan(&raw)).To(Succeed())
		var env secretstore.Envelope
		Expect(json.Unmarshal(raw, &env)).To(Succeed())
		plain, err := ring(ctx).Open(tracedest.PlatformBinding(), env)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(plain)).To(ContainSubstring("api_key"))
	})

	It("refuses to make a new KEK Secret when the database holds encrypted values", func(ctx SpecContext) {
		seed(ctx, 1)
		fresh := fake.NewClientset() // the Secret was deleted
		used := func(ctx context.Context) (bool, error) { return keyscli.AnyEnvelope(ctx, pool) }
		created, err := keyscli.EnsureKubeSecret(ctx, fresh, ns, "koinon-kek", now, used)
		Expect(err).To(MatchError(keyscli.ErrKEKMissing))
		Expect(err.Error()).To(ContainSubstring("restore the Secret from your backup"))
		Expect(created).To(BeFalse())
		list, _ := fresh.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		Expect(list.Items).To(BeEmpty(), "nothing was created")

		By("an MCP server credential alone also counts")
		pgtest.TruncateOrgs(ctx, pool)
		org, user, _ = pgtest.NewOrg(ctx, pool)
		seedMCP(ctx)
		_, err = keyscli.EnsureKubeSecret(ctx, fresh, ns, "koinon-kek", now, used)
		Expect(err).To(MatchError(keyscli.ErrKEKMissing))

		By("with no encrypted value yet, a fresh install creates it")
		pgtest.TruncateOrgs(ctx, pool)
		created, err = keyscli.EnsureKubeSecret(ctx, fresh, ns, "koinon-kek", now, used)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
	})

	It("cannot change a key set it only reads from a directory", func(ctx SpecContext) {
		dir := keyscli.DirStore{Dir: GinkgoT().TempDir()}
		Expect(dir.Save(ctx, keyscli.KeySet{})).To(MatchError(ContainSubstring("cannot be changed here")))
	})

	It("refuses an existing or invalid KEK ID", func(ctx SpecContext) {
		Expect(tool.Add(ctx, "kek-20260101-aaaa")).To(MatchError(ContainSubstring("already exists")))
		Expect(tool.Add(ctx, "active")).To(MatchError(ContainSubstring("not a valid KEK ID")))
	})

	It("creates the KEK Secret once and never overwrites it", func(ctx SpecContext) {
		fresh := fake.NewClientset()
		created, err := keyscli.EnsureKubeSecret(ctx, fresh, ns, "koinon-kek", now, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		store := keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}
		first, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Keys).To(HaveLen(1))
		Expect(first.Active).To(MatchRegexp(`^kek-20261023-[0-9a-f]{4}$`))

		created, err = keyscli.EnsureKubeSecret(ctx, fresh, ns, "koinon-kek", now.Add(24*time.Hour), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		again, _ := store.Load(ctx)
		Expect(again.Keys).To(Equal(first.Keys))

		By("a Secret someone else made is left alone, whatever it holds")
		_, err = fresh.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: ns}, Data: map[string][]byte{"x": []byte("y")}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		created, err = keyscli.EnsureKubeSecret(ctx, fresh, ns, "mine", now, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		got, _ := fresh.CoreV1().Secrets(ns).Get(ctx, "mine", metav1.GetOptions{})
		Expect(got.Data).To(HaveLen(1))
	})
})
