// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the template harness block migration (ADR 0112)", func() {
	It("moves spec.flavors into spec.harness and keeps the harness settings", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_template_harness_block.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(1))
		version, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
		Expect(err).NotTo(HaveOccurred())

		old, err := sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(old.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, old, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		add := func(slug, spec string) {
			GinkgoHelper()
			sum := sha256.Sum256([]byte(slug))
			_, err := old.ExecContext(ctx,
				`INSERT INTO templates (slug, content_hash, display_name, description, spec) VALUES ($1, $2, $1, '', $3::jsonb)`,
				slug, hex.EncodeToString(sum[:]), spec)
			Expect(err).NotTo(HaveOccurred())
		}
		add("with-settings", `{"kind":"Template","spec":{"flavors":{"allowed":["opencode"],"default":"opencode"},"harness":{"pluginInstalls":true},"model":{"name":"default"}}}`)
		add("without-settings", `{"kind":"Template","spec":{"flavors":{"allowed":["opencode"],"default":"opencode"},"model":{"name":"default"}}}`)
		add("already-new", `{"kind":"Template","spec":{"harness":{"allowed":["opencode"],"default":"opencode"},"model":{"name":"default"}}}`)

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		spec := func(slug string) string {
			GinkgoHelper()
			var s string
			Expect(old.QueryRowContext(ctx, `SELECT spec::text FROM templates WHERE slug = $1`, slug).Scan(&s)).To(Succeed())
			return s
		}
		Expect(spec("with-settings")).To(MatchJSON(`{"kind":"Template","spec":{"harness":{"allowed":["opencode"],"default":"opencode","pluginInstalls":true},"model":{"name":"default"}}}`))
		Expect(spec("without-settings")).To(MatchJSON(`{"kind":"Template","spec":{"harness":{"allowed":["opencode"],"default":"opencode"},"model":{"name":"default"}}}`))
		Expect(spec("already-new")).To(MatchJSON(`{"kind":"Template","spec":{"harness":{"allowed":["opencode"],"default":"opencode"},"model":{"name":"default"}}}`))
	})
})
