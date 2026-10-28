// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"database/sql"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/modelformat"
)

var _ = Describe("the model formats migration (ADR 0069)", func() {
	It("backfills anthropic for the models of anthropic providers and keeps the rest on openai", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_model_formats.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(1))
		version, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
		Expect(err).NotTo(HaveOccurred())

		admin, err := sql.Open("pgx", dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(admin.Close)
		_, err = admin.ExecContext(ctx, `CREATE DATABASE formats_before`)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) {
			_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS formats_before WITH (FORCE)`)
		})
		old, err := sql.Open("pgx", strings.Replace(dsn, "/koinon?", "/formats_before?", 1))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(old.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, old, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		var org string
		Expect(old.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('before', 'Before', 'personal', now()) RETURNING id`).Scan(&org)).To(Succeed())
		addModel := func(providerName, typ, model string) {
			GinkgoHelper()
			var pid string
			err := old.QueryRowContext(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, $3, 'https://api.example.com/v1') RETURNING id`, org, providerName, typ).Scan(&pid)
			Expect(err).NotTo(HaveOccurred())
			_, err = old.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, org, pid, model)
			Expect(err).NotTo(HaveOccurred())
		}
		addModel("direct", "anthropic", "claude-haiku-4-5")
		addModel("router", "openrouter", "anthropic/claude-haiku-5.5")
		addModel("oa", "openai", "gpt-5")

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		formats := map[string]string{}
		rows, err := old.QueryContext(ctx, `SELECT name, format || ':' || format_overridden::text FROM models`)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(rows.Close)
		for rows.Next() {
			var n, f string
			Expect(rows.Scan(&n, &f)).To(Succeed())
			formats[n] = f
		}
		Expect(formats).To(Equal(map[string]string{
			"claude-haiku-4-5":           "anthropic:false",
			"anthropic/claude-haiku-5.5": "openai:false",
			"gpt-5":                      "openai:false",
		}))

		By("the CHECK lists exactly the formats the table knows")
		var def string
		Expect(old.QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'models_format_check'`).Scan(&def)).To(Succeed())
		var inCheck []string
		for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(def, -1) {
			inCheck = append(inCheck, m[1])
		}
		var known []string
		for _, f := range modelformat.All() {
			known = append(known, string(f))
		}
		Expect(inCheck).To(ConsistOf(known))

		By("the CHECKs refuse an unknown format and a workspace ID on another type")
		_, err = old.ExecContext(ctx, `UPDATE models SET format = 'gemini'`)
		Expect(err).To(MatchError(ContainSubstring("models_format_check")))
		_, err = old.ExecContext(ctx, `UPDATE providers SET anthropic_workspace_id = 'wrkspc_01' WHERE type = 'openai'`)
		Expect(err).To(MatchError(ContainSubstring("providers_anthropic_workspace_id_type")))
		_, err = old.ExecContext(ctx, `UPDATE providers SET anthropic_workspace_id = 'bad id' WHERE type = 'anthropic'`)
		Expect(err).To(MatchError(ContainSubstring("providers_anthropic_workspace_id_format")))
		_, err = old.ExecContext(ctx, `UPDATE providers SET anthropic_workspace_id = 'wrkspc_01Ab' WHERE type = 'anthropic'`)
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))
})
