// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// The upgrade preflight refuses while a v1 MCP credential exists (ADR 0097 12.5 step 3, spec 7).
var _ = Describe("the v1 MCP credential preflight", func() {
	It("refuses with a v1 credential, passes when none is left, and prints no value", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		pgtest.TruncateOrgs(ctx, pool)
		db := stdlib.OpenDBFromPool(pool)
		DeferCleanup(func() { _ = db.Close() })

		Expect(checkNoV1MCPCredentials(ctx, db)).To(Succeed())

		org, user, _ := pgtest.NewOrg(ctx, pool)
		key := make([]byte, secretstore.KeySize)
		ring, err := secretstore.NewKeyring(map[string][]byte{"kek-20260101-aaaa": key}, "kek-20260101-aaaa")
		Expect(err).NotTo(HaveOccurred())
		const plaintext = "koinon_test_not-a-real-token-guard"
		s, err := mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"}).
			Create(ctx, org, audit.User(user), user, mcpservers.Input{Name: pgtest.Uniq("m"), URL: "https://mcp.example.com/m", CredentialHeader: "X-Api-Key", CredentialValue: plaintext})
		Expect(err).NotTo(HaveOccurred())

		err = checkNoV1MCPCredentials(ctx, db)
		Expect(errors.Is(err, ErrV1MCPCredentials)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("1 credential(s)"))
		Expect(err.Error()).NotTo(ContainSubstring(plaintext))

		var id pgtype.UUID
		Expect(id.Scan(s.ID)).To(Succeed())
		_, err = pool.Exec(ctx, `UPDATE mcp_servers SET credential = NULL, credential_header = NULL WHERE org_id = $1 AND id = $2`, org, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(checkNoV1MCPCredentials(ctx, db)).To(Succeed())
	})
})
