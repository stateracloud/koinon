// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"io/fs"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the home repository epochs migration (ADR 0085 decision 13, #213)", func() {
	It("keeps every existing repository and checkpoint in epoch 0 and guards the new rows", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_home_repository_epochs.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(1))
		version, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
		Expect(err).NotTo(HaveOccurred())

		db, err := sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		var org, agent string
		Expect(db.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('epochs', 'Epochs', 'personal', now()) RETURNING id`).Scan(&org)).To(Succeed())
		Expect(db.QueryRowContext(ctx, `INSERT INTO agents (org_id, display_name, flavor) VALUES ($1, 'a', 'opencode') RETURNING id`, org).Scan(&agent)).To(Succeed())
		prefix := "orgs/" + org + "/homes/" + agent + "/"
		_, err = db.ExecContext(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{"kek_id":"k"}')`, org, agent, prefix)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at) VALUES ($1, $2, 'hourly', 'complete', 's', now())`, org, agent)
		Expect(err).NotTo(HaveOccurred())

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("the existing rows: epoch 0, live, unchanged")
		var repoEpoch int
		Expect(db.QueryRowContext(ctx, `SELECT repository_epoch FROM home_checkpoints WHERE org_id = $1`, org).Scan(&repoEpoch)).To(Succeed())
		Expect(repoEpoch).To(BeZero())
		var env string
		var retired, lost sql.NullTime
		Expect(db.QueryRowContext(ctx, `SELECT password_envelope::text, retired_at, credential_lost_at FROM home_repositories WHERE org_id = $1`, org).
			Scan(&env, &retired, &lost)).To(Succeed())
		Expect(env).To(Equal(`{"kek_id": "k"}`))
		Expect(retired.Valid || lost.Valid).To(BeFalse())

		By("a later epoch is nested under the agent's prefix and bound to its own row")
		_, err = db.ExecContext(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
			VALUES ($1, $2, 1, $3, '{}', 'key_lost')`, org, agent, prefix+"r1/")
		Expect(err).NotTo(HaveOccurred())
		var row string
		Expect(db.QueryRowContext(ctx, `SELECT binding_row FROM home_repository_epochs WHERE org_id = $1`, org).Scan(&row)).To(Succeed())
		Expect(row).To(Equal(agent + "/r1"))

		bad := map[string]string{
			"epoch 0 is home_repositories'": `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason) VALUES ($1, $2, 0, $3, '{}', 'key_lost')`,
			"a prefix outside the agent's":  `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason) VALUES ($1, $2, 2, 'orgs/x/', '{}', 'key_lost')`,
			"another epoch's folder":        `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason) VALUES ($1, $2, 3, $3, '{}', 'key_lost')`,
			"an unknown reason":             `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason) VALUES ($1, $2, 2, $3, '{}', 'rotation')`,
			"no envelope without the mark":  `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason) VALUES ($1, $2, 2, $3, NULL, 'key_lost')`,
		}
		for what, stmt := range bad {
			args := []any{org, agent}
			if strings.Contains(stmt, "$3") {
				args = append(args, prefix+"r2/")
			}
			_, err := db.ExecContext(ctx, stmt, args...)
			Expect(err).To(HaveOccurred(), what)
		}
		_, err = db.ExecContext(ctx, `UPDATE home_repositories SET deleted_at = now() WHERE org_id = $1`, org)
		Expect(err).To(HaveOccurred(), "deleted only after a deadline")
		_, err = db.ExecContext(ctx, `UPDATE home_repositories SET password_envelope = NULL WHERE org_id = $1`, org)
		Expect(err).To(HaveOccurred(), "an envelope is cleared only with credential_lost_at")
		_, err = db.ExecContext(ctx, `UPDATE home_repositories SET password_envelope = NULL, credential_lost_at = now() WHERE org_id = $1`, org)
		Expect(err).NotTo(HaveOccurred())
	})
})
