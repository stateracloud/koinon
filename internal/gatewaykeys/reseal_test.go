// SPDX-License-Identifier: Apache-2.0

package gatewaykeys_test

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

type fixedSource struct {
	pub secretstore.RecipientPublicKey
}

func (f fixedSource) Current() (secretstore.RecipientPublicKey, error) { return f.pub, nil }

// raceDB runs a hook before the first UPDATE of mcp_servers, as a concurrent API write would.
type raceDB struct {
	store.DBTX
	before func()
}

func (d *raceDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if d.before != nil && strings.HasPrefix(sql, "UPDATE mcp_servers") {
		f := d.before
		d.before = nil
		f()
	}
	return d.DBTX.Exec(ctx, sql, args...)
}

var _ = Describe("re-sealing MCP credentials after a rotation (ADR 0097 12.4, spec 6)", func() {
	const plaintext = "koinon_test_not-a-real-token-reseal"
	var (
		pool      *pgxpool.Pool
		org, user pgtype.UUID
		oldK, new *secretstore.RecipientKey
		admin     gatewaykeys.Admin
	)
	BeforeEach(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		var err error
		oldK, err = secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
		new, err = secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
		org, user, _ = pgtest.NewOrg(ctx, pool)
		Expect(gatewaykeys.Insert(ctx, pool, oldK.Public())).To(Succeed())
		_, err = gatewaykeys.Move(ctx, pool, oldK.ID(), gatewaykeys.Pending, gatewaykeys.Active)
		Expect(err).NotTo(HaveOccurred())
		admin = gatewaykeys.Admin{DB: pool}
	})
	seed := func(ctx SpecContext) pgtype.UUID {
		GinkgoHelper()
		svc := mcpservers.New(pool, nil, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"}).WithRecipient(fixedSource{pub: oldK.Public()})
		s, err := svc.Create(ctx, org, audit.User(user), user, mcpservers.Input{Name: pgtest.Uniq("m"), URL: "https://mcp.example.com/m", CredentialHeader: "X-Api-Key", CredentialValue: plaintext})
		Expect(err).NotTo(HaveOccurred())
		var id pgtype.UUID
		Expect(id.Scan(s.ID)).To(Succeed())
		return id
	}
	rotate := func(ctx SpecContext) {
		GinkgoHelper()
		Expect(gatewaykeys.Insert(ctx, pool, new.Public())).To(Succeed())
		_, err := admin.Activate(ctx)
		Expect(err).NotTo(HaveOccurred())
	}
	keyOf := func(ctx SpecContext, id pgtype.UUID) string {
		GinkgoHelper()
		var k string
		Expect(pool.QueryRow(ctx, `SELECT credential->>'recipient_key_id' FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, id).Scan(&k)).To(Succeed())
		return k
	}

	It("keeps the old key readable, moves credentials to the new key, then lets the old key retire", func(ctx SpecContext) {
		id := seed(ctx)
		rotate(ctx)
		By("a credential sealed to the old key stays under it until it is re-sealed")
		Expect(keyOf(ctx, id)).To(Equal(oldK.ID()))
		n, err := gatewaykeys.InUse(ctx, pool, oldK.ID())
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(1))

		keyring := secretstore.NewRecipientKeyring(oldK, new)
		By("a dry run writes nothing")
		rep, err := gatewaykeys.Resealer{DB: pool, Keyring: keyring, DryRun: true}.Run(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.WouldMove).To(Equal(1))
		Expect(keyOf(ctx, id)).To(Equal(oldK.ID()))

		By("a run moves it and a rerun moves nothing")
		rep, err = gatewaykeys.Resealer{DB: pool, Keyring: keyring}.Run(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Moved).To(Equal(1))
		Expect(rep.From).To(Equal(map[string]int{oldK.ID(): 1}))
		Expect(keyOf(ctx, id)).To(Equal(new.ID()))
		rep, err = gatewaykeys.Resealer{DB: pool, Keyring: keyring}.Run(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Moved + rep.WouldMove + rep.Skipped + rep.Failed).To(BeZero())

		By("the credential still opens, bound to its row, with the new key alone")
		var raw []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, id).Scan(&raw)).To(Succeed())
		env, err := mcpservers.ParseSealedCredential(raw)
		Expect(err).NotTo(HaveOccurred())
		got, err := new.Open(mcpservers.CredentialBinding(org, id), env)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(plaintext))

		By("no credential names the old key, so it retires")
		n, err = gatewaykeys.InUse(ctx, pool, oldK.ID())
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
	})

	It("does not overwrite a credential the API wrote after the read (the compare-and-swap)", func(ctx SpecContext) {
		id := seed(ctx)
		rotate(ctx)
		// The API sets the credential again, sealed to the old key with new bytes, between the read and the write.
		newer, err := secretstore.SealV2(oldK.Public(), mcpservers.CredentialBinding(org, id), []byte("koinon_test_not-a-real-token-newer"))
		Expect(err).NotTo(HaveOccurred())
		newerJSON := mustJSON(newer)
		db := &raceDB{DBTX: pool}
		db.before = func() {
			_, err := pool.Exec(ctx, `UPDATE mcp_servers SET credential = $3 WHERE org_id = $1 AND id = $2`, org, id, newerJSON)
			Expect(err).NotTo(HaveOccurred())
		}
		rep, err := gatewaykeys.Resealer{DB: db, Keyring: secretstore.NewRecipientKeyring(oldK, new)}.Run(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Skipped).To(Equal(1))
		Expect(rep.Moved).To(BeZero())
		var raw []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, id).Scan(&raw)).To(Succeed())
		Expect(string(raw)).To(MatchJSON(newerJSON))

		By("a rerun moves the newer value")
		rep, err = gatewaykeys.Resealer{DB: pool, Keyring: secretstore.NewRecipientKeyring(oldK, new)}.Run(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Moved).To(Equal(1))
	})

	It("fails a credential whose key this process does not hold, and moves the rest", func(ctx SpecContext) {
		id := seed(ctx)
		rotate(ctx)
		rep, err := gatewaykeys.Resealer{DB: pool, Keyring: secretstore.NewRecipientKeyring(new)}.Run(ctx)
		Expect(err).To(HaveOccurred())
		Expect(rep.Failures).To(Equal(map[string]int{gatewaykeys.ReasonUnknownKey: 1}))
		Expect(keyOf(ctx, id)).To(Equal(oldK.ID()))
		Expect(err.Error()).NotTo(ContainSubstring(plaintext))
	})

	It("refuses while the process does not hold the active key", func(ctx SpecContext) {
		rotate(ctx)
		_, err := gatewaykeys.Resealer{DB: pool, Keyring: secretstore.NewRecipientKeyring(oldK)}.Run(ctx)
		Expect(err).To(MatchError(ContainSubstring("does not hold the private key of the active key")))
	})
})

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return b
}
