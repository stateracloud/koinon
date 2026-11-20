// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/formatgate"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

type fixedKey struct {
	pub secretstore.RecipientPublicKey
	err error
}

func (f fixedKey) Current() (secretstore.RecipientPublicKey, error) { return f.pub, f.err }

// koinon-api seals v2 when a gateway key is active, and v1 otherwise (ADR 0097 decision 12, spec 1).
var _ = Describe("sealing a server credential to the gateway key", func() {
	const plaintext = "koinon_test_not-a-real-token-v2"
	var (
		org, user pgtype.UUID
		actor     audit.Actor
		gk        *secretstore.RecipientKey
	)
	BeforeEach(func(ctx SpecContext) {
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
		var err error
		gk, err = secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
	})
	svcWith := func(src mcpservers.RecipientSource) *mcpservers.Service {
		s := mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
		if src != nil {
			s = s.WithRecipient(src)
		}
		return s
	}
	activate := func(ctx SpecContext) {
		GinkgoHelper()
		Expect(gatewaykeys.Insert(ctx, pool, gk.Public())).To(Succeed())
		_, err := gatewaykeys.Move(ctx, pool, gk.ID(), gatewaykeys.Pending, gatewaykeys.Active)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) {
			_, _ = gatewaykeys.Move(ctx, pool, gk.ID(), gatewaykeys.Active, gatewaykeys.Retiring)
			_, _ = gatewaykeys.Move(ctx, pool, gk.ID(), gatewaykeys.Retiring, gatewaykeys.Retired)
		})
	}
	stored := func(ctx SpecContext, id string) []byte {
		GinkgoHelper()
		var sid pgtype.UUID
		Expect(sid.Scan(id)).To(Succeed())
		var b []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, sid).Scan(&b)).To(Succeed())
		return b
	}
	in := mcpservers.Input{Name: "sealed", URL: "https://mcp.example.com/sealed", CredentialHeader: "X-Api-Key", CredentialValue: plaintext}

	It("seals v2 with an active key; only the gateway key opens it, and the ordinary opener refuses it (spec 1)", func(ctx SpecContext) {
		activate(ctx)
		svc := svcWith(fixedKey{pub: gk.Public()})
		s, err := svc.Create(ctx, org, actor, user, in)
		Expect(err).NotTo(HaveOccurred())
		raw := stored(ctx, s.ID)
		Expect(raw).NotTo(ContainSubstring(plaintext))
		Expect(secretstore.EnvelopeFormat(raw)).To(Equal(secretstore.FormatV2))

		var sid pgtype.UUID
		Expect(sid.Scan(s.ID)).To(Succeed())
		// koinon-api's own opener (the KEK ring) cannot read it.
		_, err = mcpservers.OpenCredential(ring, org, sid, raw)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, secretstore.ErrUnsupportedFormat)).To(BeTrue())
		// The gateway key opens it, with the binding of this row.
		env, err := mcpservers.ParseSealedCredential(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(env.RecipientKeyID).To(Equal(gk.ID()))
		got, err := gk.Open(mcpservers.CredentialBinding(org, sid), env)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(plaintext))
	})

	It("seals v1 when no key is active", func(ctx SpecContext) {
		svc := svcWith(fixedKey{err: gatewaykeys.ErrNoActiveKey})
		s, err := svc.Create(ctx, org, actor, user, in)
		Expect(err).NotTo(HaveOccurred())
		raw := stored(ctx, s.ID)
		Expect(secretstore.EnvelopeFormat(raw)).To(Equal(secretstore.FormatV1))
		var sid pgtype.UUID
		Expect(sid.Scan(s.ID)).To(Succeed())
		got, err := mcpservers.OpenCredential(ring, org, sid, raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(plaintext))
	})

	It("seals v1 while the format gate is shut (not every component reads v2)", func(ctx SpecContext) {
		svc := svcWith(fixedKey{err: fmt.Errorf("%w: gateway pod(s) do not read it", formatgate.ErrNotReady)})
		s, err := svc.Create(ctx, org, actor, user, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(secretstore.EnvelopeFormat(stored(ctx, s.ID))).To(Equal(secretstore.FormatV1))
	})

	It("refuses the write, and never falls back to v1, when the key cannot be read", func(ctx SpecContext) {
		svc := svcWith(fixedKey{err: context.DeadlineExceeded})
		_, err := svc.Create(ctx, org, actor, user, in)
		Expect(err).To(HaveOccurred())
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM mcp_servers WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})
})
