// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Home backup retention", func() {
	var svc *orgsettings.Service
	BeforeEach(func() { svc = orgsettings.NewService(pool) })

	It("defaults to checkpoint's retain, 7 days", func() {
		Expect(orgsettings.DefaultHomeBackupRetention).To(Equal(checkpoint.DefaultRetain))
		Expect(orgsettings.DefaultHomeBackupRetentionDays).To(Equal(7))
	})

	It("lets an org admin change it, audited only when it changes, and leaves the other settings", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		days, err := svc.HomeBackupRetentionDays(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(days).To(Equal(7))

		forty := 40
		Expect(svc.SetRunContentRetention(ctx, org, audit.System("koinon-test:spec"), &forty)).To(Succeed())
		thirty := 30
		Expect(svc.SetHomeBackupRetention(ctx, org, audit.System("koinon-test:spec"), &thirty)).To(Succeed())
		Expect(svc.SetHomeBackupRetention(ctx, org, audit.System("koinon-test:spec"), &thirty)).To(Succeed(), "unchanged: no audit entry")
		d, err := svc.HomeBackupRetention(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(30 * 24 * time.Hour))
		run, _ := svc.RunContentRetentionDays(ctx, org)
		Expect(run).To(Equal(40), "the run content retention stays")

		Expect(svc.SetHomeBackupRetention(ctx, org, audit.System("koinon-test:spec"), nil)).To(Succeed())
		days, _ = svc.HomeBackupRetentionDays(ctx, org)
		Expect(days).To(Equal(7))
		run, _ = svc.RunContentRetentionDays(ctx, org)
		Expect(run).To(Equal(40))

		rows, err := pool.Query(ctx, `SELECT summary->'before'->>'days', summary->'after'->>'days' FROM audit_events
			WHERE org_id = $1 AND action = 'org.home_backup_retention.set' ORDER BY created_at`, org)
		Expect(err).NotTo(HaveOccurred())
		var changes [][2]string
		for rows.Next() {
			var b, a string
			Expect(rows.Scan(&b, &a)).To(Succeed())
			changes = append(changes, [2]string{b, a})
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(changes).To(Equal([][2]string{{"7", "30"}, {"30", "7"}}))
	})

	It("rejects a retention outside 1 to 90 days, here and in the table", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		for _, d := range []int{0, -1, 91} {
			err := svc.SetHomeBackupRetention(ctx, org, audit.System("koinon-test:spec"), &d)
			var inv *orgsettings.InvalidError
			Expect(err).To(BeAssignableToTypeOf(inv), "%d days", d)
		}
		for _, d := range []int{1, 90} {
			Expect(svc.SetHomeBackupRetention(ctx, org, audit.System("koinon-test:spec"), &d)).To(Succeed(), "%d days", d)
		}
		_, err := pool.Exec(ctx, `UPDATE org_settings SET home_backup_retention_days = 91 WHERE org_id = $1`, org)
		Expect(err).To(MatchError(ContainSubstring("home_backup_retention_days")))
	})

	It("is the deadline a new repository epoch gives the retired one", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		var agent pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, flavor) VALUES ($1, 'homes', 'opencode') RETURNING id`, org).Scan(&agent)).To(Succeed())
		three := 3
		Expect(svc.SetHomeBackupRetention(ctx, org, audit.System("koinon-test:spec"), &three)).To(Succeed())

		pw := checkpoint.Passwords{Q: store.New(pool), Keys: secretstoretest.Keyring(), Begin: pool.Begin}
		_, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		retain, err := svc.HomeBackupRetention(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		_, ok, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, retain)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		var deleteAfter pgtype.Timestamptz
		Expect(pool.QueryRow(ctx, `SELECT delete_after FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, org, agent).Scan(&deleteAfter)).To(Succeed())
		Expect(deleteAfter.Time).To(BeTemporally("~", time.Now().Add(72*time.Hour), time.Minute))
	})
})
