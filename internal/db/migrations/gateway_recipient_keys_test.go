// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"database/sql"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// The gateway recipient keys (ADR 0097 decision 12, issue #1151, slice 2): the table is a guarded
// register, a v2 credential must name a live key, and each role of the lifecycle holds the privileges
// it needs and no more (spec 8 of the ADR's list).
var _ = Describe("gateway recipient keys", func() {
	const (
		pub1 = "koinon_test_not-a-real-public-key-0001-padding"
		key1 = "gk-0123456789abcdef"
		key2 = "gk-fedcba9876543210"
	)
	exec := func(ctx context.Context, q string, args ...any) error {
		_, err := db.ExecContext(ctx, q, args...)
		return err
	}
	insertKey := func(ctx context.Context, id, state string) error {
		extra := ""
		switch state {
		case "active":
			extra = ", activated_at"
		case "retiring":
			extra = ", activated_at, retiring_at"
		case "retired":
			extra = ", retired_at"
		}
		vals := strings.Repeat(", now()", strings.Count(extra, ","))
		return exec(ctx, `INSERT INTO gateway_recipient_keys (key_id, recipient, public_key, state`+extra+`)
			VALUES ($1, 'hpke-aaaa-bbbb-cccc', $2, $3`+vals+`)`, id, []byte(pub1), state)
	}
	BeforeEach(func(ctx SpecContext) {
		Expect(exec(ctx, `DELETE FROM gateway_recipient_key_loads`)).To(Succeed())
		Expect(exec(ctx, `ALTER TABLE gateway_recipient_keys DISABLE TRIGGER gateway_recipient_keys_guard`)).To(Succeed())
		Expect(exec(ctx, `DELETE FROM gateway_recipient_keys`)).To(Succeed())
		Expect(exec(ctx, `ALTER TABLE gateway_recipient_keys ENABLE TRIGGER gateway_recipient_keys_guard`)).To(Succeed())
	})

	It("never deletes a row, and moves its state forward only", func(ctx SpecContext) {
		Expect(insertKey(ctx, key1, "pending")).To(Succeed())
		Expect(exec(ctx, `DELETE FROM gateway_recipient_keys WHERE key_id = $1`, key1)).To(MatchError(ContainSubstring("never deleted")))
		Expect(exec(ctx, `TRUNCATE gateway_recipient_keys CASCADE`)).To(MatchError(ContainSubstring("never deleted")))
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET public_key = $2 WHERE key_id = $1`, key1, []byte("koinon_test_not-a-real-other-key-padding"))).
			To(MatchError(ContainSubstring("changes only its state")))
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'retiring', activated_at = now(), retiring_at = now() WHERE key_id = $1`, key1)).
			To(MatchError(ContainSubstring("cannot move")))
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'active', activated_at = now() WHERE key_id = $1`, key1)).To(Succeed())
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'pending', activated_at = NULL WHERE key_id = $1`, key1)).
			To(MatchError(ContainSubstring("cannot move")))
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'retiring', retiring_at = now() WHERE key_id = $1`, key1)).To(Succeed())
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'retired', retired_at = now() WHERE key_id = $1`, key1)).To(Succeed())
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'active', activated_at = now(), retired_at = NULL WHERE key_id = $1`, key1)).
			To(HaveOccurred(), "a retired key stays retired")
	})

	It("keeps at most one active key", func(ctx SpecContext) {
		Expect(insertKey(ctx, key1, "active")).To(Succeed())
		Expect(insertKey(ctx, key2, "active")).To(MatchError(ContainSubstring("one_active")))
		Expect(insertKey(ctx, key2, "pending")).To(Succeed())
		Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'active', activated_at = now() WHERE key_id = $1`, key2)).
			To(MatchError(ContainSubstring("one_active")))
	})

	It("refuses a malformed key ID, a state with the wrong times and a key nobody reported", func(ctx SpecContext) {
		Expect(insertKey(ctx, "gk-nothex", "pending")).To(HaveOccurred())
		Expect(exec(ctx, `INSERT INTO gateway_recipient_keys (key_id, recipient, public_key, state) VALUES ($1, 'hpke-aaaa-bbbb-cccc', $2, 'active')`,
			key1, []byte(pub1))).To(MatchError(ContainSubstring("times")))
		Expect(exec(ctx, `INSERT INTO gateway_recipient_key_loads (key_id, copy_id) VALUES ($1, 'pod-a')`, key1)).To(HaveOccurred())
	})

	Describe("the stored MCP credential", func() {
		var org string
		insertServer := func(ctx context.Context, credential string) error {
			return exec(ctx, `INSERT INTO mcp_servers (org_id, name, url, credential_header, credential)
				VALUES ($1, 'm-'||substr(md5(random()::text), 1, 8), 'https://mcp.example.com/m', 'X-Api-Key', $2::jsonb)`, org, credential)
		}
		BeforeEach(func(ctx SpecContext) {
			Expect(db.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at)
				VALUES ('gk-' || substr(md5(random()::text), 1, 8), 'Gateway keys', 'personal', now()) RETURNING id`).Scan(&org)).To(Succeed())
		})

		It("accepts a value of an earlier format and refuses a v2 value that names no live key", func(ctx SpecContext) {
			Expect(insertServer(ctx, `{"v":1,"note":"koinon_test_not-a-real-envelope"}`)).To(Succeed())
			v2 := func(id string) string {
				return `{"fmt":2,"recipient_key_id":"` + id + `","note":"koinon_test_not-a-real-envelope"}`
			}
			Expect(insertServer(ctx, v2(key1))).To(MatchError(ContainSubstring("active or retiring")), "no such key")
			Expect(insertKey(ctx, key1, "pending")).To(Succeed())
			Expect(insertServer(ctx, v2(key1))).To(MatchError(ContainSubstring("active or retiring")), "a pending key")
			Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'active', activated_at = now() WHERE key_id = $1`, key1)).To(Succeed())
			Expect(insertServer(ctx, v2(key1))).To(Succeed())

			var n int
			Expect(db.QueryRowContext(ctx, `SELECT gateway_recipient_key_in_use($1)`, key1).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1), "the count of the credentials that name the key")
			Expect(db.QueryRowContext(ctx, `SELECT gateway_recipient_key_in_use($1)`, key2).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())

			Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'retiring', retiring_at = now() WHERE key_id = $1`, key1)).To(Succeed())
			Expect(insertServer(ctx, v2(key1))).To(Succeed(), "a retiring key still opens its credentials")
			Expect(exec(ctx, `UPDATE gateway_recipient_keys SET state = 'retired', retired_at = now() WHERE key_id = $1`, key1)).To(Succeed())
			Expect(insertServer(ctx, v2(key1))).To(MatchError(ContainSubstring("active or retiring")), "a retired key")
		})
	})

	// Spec 8: the privileges of each role, read from the catalogue after the grants ran. The roles are
	// the chart's group roles; a login would only be a member of one of them.
	Describe("the grants", func() {
		priv := func(ctx context.Context, role, q string, args ...any) bool {
			GinkgoHelper()
			var ok sql.NullBool
			Expect(db.QueryRowContext(ctx, q, append([]any{role}, args...)...).Scan(&ok)).To(Succeed(), q)
			return ok.Bool
		}
		table := func(ctx context.Context, role, tbl, p string) bool {
			return priv(ctx, role, `SELECT has_table_privilege($1, $2, $3)`, tbl, p)
		}
		column := func(ctx context.Context, role, tbl, col, p string) bool {
			return priv(ctx, role, `SELECT has_column_privilege($1, $2, $3, $4)`, tbl, col, p)
		}
		BeforeEach(func(ctx SpecContext) {
			for _, r := range []string{migrations.GatewayRole, migrations.GatewayKeyAdminRole, migrations.GatewayResealRole} {
				Expect(pgtest.EnsureRole(ctx, db, r)).To(Succeed())
			}
			ok, err := migrations.GrantGateway(ctx, db)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			granted, err := migrations.GrantGatewayKeyRoles(ctx, db)
			Expect(err).NotTo(HaveOccurred())
			Expect(granted).To(ConsistOf(migrations.GatewayKeyAdminRole, migrations.GatewayResealRole))
		})

		It("lets the gateway read the public keys and report a load, and nothing on mcp_servers beyond the guard's columns", func(ctx SpecContext) {
			g := migrations.GatewayRole
			Expect(table(ctx, g, "gateway_recipient_keys", "SELECT")).To(BeTrue())
			Expect(table(ctx, g, "gateway_recipient_keys", "INSERT")).To(BeFalse())
			Expect(table(ctx, g, "gateway_recipient_keys", "UPDATE")).To(BeFalse())
			Expect(table(ctx, g, "gateway_recipient_key_loads", "INSERT")).To(BeTrue())
			Expect(column(ctx, g, "gateway_recipient_key_loads", "loaded_at", "UPDATE")).To(BeTrue())
			Expect(column(ctx, g, "gateway_recipient_key_loads", "key_id", "UPDATE")).To(BeFalse())
			Expect(table(ctx, g, "gateway_recipient_key_loads", "DELETE")).To(BeFalse())
			// It never writes mcp_servers and never reads the sealed column.
			Expect(table(ctx, g, "mcp_servers", "UPDATE")).To(BeFalse())
			Expect(column(ctx, g, "mcp_servers", "credential", "UPDATE")).To(BeFalse())
			Expect(column(ctx, g, "mcp_servers", "credential", "SELECT")).To(BeFalse())
		})

		It("lets the key-admin insert a key and move its state, and read no credential", func(ctx SpecContext) {
			k := migrations.GatewayKeyAdminRole
			Expect(table(ctx, k, "gateway_recipient_keys", "INSERT")).To(BeTrue())
			Expect(column(ctx, k, "gateway_recipient_keys", "state", "UPDATE")).To(BeTrue())
			Expect(column(ctx, k, "gateway_recipient_keys", "public_key", "UPDATE")).To(BeFalse())
			Expect(table(ctx, k, "gateway_recipient_keys", "DELETE")).To(BeFalse())
			Expect(table(ctx, k, "gateway_recipient_key_loads", "SELECT")).To(BeTrue())
			Expect(table(ctx, k, "gateway_recipient_key_loads", "INSERT")).To(BeFalse())
			Expect(priv(ctx, k, `SELECT has_function_privilege($1, 'gateway_recipient_key_in_use(text)', 'EXECUTE')`)).To(BeTrue())
			Expect(column(ctx, k, "mcp_servers", "credential", "SELECT")).To(BeFalse())
			Expect(column(ctx, k, "mcp_servers", "credential", "UPDATE")).To(BeFalse())
		})

		It("gives the re-seal role the compare-and-swap and nothing more", func(ctx SpecContext) {
			r := migrations.GatewayResealRole
			for _, c := range []string{"org_id", "id", "credential"} {
				Expect(column(ctx, r, "mcp_servers", c, "SELECT")).To(BeTrue(), c)
			}
			Expect(column(ctx, r, "mcp_servers", "url", "SELECT")).To(BeFalse())
			Expect(column(ctx, r, "mcp_servers", "credential", "UPDATE")).To(BeTrue())
			Expect(column(ctx, r, "mcp_servers", "url", "UPDATE")).To(BeFalse())
			Expect(table(ctx, r, "mcp_servers", "INSERT")).To(BeFalse())
			Expect(table(ctx, r, "mcp_servers", "DELETE")).To(BeFalse())
			Expect(table(ctx, r, "gateway_recipient_keys", "SELECT")).To(BeTrue())
			Expect(table(ctx, r, "gateway_recipient_keys", "INSERT")).To(BeFalse())
		})

		It("leaves a role that no grant names, as koinon-api's login is, with no write on the keys", func(ctx SpecContext) {
			Expect(pgtest.EnsureRole(ctx, db, "koinon_probe_nogrant")).To(Succeed())
			for _, tbl := range []string{"gateway_recipient_keys", "gateway_recipient_key_loads"} {
				for _, p := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
					Expect(table(ctx, "koinon_probe_nogrant", tbl, p)).To(BeFalse(), tbl+" "+p)
				}
			}
			Expect(priv(ctx, "koinon_probe_nogrant", `SELECT has_function_privilege($1, 'gateway_recipient_key_in_use(text)', 'EXECUTE')`)).To(BeFalse(),
				"EXECUTE is revoked from PUBLIC")
		})

		It("runs again without an error and skips a role that does not exist", func(ctx SpecContext) {
			_, err := migrations.GrantGatewayKeyRoles(ctx, db)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
