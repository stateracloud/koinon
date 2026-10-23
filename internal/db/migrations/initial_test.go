// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// inTx runs fn in a transaction and returns the commit error, so deferred
// constraint triggers are exercised.
func inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// newUser creates a user with its personal org and manual owner membership.
func newUser(ctx context.Context) (userID, orgID string) {
	err := inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Personal', 'personal') RETURNING id`,
			uniq("u")).Scan(&orgID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			uniq("sub"), orgID).Scan(&userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`,
			orgID, userID)
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	return userID, orgID
}

// newTeamOrg creates a team org owned by userID.
func newTeamOrg(ctx context.Context, userID string) (orgID string) {
	err := inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Team', 'team') RETURNING id`,
			uniq("team")).Scan(&orgID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`,
			orgID, userID)
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	return orgID
}

var _ = Describe("initial schema", func() {
	It("generates time-ordered UUIDv7 ids", func(ctx SpecContext) {
		_, orgID := newUser(ctx)
		var version int
		Expect(db.QueryRowContext(ctx, `SELECT uuid_extract_version($1::uuid)`, orgID).Scan(&version)).To(Succeed())
		Expect(version).To(Equal(7))
	})

	Describe("orgs", func() {
		It("rejects an org committed without a manual owner", func(ctx SpecContext) {
			err := inTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'x', 'team')`, uniq("orphan"))
				return err
			})
			Expect(err).To(MatchError(ContainSubstring("must have at least one manual owner")))
		})

		It("rejects slugs that are not DNS-safe", func(ctx SpecContext) {
			_, err := db.ExecContext(ctx,
				`INSERT INTO orgs (slug, display_name, kind) VALUES ('Bad_Slug', 'x', 'team')`)
			Expect(err).To(MatchError(ContainSubstring("orgs_slug_check")))
		})

		It("keeps the slug immutable", func(ctx SpecContext) {
			_, orgID := newUser(ctx)
			_, err := db.ExecContext(ctx, `UPDATE orgs SET slug = $1 WHERE id = $2`, uniq("renamed"), orgID)
			Expect(err).To(MatchError(ContainSubstring("slug is immutable")))
		})
	})

	Describe("users", func() {
		It("requires the personal org to be of kind personal", func(ctx SpecContext) {
			ownerID, _ := newUser(ctx)
			teamID := newTeamOrg(ctx, ownerID)
			_, err := db.ExecContext(ctx,
				`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2)`,
				uniq("sub"), teamID)
			Expect(err).To(MatchError(ContainSubstring("users_personal_org_id_personal_org_kind_fkey")))
		})
	})

	Describe("memberships", func() {
		It("refuses to remove the last manual owner", func(ctx SpecContext) {
			userID, orgID := newUser(ctx)
			err := inTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID)
				return err
			})
			Expect(err).To(MatchError(ContainSubstring("must have at least one manual owner")))
		})

		It("does not count an IdP-sourced owner as the manual owner", func(ctx SpecContext) {
			userID, _ := newUser(ctx)
			otherID, _ := newUser(ctx)
			orgID := newTeamOrg(ctx, userID)

			var mappingID string
			Expect(db.QueryRowContext(ctx,
				`INSERT INTO org_group_mappings (org_id, oidc_issuer, group_name, role) VALUES ($1, 'https://idp.test', 'admins', 'owner') RETURNING id`,
				orgID).Scan(&mappingID)).To(Succeed())
			_, err := db.ExecContext(ctx,
				`INSERT INTO org_memberships (org_id, user_id, source, group_mapping_id, role) VALUES ($1, $2, 'idp_group', $3, 'owner')`,
				orgID, otherID, mappingID)
			Expect(err).NotTo(HaveOccurred())

			err = inTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`DELETE FROM org_memberships WHERE org_id = $1 AND source = 'manual'`, orgID)
				return err
			})
			Expect(err).To(MatchError(ContainSubstring("must have at least one manual owner")))
		})

		It("computes the effective role as the highest across sources", func(ctx SpecContext) {
			ownerID, _ := newUser(ctx)
			memberID, _ := newUser(ctx)
			orgID := newTeamOrg(ctx, ownerID)

			var mappingID string
			Expect(db.QueryRowContext(ctx,
				`INSERT INTO org_group_mappings (org_id, oidc_issuer, group_name, role) VALUES ($1, 'https://idp.test', 'eng', 'admin') RETURNING id`,
				orgID).Scan(&mappingID)).To(Succeed())
			_, err := db.ExecContext(ctx,
				`INSERT INTO org_memberships (org_id, user_id, source, group_mapping_id, role) VALUES
				        ($1, $2, 'manual', NULL, 'viewer'), ($1, $2, 'idp_group', $3, 'admin')`,
				orgID, memberID, mappingID)
			Expect(err).NotTo(HaveOccurred())

			var role string
			Expect(db.QueryRowContext(ctx,
				`SELECT max(role)::text FROM org_memberships WHERE org_id = $1 AND user_id = $2`,
				orgID, memberID).Scan(&role)).To(Succeed())
			Expect(role).To(Equal("admin"))
		})

		It("requires a group mapping for IdP-sourced rows", func(ctx SpecContext) {
			ownerID, _ := newUser(ctx)
			orgID := newTeamOrg(ctx, ownerID)
			_, err := db.ExecContext(ctx,
				`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'idp_group', 'admin')`,
				orgID, ownerID)
			Expect(err).To(MatchError(ContainSubstring("org_memberships_check")))
		})

		It("rejects a group mapping from another org", func(ctx SpecContext) {
			ownerID, _ := newUser(ctx)
			memberID, _ := newUser(ctx)
			orgA := newTeamOrg(ctx, ownerID)
			orgB := newTeamOrg(ctx, ownerID)

			var mappingB string
			Expect(db.QueryRowContext(ctx,
				`INSERT INTO org_group_mappings (org_id, oidc_issuer, group_name, role) VALUES ($1, 'https://idp.test', 'eng', 'member') RETURNING id`,
				orgB).Scan(&mappingB)).To(Succeed())
			_, err := db.ExecContext(ctx,
				`INSERT INTO org_memberships (org_id, user_id, source, group_mapping_id, role) VALUES ($1, $2, 'idp_group', $3, 'member')`,
				orgA, memberID, mappingB)
			Expect(err).To(MatchError(ContainSubstring("org_memberships_org_id_group_mapping_id_fkey")))
		})
	})
})
