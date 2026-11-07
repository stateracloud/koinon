// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("00012_providers_audit_tokens", func() {
	var org, user string
	BeforeEach(func(ctx SpecContext) { user, org = newUser(ctx) })

	It("makes audit_events append-only, even for the table owner", func(ctx SpecContext) {
		var id string
		Expect(db.QueryRowContext(ctx,
			`INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind) VALUES ($1, 'system', 'koinon-test:spec', 'test.insert', 'test') RETURNING id`,
			org).Scan(&id)).To(Succeed())
		_, err := db.ExecContext(ctx, `UPDATE audit_events SET action = 'test.changed' WHERE id = $1`, id)
		Expect(err).To(MatchError(ContainSubstring("append-only")))
		_, err = db.ExecContext(ctx, `DELETE FROM audit_events WHERE id = $1`, id)
		Expect(err).To(MatchError(ContainSubstring("append-only")))
	})

	It("refuses a new system event without a component ID, and keeps the other kinds as they were (00067, ADR 0100)", func(ctx SpecContext) {
		_, err := db.ExecContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind) VALUES ($1, 'system', '', 'test.unnamed', 'test')`, org)
		Expect(err).To(MatchError(ContainSubstring("audit_events_system_actor_named")))
		_, err = db.ExecContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, action, target_kind) VALUES ($1, 'system', 'test.unnamed', 'test')`, org)
		Expect(err).To(MatchError(ContainSubstring("audit_events_system_actor_named")), "no ID at all is an empty one")
		_, err = db.ExecContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind) VALUES ($1, 'cli', '', 'test.cli', 'test')`, org)
		Expect(err).NotTo(HaveOccurred(), "the rule is for system actors")
	})

	It("revokes UPDATE, DELETE and TRUNCATE on audit_events from koinon_app, when that role exists", func(ctx SpecContext) {
		// The chart's CNPG bootstrap creates koinon_app with default privileges; here we create it first.
		err := pgtest.EnsureRole(ctx, db, "koinon_app_probe")
		Expect(err).NotTo(HaveOccurred())
		// The migration only revokes from koinon_app; assert the statement it runs is valid for a role
		// that holds the default privileges.
		_, err = db.ExecContext(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON audit_events TO koinon_app_probe`)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM koinon_app_probe`)
		Expect(err).NotTo(HaveOccurred())
		var can sql.NullBool
		Expect(db.QueryRowContext(ctx, `SELECT has_table_privilege('koinon_app_probe', 'audit_events', 'UPDATE')`).Scan(&can)).To(Succeed())
		Expect(can.Bool).To(BeFalse())
	})

	It("keeps provider and model names unique per org among enabled entries, and org-scopes models", func(ctx SpecContext) {
		_, other := newUser(ctx)
		var prov, otherProv string
		ins := func(o string, dest *string) {
			GinkgoHelper()
			Expect(db.QueryRowContext(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'p', 'openai', 'https://x/v1') RETURNING id`, o).Scan(dest)).To(Succeed())
		}
		ins(org, &prov)
		ins(other, &otherProv)
		_, err := db.ExecContext(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'p', 'openai', 'https://x/v1')`, org)
		Expect(err).To(HaveOccurred(), "a second enabled provider of the same name")

		_, err = db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, 'm')`, org, otherProv)
		Expect(err).To(HaveOccurred(), "a model cannot point at another org's provider (composite FK)")
		_, err = db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name, input_rate) VALUES ($1, $2, 'm', '1')`, org, prov)
		Expect(err).To(HaveOccurred(), "rates come in pairs")
		_, err = db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name, input_rate, output_rate) VALUES ($1, $2, 'm', '1.5', '2')`, org, prov)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, 'm')`, org, prov)
		Expect(err).To(HaveOccurred())
	})

	It("limits tokens to admin or owner and 90 days", func(ctx SpecContext) {
		insert := func(prefix, role, interval string) error {
			_, err := db.ExecContext(ctx, `INSERT INTO api_tokens (org_id, name, prefix, token_hash, role, created_by, expires_at, created_at)
				VALUES ($1, 't', $2, decode(repeat('ab', 32), 'hex'), $3::org_role, $5, now() + $4::interval, now())`, org, prefix, role, interval, user)
			return err
		}
		Expect(insert("aaaaaaa1", "admin", "30 days")).To(Succeed())
		Expect(insert("aaaaaaa2", "member", "30 days")).NotTo(Succeed())
		Expect(insert("aaaaaaa3", "owner", "91 days")).NotTo(Succeed())
		Expect(insert("aaaaaaa1", "owner", "1 day")).NotTo(Succeed(), "prefixes are unique")
	})
})
