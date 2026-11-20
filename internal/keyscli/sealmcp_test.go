// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// keys seal-mcp moves stored v1 MCP credentials to v2 (ADR 0097 12.5 step 2, spec 7).
var _ = Describe("koinon-api keys seal-mcp", func() {
	const plaintext = "koinon_test_not-a-real-token-seal-mcp"
	var (
		tool *keyscli.Tool
		out  *bytes.Buffer
		gk   *secretstore.RecipientKey
		ring *secretstore.Keyring
	)

	BeforeEach(func(ctx SpecContext) {
		pgtest.TruncateOrgs(ctx, pool)
		kek := make([]byte, secretstore.KeySize)
		_, _ = rand.Read(kek)
		const id = "kek-20260101-aaaa"
		kube := fake.NewClientset()
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
			Data:       map[string][]byte{id: secretstore.EncodeKey(kek), "active": []byte(id)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		store := keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek"}
		set, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		ring, err = secretstore.NewKeyring(set.Keys, set.Active)
		Expect(err).NotTo(HaveOccurred())
		out = &bytes.Buffer{}
		tool = &keyscli.Tool{Pool: pool, Store: store, Out: out, Now: time.Now}
		gk, err = secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
	})

	activate := func(ctx SpecContext) {
		GinkgoHelper()
		Expect(gatewaykeys.Insert(ctx, pool, gk.Public())).To(Succeed())
		_, err := gatewaykeys.Move(ctx, pool, gk.ID(), gatewaykeys.Pending, gatewaykeys.Active)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) {
			_, _ = pool.Exec(ctx, `DELETE FROM mcp_servers`)
			_, _ = gatewaykeys.Move(ctx, pool, gk.ID(), gatewaykeys.Active, gatewaykeys.Retiring)
			_, _ = gatewaykeys.Move(ctx, pool, gk.ID(), gatewaykeys.Retiring, gatewaykeys.Retired)
		})
	}
	seed := func(ctx SpecContext, org, user pgtype.UUID) pgtype.UUID {
		GinkgoHelper()
		svc := mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: ns})
		s, err := svc.Create(ctx, org, audit.User(user), user, mcpservers.Input{
			Name: pgtest.Uniq("m"), URL: "https://mcp.example.com/mcp", CredentialPrefix: "Bearer ", CredentialValue: plaintext,
		})
		Expect(err).NotTo(HaveOccurred())
		var id pgtype.UUID
		Expect(id.Scan(s.ID)).To(Succeed())
		return id
	}
	formats := func(ctx SpecContext) (v1, v2 int) {
		GinkgoHelper()
		Expect(pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE credential->>'fmt' IS DISTINCT FROM '2'), count(*) FILTER (WHERE credential->>'fmt' = '2') FROM mcp_servers WHERE credential IS NOT NULL`).Scan(&v1, &v2)).To(Succeed())
		return v1, v2
	}

	It("refuses while no gateway key is active, and writes nothing", func(ctx SpecContext) {
		org, user, _ := pgtest.NewOrg(ctx, pool)
		seed(ctx, org, user)
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{})).To(MatchError(keyscli.ErrNoActiveGatewayKey))
		v1, v2 := formats(ctx)
		Expect([]int{v1, v2}).To(Equal([]int{1, 0}))
	})

	It("moves every v1 credential once, reruns to nothing, and never prints a value", func(ctx SpecContext) {
		activate(ctx)
		orgA, userA, _ := pgtest.NewOrg(ctx, pool)
		orgB, userB, _ := pgtest.NewOrg(ctx, pool)
		a1, a2, b1 := seed(ctx, orgA, userA), seed(ctx, orgA, userA), seed(ctx, orgB, userB)

		By("a dry run reports and writes nothing")
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{DryRun: true})).To(Succeed())
		Expect(out.String()).To(ContainSubstring("3 MCP credential(s) would move"))
		v1, v2 := formats(ctx)
		Expect([]int{v1, v2}).To(Equal([]int{3, 0}))

		By("--org moves one org only")
		out.Reset()
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{Org: orgA.String()})).To(Succeed())
		v1, v2 = formats(ctx)
		Expect([]int{v1, v2}).To(Equal([]int{1, 2}))

		By("a full run moves the rest, and a rerun moves none")
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{})).To(Succeed())
		out.Reset()
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{})).To(Succeed())
		Expect(out.String()).To(ContainSubstring("0 MCP credential(s) moved"))
		v1, v2 = formats(ctx)
		Expect([]int{v1, v2}).To(Equal([]int{0, 3}))

		By("the gateway key opens each one, bound to its own row")
		for org, id := range map[pgtype.UUID]pgtype.UUID{orgA: a1, orgB: b1} {
			var raw []byte
			Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, id).Scan(&raw)).To(Succeed())
			env, err := mcpservers.ParseSealedCredential(raw)
			Expect(err).NotTo(HaveOccurred())
			Expect(env.RecipientKeyID).To(Equal(gk.ID()))
			got, err := gk.Open(mcpservers.CredentialBinding(org, id), env)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got)).To(Equal(plaintext))
		}
		_ = a2
		Expect(out.String()).NotTo(ContainSubstring(plaintext))
	})

	It("moves back with --to-v1 only where the identity holds the gateway key", func(ctx SpecContext) {
		activate(ctx)
		org, user, _ := pgtest.NewOrg(ctx, pool)
		id := seed(ctx, org, user)
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{})).To(Succeed())
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{ToV1: true})).To(MatchError(keyscli.ErrToV1Unavailable))

		tool.GatewayKeyring = func(ctx context.Context) (*secretstore.RecipientKeyring, error) {
			return secretstore.NewRecipientKeyring(gk), nil
		}
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{ToV1: true})).To(Succeed())
		v1, v2 := formats(ctx)
		Expect([]int{v1, v2}).To(Equal([]int{1, 0}))
		var raw []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, id).Scan(&raw)).To(Succeed())
		got, err := mcpservers.OpenCredential(ring, org, id, raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(plaintext))
	})

	It("keeps v2 unsealed while the write gate is shut", func(ctx SpecContext) {
		activate(ctx)
		org, user, _ := pgtest.NewOrg(ctx, pool)
		seed(ctx, org, user)
		tool.SealGate = func(context.Context) error { return errors.New("1 gateway pod(s) do not read mcp-credential-v2") }
		err := tool.SealMCP(ctx, keyscli.SealMCPOptions{})
		Expect(err).To(MatchError(ContainSubstring("nothing was changed")))
		v1, v2 := formats(ctx)
		Expect([]int{v1, v2}).To(Equal([]int{1, 0}))
	})

	It("counts v1 and v2 under the key ID in status and verify", func(ctx SpecContext) {
		activate(ctx)
		org, user, _ := pgtest.NewOrg(ctx, pool)
		seed(ctx, org, user)
		seed(ctx, org, user)
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{Org: org.String()})).To(Succeed())
		v2, v1, err := tool.V2Usage(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(v1).To(Equal(0))
		Expect(v2).To(HaveKeyWithValue(gk.ID(), 2))
		out.Reset()
		Expect(tool.Verify(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("MCP credentials: 0 in v1, 2 in v2"))
	})

	It("refuses an --org that is not a UUID", func(ctx SpecContext) {
		Expect(tool.SealMCP(ctx, keyscli.SealMCPOptions{Org: "acme"})).To(MatchError(ContainSubstring("not a UUID")))
	})
})
