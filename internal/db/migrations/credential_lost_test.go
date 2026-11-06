// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The lost-credential state of ADR 0085 decision 13 (#85): the database keeps its invariants for
// every writer, an N-1 binary's included.
var _ = Describe("lost credentials", func() {
	// An envelope's shape does not matter to the database: jsonb, an object.
	const envelope = `{"v":1,"note":"koinon_test_not-a-real-envelope"}`

	type table struct {
		name, envelope, other string
		insert                func(org string) string // returns the row's WHERE clause
	}
	var org string
	scan := func(query string, args ...any) string {
		GinkgoHelper()
		var s sql.NullString
		Expect(db.QueryRowContext(context.Background(), query, args...).Scan(&s)).To(Succeed(), query)
		return s.String
	}
	exec := func(query string, args ...any) error {
		_, err := db.ExecContext(context.Background(), query, args...)
		return err
	}
	checkViolation := func(err error, constraint string) {
		GinkgoHelper()
		var pg *pgconn.PgError
		Expect(errors.As(err, &pg)).To(BeTrue(), "%v", err)
		Expect(pg.Code).To(Equal("23514"))
		Expect(pg.ConstraintName).To(Equal(constraint))
	}

	BeforeEach(func() {
		// (A deleted org: the manual-owner rule is checked for live orgs only.)
		org = scan(`INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('lost-' || substr(md5(random()::text), 1, 8), 'Lost', 'personal', now()) RETURNING id`)
	})

	tables := []table{
		{name: "providers", envelope: "api_key", other: "url = url || '/x'", insert: func(org string) string {
			id := scan(`INSERT INTO providers (org_id, name, type, url, api_key) VALUES ($1, 'p', 'openrouter', 'https://example.com/v1', $2) RETURNING id`, org, envelope)
			return fmt.Sprintf("id = '%s'", id)
		}},
		{name: "connections", envelope: "secret_envelope", other: "display_name = display_name || 'x'", insert: func(org string) string {
			id := scan(`INSERT INTO connections (org_id, connector, display_name, secret_name, secret_envelope) VALUES ($1, 'github', 'GitHub', $3, $2) RETURNING id`, org, envelope, "conn-"+org)
			return fmt.Sprintf("id = '%s'", id)
		}},
		{name: "mcp_servers", envelope: "credential", other: "url = url || 'x'", insert: func(org string) string {
			id := scan(`INSERT INTO mcp_servers (org_id, name, url, credential_header, credential) VALUES ($1, 'm', 'https://mcp.example.com/m', 'X-Api-Key', $2) RETURNING id`, org, envelope)
			return fmt.Sprintf("id = '%s'", id)
		}},
		{name: "trace_destinations", envelope: "credentials", other: "endpoint_url = endpoint_url || '/x'", insert: func(org string) string {
			id := scan(`INSERT INTO trace_destinations (org_id, name, backend, endpoint_url, credentials) VALUES ($1, 'dd', 'datadog', 'https://otlp.example.com', $2) RETURNING id`, org, envelope)
			return fmt.Sprintf("id = '%s'", id)
		}},
	}

	for _, t := range tables {
		Context("on "+t.name, func() {
			var where string
			lost := func() bool {
				GinkgoHelper()
				return scan(fmt.Sprintf(`SELECT (credential_lost_at IS NOT NULL)::text FROM %s WHERE %s`, t.name, where)) == "true"
			}
			BeforeEach(func() { where = t.insert(org) })

			It("refuses a lost row that still holds an envelope", func() {
				checkViolation(exec(fmt.Sprintf(`UPDATE %s SET credential_lost_at = now() WHERE %s`, t.name, where)), t.name+"_credential_lost")
			})

			It("keeps the mark through other updates and clears it when an envelope is written again", func() {
				Expect(exec(fmt.Sprintf(`UPDATE %s SET %s = NULL, credential_lost_at = now() WHERE %s`, t.name, t.envelope, where))).To(Succeed())
				Expect(lost()).To(BeTrue())
				Expect(exec(fmt.Sprintf(`UPDATE %s SET %s WHERE %s`, t.name, t.other, where))).To(Succeed())
				Expect(lost()).To(BeTrue())
				Expect(exec(fmt.Sprintf(`UPDATE %s SET %s = $1 WHERE %s`, t.name, t.envelope, where), envelope)).To(Succeed())
				Expect(lost()).To(BeFalse())
			})
		})
	}

	It("lets a lost MCP credential keep its header, and only a lost one", func() {
		id := scan(`INSERT INTO mcp_servers (org_id, name, url, credential_header, credential) VALUES ($1, 'h', 'https://mcp.example.com/h', 'X-Api-Key', $2) RETURNING id`, org, envelope)
		checkViolation(exec(`UPDATE mcp_servers SET credential = NULL WHERE id = $1`, id), "mcp_servers_credential_header")
		Expect(exec(`UPDATE mcp_servers SET credential = NULL, credential_lost_at = now() WHERE id = $1`, id)).To(Succeed())
		Expect(scan(`SELECT credential_header FROM mcp_servers WHERE id = $1`, id)).To(Equal("X-Api-Key"))
		// Removing the credential on a lost row clears the header and the mark together.
		Expect(exec(`UPDATE mcp_servers SET credential_header = NULL, credential_lost_at = NULL WHERE id = $1`, id)).To(Succeed())
		checkViolation(exec(`INSERT INTO mcp_servers (org_id, name, url, credential) VALUES ($1, 'v', 'https://mcp.example.com/v', $2)`, org, envelope), "mcp_servers_credential_header")
	})

	It("holds the same rules for the platform destination", func(ctx SpecContext) {
		tx, err := db.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		// The table is a singleton other specs may use: everything here is rolled back.
		DeferCleanup(func() { _ = tx.Rollback() })
		run := func(q string, args ...any) error {
			_, err := tx.ExecContext(ctx, "SAVEPOINT s")
			Expect(err).NotTo(HaveOccurred())
			_, err = tx.ExecContext(ctx, q, args...)
			if err != nil {
				_, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT s")
				Expect(rerr).NotTo(HaveOccurred())
			}
			return err
		}
		lost := func() bool {
			var v bool
			Expect(tx.QueryRowContext(ctx, `SELECT credential_lost_at IS NOT NULL FROM platform_trace_destination`).Scan(&v)).To(Succeed())
			return v
		}
		Expect(run(`DELETE FROM platform_trace_destination`)).To(Succeed())
		Expect(run(`INSERT INTO platform_trace_destination (backend, endpoint_url, credentials) VALUES ('datadog', 'https://otlp.example.com', $1)`, envelope)).To(Succeed())
		checkViolation(run(`UPDATE platform_trace_destination SET credential_lost_at = now()`), "platform_trace_destination_credential_lost")
		Expect(run(`UPDATE platform_trace_destination SET credentials = NULL, credential_lost_at = now()`)).To(Succeed())
		Expect(run(`UPDATE platform_trace_destination SET endpoint_url = 'https://otlp2.example.com'`)).To(Succeed())
		Expect(lost()).To(BeTrue())
		Expect(run(`UPDATE platform_trace_destination SET credentials = $1`, envelope)).To(Succeed())
		Expect(lost()).To(BeFalse())
	})
})
