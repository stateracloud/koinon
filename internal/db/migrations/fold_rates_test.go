// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("00014_fold_model_rates_into_prices", func() {
	// The suite migrates an empty database, so the conversion is run again here on rows that
	// carry rates: the DO block of the migration, as written.
	runConversion := func(ctx SpecContext) {
		GinkgoHelper()
		raw, err := migrations.FS.ReadFile("00014_fold_model_rates_into_prices.sql")
		Expect(err).NotTo(HaveOccurred())
		s := string(raw)
		start := strings.Index(s, "DO $$")
		end := strings.Index(s, "$$;") + len("$$;")
		_, err = db.ExecContext(ctx, s[start:end])
		Expect(err).NotTo(HaveOccurred())
	}

	It("turns a model's rates into a dated org price that resolves, and skips rates that cannot be priced", func(ctx SpecContext) {
		_, org := newUser(ctx)
		var prov string
		Expect(db.QueryRowContext(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'p', 'openrouter', 'https://x/v1') RETURNING id`, org).Scan(&prov)).To(Succeed())
		_, err := db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name, input_rate, output_rate, cache_read_rate, created_at) VALUES ($1, $2, 'deepseek/flash', '0.30', '1.20', '0.03', '2026-10-01T12:00:00Z')`, org, prov)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name, input_rate, output_rate) VALUES ($1, $2, 'free', '0', '0')`, org, prov)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name, input_rate, output_rate) VALUES ($1, $2, 'too-precise', '1.1234567', '1')`, org, prov)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, 'unpriced')`, org, prov)
		Expect(err).NotTo(HaveOccurred())

		runConversion(ctx)

		rows, err := db.QueryContext(ctx, `SELECT name, entry, valid_from::text, created_by FROM price_overrides WHERE org_id = $1 ORDER BY name`, org)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = rows.Close() }()
		type row struct {
			name, from, by string
			entry          pricing.Entry
		}
		var got []row
		for rows.Next() {
			var r row
			var raw []byte
			Expect(rows.Scan(&r.name, &raw, &r.from, &r.by)).To(Succeed())
			Expect(json.Unmarshal(raw, &r.entry)).To(Succeed())
			got = append(got, r)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(2), "the unpriced model and the one with an unusable rate are skipped")

		for i := range got {
			got[i].entry.ID = got[i].name
			_, err := got[i].entry.Resolve(true)
			Expect(err).NotTo(HaveOccurred(), "the migrated entry validates like one set through the API")
		}
		Expect(got[0].name).To(MatchRegexp(`^model-deepseek-flash-[0-9a-f]{8}$`))
		Expect(got[0].entry.Provider).To(Equal("openrouter"))
		Expect(got[0].entry.Match.Glob).To(Equal("deepseek/flash"))
		Expect(got[0].entry.Prices).To(Equal(pricing.Wire{Input: "0.30", Output: "1.20", CacheRead: "0.03"}))
		Expect(got[0].entry.MaxOutput).To(Equal(131072))
		Expect(got[0].from).To(HavePrefix("2026-10-01 12:00:00"))
		Expect(got[0].by).To(Equal("migration 00014"))
		Expect(got[1].entry.MaxOutput).To(BeZero(), "a free model needs no max_output")
	})
})
