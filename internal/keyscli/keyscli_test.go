// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
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
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
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
		// Every spec starts with no values, so counts are exact. (TRUNCATE is the owner's;
		// the append-only trigger covers UPDATE and DELETE of audit_events only.)
		_, err := pool.Exec(ctx, `TRUNCATE orgs CASCADE`)
		Expect(err).NotTo(HaveOccurred())
		now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		kube = fake.NewClientset()
		first := "kek-20260101-aaaa"
		_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
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
				Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x/v1", APIKey: "sk-" + string(rune('a'+i)),
			})
			Expect(err).NotTo(HaveOccurred())
			ids = append(ids, p.ID)
		}
		return ids
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

		Expect(tool.Retire(ctx, old)).To(MatchError(ContainSubstring("2 value(s) still use")))
		Expect(tool.Retire(ctx, next)).To(MatchError(ContainSubstring("active KEK")))

		Expect(tool.Rewrap(ctx)).To(Succeed())
		Expect(usage(ctx)).To(Equal(map[string]int{next: 2}))
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

		By("rewrap is idempotent")
		out.Reset()
		Expect(tool.Rewrap(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("0 value(s) rewrapped"))
	})

	It("reencrypt seals every value under a new data key, connections included", func(ctx SpecContext) {
		ids := seed(ctx, 2)
		r := ring(ctx)
		connID := pgtype.UUID{Bytes: [16]byte{1, 2, 3}, Valid: true}
		env, err := connections.SealFields(r, org, connID, map[string][]byte{connections.GitHubPrivateKeyField: []byte("pem")})
		Expect(err).NotTo(HaveOccurred())
		_, err = store.New(pool).CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: connID, Connector: "github", DisplayName: "c", Config: []byte(`{}`),
			SecretEnvelope: env, Source: store.ConfigSourceUi,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(usage(ctx)).To(Equal(map[string]int{"kek-20260101-aaaa": 3}))

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
	})

	It("cannot change a key set it only reads from a directory", func(ctx SpecContext) {
		dir := keyscli.DirStore{Dir: GinkgoT().TempDir()}
		Expect(dir.Save(ctx, keyscli.KeySet{})).To(MatchError(ContainSubstring("cannot be changed here")))
	})

	It("refuses an existing or invalid KEK ID", func(ctx SpecContext) {
		Expect(tool.Add(ctx, "kek-20260101-aaaa")).To(MatchError(ContainSubstring("already exists")))
		Expect(tool.Add(ctx, "active")).To(MatchError(ContainSubstring("not a valid KEK ID")))
	})
})
