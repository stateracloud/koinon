// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// These specs skip without Docker (pgtest requires it); CI runs them.
var _ = Describe("CreateLocalUser", func() {
	const pw = "koinon_test_not-a-real-password"

	It("creates a member who can be verified, with an audit event", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		_, err := Bootstrap(ctx, pool, BootstrapConfig{Username: "admin"}, time.Now())
		Expect(err).NotTo(HaveOccurred())

		u, err := CreateLocalUser(ctx, pool, audit.CLI("users create"), LocalUserSpec{
			Username: "drill-member", Password: pw, OrgSlug: DefaultOrgSlug, Role: store.OrgRoleMember,
		}, time.Now())
		Expect(err).NotTo(HaveOccurred())

		cred, err := store.New(pool).GetLocalCredentialByUsername(ctx, "drill-member")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.UserID).To(Equal(u.ID))
		Expect(cred.MustChangePassword).To(BeFalse())
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'org.member.added'`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("refuses a duplicate username, an unknown org, a bad role and a short password", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		_, err := Bootstrap(ctx, pool, BootstrapConfig{Username: "admin"}, time.Now())
		Expect(err).NotTo(HaveOccurred())
		spec := LocalUserSpec{Username: "drill-admin", Password: pw, OrgSlug: DefaultOrgSlug, Role: store.OrgRoleAdmin}
		_, err = CreateLocalUser(ctx, pool, audit.CLI("users create"), spec, time.Now())
		Expect(err).NotTo(HaveOccurred())

		_, err = CreateLocalUser(ctx, pool, audit.CLI("users create"), spec, time.Now())
		Expect(err).To(MatchError(ContainSubstring("exists already")))
		other := spec
		other.Username, other.OrgSlug = "x1", "no-such-org"
		_, err = CreateLocalUser(ctx, pool, audit.CLI("users create"), other, time.Now())
		Expect(err).To(MatchError(ContainSubstring("no org")))
		other.OrgSlug, other.Role = DefaultOrgSlug, "root"
		_, err = CreateLocalUser(ctx, pool, audit.CLI("users create"), other, time.Now())
		Expect(err).To(MatchError(ContainSubstring("role must be")))
		other.Role, other.Password = store.OrgRoleMember, "short"
		_, err = CreateLocalUser(ctx, pool, audit.CLI("users create"), other, time.Now())
		Expect(err).To(HaveOccurred())
	})
})
