// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/kekhealth"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
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
		_, err0 := pool.Exec(ctx, `DELETE FROM kek_fingerprints`)
		Expect(err0).NotTo(HaveOccurred())
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
		tool = &keyscli.Tool{Pool: pool, Store: ks, Out: out, Now: func() time.Time { return now }, ProbeTimeout: 5 * time.Second, ProbePoll: 10 * time.Millisecond}
		freeStorage()
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

	It("verify opens every value, changes nothing and names the values that do not open", func(ctx SpecContext) {
		ids := seed(ctx, 2)
		seedMCP(ctx)
		snapshot := func() string {
			var s string
			Expect(pool.QueryRow(ctx, `SELECT coalesce(string_agg(api_key::text, ',' ORDER BY id), '') FROM providers WHERE org_id = $1`, org).Scan(&s)).To(Succeed())
			return s
		}
		events := func() int {
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events`).Scan(&n)).To(Succeed())
			return n
		}
		before, nEvents := snapshot(), events()

		Expect(tool.Verify(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("3 value(s) opened, 0 failed."))
		Expect(snapshot()).To(Equal(before), "verify writes no row")
		Expect(events()).To(Equal(nEvents), "verify writes no audit event")

		By("a value sealed under a KEK the key set lacks")
		out.Reset()
		_, err := pool.Exec(ctx, `UPDATE providers SET api_key = jsonb_set(api_key, '{kek_id}', '"gone"') WHERE id = $1`, ids[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(tool.Verify(ctx)).To(MatchError(keyscli.ErrVerifyFailed))
		Expect(out.String()).To(And(
			ContainSubstring("FAIL providers.api_key org "+org.String()+" row "+ids[0]+": unknown_kek"),
			ContainSubstring("2 value(s) opened, 1 failed."),
			ContainSubstring("restore the key backup")))
		Expect(out.String()).NotTo(ContainSubstring("sk-"), "no plaintext is printed")
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

	It("rewraps and reencrypts home repository passwords, every repository epoch included", func(ctx SpecContext) {
		var agent pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'a', 'opencode') RETURNING id`, org).Scan(&agent)).To(Succeed())
		pw := checkpoint.Passwords{Q: store.New(pool), Keys: ring(ctx), Begin: pool.Begin}
		e0, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		e1, _, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, checkpoint.DefaultRetain)
		Expect(err).NotTo(HaveOccurred())
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
		now = now.Add(6 * time.Minute)
		Expect(tool.Retire(ctx, "kek-20260101-aaaa")).To(Succeed(), "no value uses the old KEK any more")

		pw.Keys = ring(ctx)
		Expect(pw.At(ctx, org, agent, 0)).To(Equal(e0))
		Expect(pw.At(ctx, org, agent, 1)).To(Equal(e1))
	})

	It("refuses to make a new KEK Secret when the database holds encrypted values", func(ctx SpecContext) {
		seed(ctx, 1)
		fresh := fake.NewClientset() // the Secret was deleted
		used := func(ctx context.Context) (bool, error) { return keyscli.AnyEnvelope(ctx, pool) }
		created, err := keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}.Ensure(ctx, now, used)
		Expect(err).To(MatchError(keyscli.ErrKEKMissing))
		Expect(err.Error()).To(ContainSubstring("restore the Secret from your backup"))
		Expect(created).To(BeFalse())
		list, _ := fresh.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		Expect(list.Items).To(BeEmpty(), "nothing was created")

		By("an MCP server credential alone also counts")
		pgtest.TruncateOrgs(ctx, pool)
		org, user, _ = pgtest.NewOrg(ctx, pool)
		seedMCP(ctx)
		_, err = keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}.Ensure(ctx, now, used)
		Expect(err).To(MatchError(keyscli.ErrKEKMissing))

		By("with no encrypted value yet, a fresh install creates it")
		pgtest.TruncateOrgs(ctx, pool)
		created, err = keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}.Ensure(ctx, now, used)
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
		created, err := keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}.Ensure(ctx, now, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		store := keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}
		first, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Keys).To(HaveLen(1))
		Expect(first.Active).To(MatchRegexp(`^kek-20261023-[0-9a-f]{4}$`))

		created, err = keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}.Ensure(ctx, now.Add(24*time.Hour), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		again, _ := store.Load(ctx)
		Expect(again.Keys).To(Equal(first.Keys))

		By("a Secret someone else made is left alone, whatever it holds")
		_, err = fresh.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: ns}, Data: map[string][]byte{"x": []byte("y")}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		created, err = keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "mine"}.Ensure(ctx, now, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		got, _ := fresh.CoreV1().Secrets(ns).Get(ctx, "mine", metav1.GetOptions{})
		Expect(got.Data).To(HaveLen(1))
	})

	It("exports the key set as a Secret manifest that restores the same keys", func(ctx SpecContext) {
		ids := seed(ctx, 1)
		Expect(tool.Export(ctx)).To(Succeed())
		Expect(out.String()).To(And(ContainSubstring("kind: Secret"), ContainSubstring("name: koinon-kek"), ContainSubstring("wrapped by local")))
		Expect(out.String()).NotTo(ContainSubstring("namespace:"), "a restore goes into any namespace")

		var sec corev1.Secret
		Expect(yaml.Unmarshal(out.Bytes(), &sec)).To(Succeed())
		restored := fake.NewClientset()
		sec.Namespace = "restored"
		_, err := restored.CoreV1().Secrets("restored").Create(ctx, &sec, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		back := keyscli.KubeStore{Client: restored, Namespace: "restored", Name: "koinon-kek"}
		r, err := back.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		orig, _ := ks.Load(ctx)
		Expect(r.Keys).To(Equal(orig.Keys))
		Expect(r.Active).To(Equal(orig.Active))
		ring2, err := secretstore.NewKeyring(r.Keys, r.Active)
		Expect(err).NotTo(HaveOccurred())
		openAll(ctx, ring2, ids)
	})

	It("imports an export over a KEK Secret that already holds other keys, and refuses what it cannot open", func(ctx SpecContext) {
		ids := seed(ctx, 1)
		Expect(tool.Export(ctx)).To(Succeed())
		backup := out.String()
		orig, _ := ks.Load(ctx)

		// A restored install: koinon-api made its own first KEK on the empty database.
		target := fake.NewClientset()
		store := keyscli.KubeStore{Client: target, Namespace: "restored", Name: "koinon-kek"}
		_, err := store.Ensure(ctx, now, nil)
		Expect(err).NotTo(HaveOccurred())
		first, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Keys).NotTo(Equal(orig.Keys))

		imp := &keyscli.Tool{Store: store, Out: &bytes.Buffer{}, In: strings.NewReader(backup)}
		Expect(imp.Import(ctx)).To(Succeed())
		got, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Keys).To(Equal(orig.Keys))
		Expect(got.Active).To(Equal(orig.Active))
		ring, err := secretstore.NewKeyring(got.Keys, got.Active)
		Expect(err).NotTo(HaveOccurred())
		openAll(ctx, ring, ids)

		// With no Secret yet, import creates it.
		empty := keyscli.KubeStore{Client: fake.NewClientset(), Namespace: "n", Name: "koinon-kek"}
		Expect((&keyscli.Tool{Store: empty, Out: &bytes.Buffer{}, In: strings.NewReader(backup)}).Import(ctx)).To(Succeed())

		// Input that is not a key set changes nothing.
		bad := &keyscli.Tool{Store: store, Out: &bytes.Buffer{}, In: strings.NewReader("kind: Secret\ndata:\n  active: bm9wZQ==\n")}
		Expect(bad.Import(ctx)).NotTo(Succeed())
		again, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Keys).To(Equal(orig.Keys))
	})

	Describe("with KEKs wrapped by an opted-in KMS", func() {
		var (
			kms            *secretstoretest.FakeKMS
			admin, service secretstore.Wrappers
		)
		// use makes the tool run as the key-admin Job (admin) or in the koinon-api pod (service).
		use := func(ws secretstore.Wrappers) {
			ks.Wrappers = ws
			tool.Store = ks
			tool.Wrappers = ws
		}
		BeforeEach(func(ctx SpecContext) {
			kms = &secretstoretest.FakeKMS{}
			var err error
			// Both read the bound-only marker and the install ID from the database, as koinon-api and
			// the key-admin Job do.
			admin, err = secretstore.NewKeyAdminWrappers(installid.BoundOnlySource(pool), kms)
			Expect(err).NotTo(HaveOccurred())
			service, err = secretstore.NewWrappers(installid.BoundOnlySource(pool), kms)
			Expect(err).NotTo(HaveOccurred())
			admin = admin.WithInstallID(installid.Source(pool))
			service = service.WithInstallID(installid.Source(pool))
			use(admin)
		})

		It("adds wrapped KEKs, keeps the local one readable, and moves the values over", func(ctx SpecContext) {
			ids := seed(ctx, 2)
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			raw, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw.Data["kek-20261023-bbbb"])).To(HavePrefix(`{"fmt":2,"backend":"fakekms","key":"koinon-test"`))
			install, err := installid.Read(ctx, pool)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw.Data["kek-20261023-bbbb"])).To(HaveSuffix(`"install":"`+install+`"}`), "bound to this database's install ID")
			Expect(raw.Data["kek-20260101-aaaa"]).NotTo(HavePrefix("{"), "the local KEK is not rewrapped")

			By("koinon-api, decrypt-only, can activate and rewrap: no KEK is wrapped again")
			use(service)
			kms.DecryptOnly.Store(true)
			Expect(tool.Activate(ctx, "kek-20261023-bbbb")).To(Succeed())
			Expect(tool.Rewrap(ctx)).To(Succeed())
			Expect(usage(ctx)).To(Equal(map[string]int{"kek-20261023-bbbb": 2}))
			openAll(ctx, ring(ctx), ids)

			out.Reset()
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).To(And(ContainSubstring("WRAPPED BY"), ContainSubstring("fakekms:koinon-test, bound"), ContainSubstring("local")))
			Expect(out.String()).To(ContainSubstring("The install ID is " + install))
			Expect(out.String()).NotTo(ContainSubstring("not bound to the install"))
		})

		It("does not open a KEK Secret of another install, and says so (old issue 615)", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			install, err := installid.Read(ctx, pool)
			Expect(err).NotTo(HaveOccurred())

			// The same Secret next to another install's database.
			use(service.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallB)))
			_, err = ks.Load(ctx)
			Expect(err).To(MatchError(secretstore.ErrOpen))
			Expect(err).NotTo(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("wrapped for install " + install + " and this is install " + secretstoretest.InstallB))
			out.Reset()
			Expect(tool.Export(ctx)).To(MatchError(secretstore.ErrOpen))
			Expect(out.String()).To(BeEmpty())
		})

		It("names a KEK wrapped before the binding as unbound, and still opens it (old issue 615)", func(ctx SpecContext) {
			legacy, err := kms.Wrap(ctx, newKey(), secretstoretest.LegacyKEKAAD("kek-20261001-old1"))
			Expect(err).NotTo(HaveOccurred())
			v, err := secretstore.StoredKEK{Backend: kms.Backend(), KeyID: kms.KeyID(), Wrapped: legacy}.Encode()
			Expect(err).NotTo(HaveOccurred())
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			sec.Data["kek-20261001-old1"] = v
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			use(service)
			Expect(tool.Activate(ctx, "kek-20261001-old1")).To(Succeed())
			out.Reset()
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).To(And(
				ContainSubstring("fakekms:koinon-test, unbound"),
				ContainSubstring("KEK kek-20261001-old1 is not bound to the install"),
				ContainSubstring("Bound only: not set."),
			))
		})

		It("shows the bound-only marker, and once it is set refuses a KEK stored unbound (old issue 615)", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			// Its own database: the suite's pool must stay without the marker for the other specs.
			marked := pgtest.Start(ctx)
			_, err := installid.SetBoundOnly(ctx, marked, secretstore.BoundOnlyRewrap)
			Expect(err).NotTo(HaveOccurred())
			markedService, err := secretstore.NewWrappers(installid.BoundOnlySource(marked), kms)
			Expect(err).NotTo(HaveOccurred())
			use(markedService.WithInstallID(installid.Source(pool)))
			tool.Pool = marked
			DeferCleanup(func() { tool.Pool = pool })

			out.Reset()
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).To(MatchRegexp(`Bound only: set at \d{4}-\d\d-\d\dT[0-9:]+Z, when the key-admin Job \(--rewrap-unbound\) bound the last unbound KEK`))

			By("unbound KEKs, from an old backup, no longer open, and status names every one of them")
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			for _, id := range []string{"kek-20261001-old1", "kek-20261001-old2"} {
				legacy, err := kms.Wrap(ctx, newKey(), secretstoretest.LegacyKEKAAD(id))
				Expect(err).NotTo(HaveOccurred())
				v, err := secretstore.StoredKEK{Backend: kms.Backend(), KeyID: kms.KeyID(), Wrapped: legacy}.Encode()
				Expect(err).NotTo(HaveOccurred())
				sec.Data[id] = v
			}
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			out.Reset()
			Expect(tool.Status(ctx)).To(MatchError(secretstore.ErrBoundOnly))
			Expect(out.String()).To(And(
				ContainSubstring("Bound only: set at"),
				ContainSubstring("REFUSED: KEK kek-20261001-old1 is stored unbound"),
				ContainSubstring("REFUSED: KEK kek-20261001-old2 is stored unbound"),
				ContainSubstring("rerun the key-admin Job"),
				Not(ContainSubstring("REFUSED: KEK kek-20261023-bbbb")),
			))
		})

		It("koinon-api never wraps a new KEK with the KMS: keys add and the first start point at the key-admin Job", func(ctx SpecContext) {
			use(service)
			kms.DecryptOnly.Store(true)
			Expect(tool.Add(ctx, "kek-20261023-cccc")).To(MatchError(secretstore.ErrKeyAdminWraps))
			Expect(tool.Add(ctx, "kek-20261023-cccc")).To(MatchError(ContainSubstring("new KEKs are wrapped by the key-admin Job")))

			fresh := fake.NewClientset()
			store := keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek", Wrappers: service}
			created, err := store.Ensure(ctx, now, nil)
			Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))
			Expect(created).To(BeFalse())
			list, _ := fresh.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
			Expect(list.Items).To(BeEmpty())
			Expect(kms.Wraps()).To(BeZero(), "Encrypt was never called")
			set, _ := ks.Load(ctx)
			Expect(set.Keys).To(HaveLen(1), "the key set is unchanged")
		})

		It("the key-admin Job creates the first KEK wrapped", func(ctx SpecContext) {
			fresh := fake.NewClientset()
			store := keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek", Wrappers: admin}
			created, err := store.Ensure(ctx, now, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(created).To(BeTrue())
			store.Wrappers = service
			set, err := store.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(set.Stored[set.Active].Backend).To(Equal(secretstoretest.FakeKMSBackend))

			By("an unreachable KMS stops the load, it does not create keys")
			kms.Down.Store(true)
			_, err = store.Load(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
		})

		It("exports a wrapped KEK still wrapped", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			use(service)
			set, _ := ks.Load(ctx)
			out.Reset()
			Expect(tool.Export(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("kek-20261023-bbbb fingerprint " + secretstore.Fingerprint(set.Keys["kek-20261023-bbbb"]) + ", wrapped by fakekms:koinon-test"))
			var sec corev1.Secret
			Expect(yaml.Unmarshal(out.Bytes(), &sec)).To(Succeed())
			Expect(string(sec.Data["kek-20261023-bbbb"])).To(HavePrefix("{"))
		})
	})

	Describe("KEK fingerprints (old issue 591)", func() {
		type row struct {
			fp, backend string
			activated   bool
		}
		recorded := func(ctx SpecContext, id string) (row, bool) {
			GinkgoHelper()
			var r row
			err := pool.QueryRow(ctx, `SELECT fingerprint, backend, activated_at IS NOT NULL FROM kek_fingerprints WHERE kek_id = $1`, id).Scan(&r.fp, &r.backend, &r.activated)
			if errors.Is(err, pgx.ErrNoRows) {
				return r, false
			}
			Expect(err).NotTo(HaveOccurred())
			return r, true
		}

		It("records the fingerprint and activation time when a KEK is activated, and status is quiet", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			_, ok := recorded(ctx, "kek-20261023-bbbb")
			Expect(ok).To(BeFalse(), "add records nothing")
			Expect(tool.Activate(ctx, "kek-20261023-bbbb")).To(Succeed())
			set, err := ks.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			r, ok := recorded(ctx, "kek-20261023-bbbb")
			Expect(ok).To(BeTrue())
			Expect(r).To(Equal(row{fp: secretstore.Fingerprint(set.Keys["kek-20261023-bbbb"]), backend: "local", activated: true}))
			Expect(out.String()).NotTo(ContainSubstring("WARNING"))

			out.Reset()
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).NotTo(ContainSubstring("WARNING"))
			Expect(out.String()).To(ContainSubstring("(local) is reachable"))
		})

		It("status warns when a loaded KEK differs from the one the database recorded", func(ctx SpecContext) {
			old := "kek-20260101-aaaa"
			set, _ := ks.Load(ctx)
			fp := secretstore.Fingerprint(set.Keys[old])
			_, err0 := kekhealth.Recorder{Q: store.New(pool)}.Record(ctx, secretstore.NewLoaded(set.Stored, set.Active, set.Keys, nil))
			Expect(err0).NotTo(HaveOccurred())
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).NotTo(ContainSubstring("WARNING"))

			By("a wrong backup is restored under the same ID")
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			sec.Data[old] = secretstore.EncodeKey(newKey())
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			out.Reset()
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).To(And(ContainSubstring("WARNING: KEK "+old), ContainSubstring("the database recorded "+fp)))
		})

		It("says a KMS key is unreachable, and warns about a local active KEK only when a KMS is configured", func(ctx SpecContext) {
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).NotTo(ContainSubstring("WARNING"), "a local-only install is never warned")

			kms := &secretstoretest.FakeKMS{}
			admin, err := secretstore.NewKeyAdminWrappers(secretstoretest.BoundOnlyUnset, kms)
			Expect(err).NotTo(HaveOccurred())
			admin = admin.WithInstallID(installid.Source(pool))
			ks.Wrappers = admin
			tool.Store, tool.Wrappers = ks, admin
			out.Reset()
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("WARNING: the active KEK kek-20260101-aaaa is a local key but a KMS (fakekms)"))

			By("activating a local KEK is refused while the KMS is the primary")
			Expect(tool.Activate(ctx, "kek-20260101-aaaa")).To(MatchError(ContainSubstring("the KMS fakekms is configured")))

			By("and allowed once the configured primary is local again (the rollback)")
			localOnly := secretstore.Wrappers{}
			ks.Wrappers = localOnly
			tool.Store, tool.Wrappers = ks, localOnly
			Expect(tool.Activate(ctx, "kek-20260101-aaaa")).To(Succeed())
			ks.Wrappers = admin
			tool.Store, tool.Wrappers = ks, admin

			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			Expect(tool.Activate(ctx, "kek-20261023-bbbb")).To(Succeed())
			r, ok := recorded(ctx, "kek-20261023-bbbb")
			Expect(ok).To(BeTrue())
			Expect(r.backend).To(Equal("fakekms"))
			out.Reset()
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("(fakekms:koinon-test) is reachable"))
			Expect(out.String()).NotTo(ContainSubstring("WARNING"))

			kms.Down.Store(true)
			out.Reset()
			Expect(tool.Status(ctx)).To(MatchError(secretstore.ErrUnreachable), "the load cannot unwrap the KEKs")
			Expect(out.String()).To(ContainSubstring("is unreachable"), "status says so before it fails")
		})
	})

	Describe("on a terminal", func() {
		var asked *bytes.Buffer
		ask := func(input string) {
			asked = &bytes.Buffer{}
			tool.Prompt = &cli.Prompter{In: strings.NewReader(input), Out: asked, Accessible: true}
		}
		active := func(ctx SpecContext) string {
			GinkgoHelper()
			set, err := ks.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			return set.Active
		}

		It("activate without an ID picks an inactive KEK, with its age and values, and confirms", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261001-bbbb")).To(Succeed())
			ask("1\nn\n")
			Expect(tool.Activate(ctx, "")).To(MatchError(cli.ErrCancelled))
			Expect(asked.String()).To(And(ContainSubstring("kek-20261001-bbbb  added 22 days ago  wraps 0 value(s)"),
				ContainSubstring("Activate kek-20261001-bbbb?")))
			Expect(asked.String()).NotTo(ContainSubstring("kek-20260101-aaaa"), "the active KEK is not offered")
			Expect(active(ctx)).To(Equal("kek-20260101-aaaa"), "declining changes nothing")

			ask("1\ny\n")
			Expect(tool.Activate(ctx, "")).To(Succeed())
			Expect(active(ctx)).To(Equal("kek-20261001-bbbb"))
		})

		It("activate with an ID confirms too, and --yes skips the confirmation", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261001-bbbb")).To(Succeed())
			ask("n\n")
			Expect(tool.Activate(ctx, "kek-20261001-bbbb")).To(MatchError(cli.ErrCancelled))
			ask("")
			tool.Yes = true
			Expect(tool.Activate(ctx, "kek-20261001-bbbb")).To(Succeed())
			Expect(asked.String()).To(BeEmpty())
		})

		It("activate has nothing to pick when only the active KEK exists", func(ctx SpecContext) {
			ask("1\n")
			Expect(tool.Activate(ctx, "")).To(MatchError(ContainSubstring("no inactive KEK")))
		})

		It("retire without an ID lists KEKs in use as unavailable and confirms with the escrow warning", func(ctx SpecContext) {
			seed(ctx, 2)
			Expect(tool.Add(ctx, "kek-20261001-bbbb")).To(Succeed())
			Expect(tool.Add(ctx, "kek-20261002-cccc")).To(Succeed())
			Expect(tool.Activate(ctx, "kek-20261001-bbbb")).To(Succeed())
			now = now.Add(6 * time.Minute)

			ask("1\nn\n")
			Expect(tool.Retire(ctx, "")).To(MatchError(cli.ErrCancelled))
			Expect(asked.String()).To(And(
				ContainSubstring("kek-20260101-aaaa  added"), ContainSubstring("2 value(s) still use KEK kek-20260101-aaaa"),
				ContainSubstring("kek-20261001-bbbb  added"), ContainSubstring("is the active KEK"),
				ContainSubstring("1. kek-20261002-cccc"), ContainSubstring("backup escrow")))
			set, _ := ks.Load(ctx)
			Expect(set.Keys).To(HaveLen(3))

			ask("1\ny\n")
			Expect(tool.Retire(ctx, "")).To(Succeed())
			set, _ = ks.Load(ctx)
			Expect(set.Keys).NotTo(HaveKey("kek-20261002-cccc"))
		})

		It("retire says so when no KEK can be retired", func(ctx SpecContext) {
			seed(ctx, 1)
			ask("1\n")
			Expect(tool.Retire(ctx, "")).To(MatchError(ContainSubstring("no KEK can be retired")))
		})

		It("rewrap confirms with the count of values it will change", func(ctx SpecContext) {
			seed(ctx, 3)
			Expect(tool.Add(ctx, "kek-20261001-bbbb")).To(Succeed())
			Expect(tool.Activate(ctx, "kek-20261001-bbbb")).To(Succeed())
			ask("n\n")
			Expect(tool.Rewrap(ctx)).To(MatchError(cli.ErrCancelled))
			Expect(asked.String()).To(ContainSubstring("Rewrap 3 value(s) under kek-20261001-bbbb?"))
			Expect(usage(ctx)).To(Equal(map[string]int{"kek-20260101-aaaa": 3}))
			ask("y\n")
			Expect(tool.Rewrap(ctx)).To(Succeed())
			Expect(usage(ctx)).To(Equal(map[string]int{"kek-20261001-bbbb": 3}))
		})

		It("reencrypt wants the active KEK ID typed", func(ctx SpecContext) {
			ids := seed(ctx, 1)
			var before []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, ids[0]).Scan(&before)).To(Succeed())
			changed := func() bool {
				var after []byte
				Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, ids[0]).Scan(&after)).To(Succeed())
				return !bytes.Equal(before, after)
			}

			ask("")
			Expect(tool.Reencrypt(ctx)).To(MatchError(cli.ErrCancelled))
			Expect(changed()).To(BeFalse())

			ask("yes\nkek-20260101-aaaa\n")
			Expect(tool.Reencrypt(ctx)).To(Succeed())
			Expect(asked.String()).To(And(ContainSubstring("Re-encrypt 1 value(s)"), ContainSubstring("type kek-20260101-aaaa to continue")))
			Expect(changed()).To(BeTrue())
		})

		It("does not ask without a prompter: a missing ID is the old error and nothing is confirmed", func(ctx SpecContext) {
			tool.Prompt = nil
			Expect(tool.Activate(ctx, "")).To(MatchError("a KEK ID is required"))
			Expect(tool.Rewrap(ctx)).To(Succeed())
			Expect(tool.Reencrypt(ctx)).To(Succeed())
		})
	})
})
