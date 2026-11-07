// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var (
	db *pgxpool.Pool
	// adminOrg is the personal org of the one platform admin: where platform-wide audit events go.
	adminOrg pgtype.UUID
)

func TestPrices(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "prices")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	if err := testenv.Docker.Err(); err != nil {
		GinkgoWriter.Println("Docker is not available; Postgres specs are skipped:", err)
		return
	}
	db = pgtest.Start(ctx)
	var user pgtype.UUID
	Expect(pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind) VALUES ('prices-admin', 'Prices', 'personal') RETURNING id`).Scan(&adminOrg); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', 'prices-admin', $1) RETURNING id`, adminOrg).Scan(&user); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, adminOrg, user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, user)
		return err
	})).To(Succeed())
}, NodeTimeout(3*time.Minute))

func needDocker() { testenv.Require(testenv.Docker) }

var seq atomic.Int64

// uniq returns a model name no other spec uses: the platform tables are shared by all specs.
func uniq() string { return fmt.Sprintf("fake/m-%d", seq.Add(1)) }

// auditCount counts the admin org's audit events of an action.
func auditCount(ctx context.Context, action string) int {
	var n int
	Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2`, adminOrg, action).Scan(&n)).To(Succeed())
	return n
}
