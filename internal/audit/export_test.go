// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func exportLines(ctx context.Context, sc audit.ExportScope, from int64) []string {
	GinkgoHelper()
	var buf bytes.Buffer
	Expect(audit.Export(ctx, store.New(pool), &buf, sc, from)).To(Succeed())
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
}

func verifyLines(lines []string, opt audit.ExportVerifyOptions) audit.ExportResult {
	GinkgoHelper()
	res, err := audit.VerifyExport(strings.NewReader(strings.Join(lines, "\n")+"\n"), opt)
	Expect(err).NotTo(HaveOccurred())
	return res
}

// editLine rewrites the JSON object on a line.
func editLine(line string, edit func(m map[string]any)) string {
	GinkgoHelper()
	var m map[string]any
	Expect(json.Unmarshal([]byte(line), &m)).To(Succeed())
	edit(m)
	b, err := json.Marshal(m)
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

// remove returns lines without index i; header is line 0, so event n is line n.
func remove(lines []string, i int) []string {
	return append(append([]string{}, lines[:i]...), lines[i+1:]...)
}

var _ = Describe("audit export and off-cluster verify", func() {
	var org pgtype.UUID
	var sc audit.ExportScope

	BeforeEach(func(ctx SpecContext) {
		org, _, _ = pgtest.NewOrg(ctx, pool)
		sc = audit.ExportScope{Org: org}
		// Three rows per query: 7 events are read in three pages.
		DeferCleanup(audit.SetExportPage(3))
		for i := range 7 {
			record(ctx, org, fmt.Sprintf("test.n%d", i+1))
		}
	})

	It("round-trips an org chain: write, export in several pages, verify the file", func(ctx SpecContext) {
		lines := exportLines(ctx, sc, 1)
		Expect(lines).To(HaveLen(1 + 7))
		var hdr audit.ExportHeader
		Expect(json.Unmarshal([]byte(lines[0]), &hdr)).To(Succeed())
		Expect(hdr.Format).To(Equal("koinon.audit.export.v1"))
		Expect(hdr.Chain).To(Equal("org"))
		Expect(hdr.OrgID).To(Equal(org.String()))
		Expect(hdr.Head.ChainSeq).To(BeEquivalentTo(7))
		Expect(hdr.InstallID).NotTo(BeEmpty())

		res := verifyLines(lines, audit.ExportVerifyOptions{})
		Expect(res.Break).To(BeNil())
		Expect(res.Rows).To(BeEquivalentTo(7))
		var out bytes.Buffer
		Expect(audit.WriteExportReport(&out, res)).To(BeTrue())
		Expect(out.String()).To(ContainSubstring("7 events"))

		By("every hashed field is on the line, and the summary is both text and object")
		var ev audit.ExportEvent
		Expect(json.Unmarshal([]byte(lines[1]), &ev)).To(Succeed())
		Expect(ev.ChainSeq).To(BeEquivalentTo(1))
		Expect(ev.PrevHash).To(Equal(hex.EncodeToString(audit.ZeroHash)))
		Expect(ev.SummaryText).To(ContainSubstring("test.n1"))
		Expect(string(ev.Summary)).To(ContainSubstring("test.n1"))
		Expect(ev.CreatedAtMicros).To(BeNumerically(">", 0))
	})

	It("fails cleanly, and does not loop, when tail rows are gone and a newer event took their place", func(ctx SpecContext) {
		// All inside one transaction that is rolled back: other specs verify every org's chain.
		tx, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
		_, err = tx.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.Exec(ctx, `DELETE FROM audit_events WHERE org_id = $1 AND chain_seq >= 6`, org)
		Expect(err).NotTo(HaveOccurred())
		// chain_seq 8: the head says 8, rows 6 and 7 are missing.
		_, err = audit.WriteInTx(ctx, tx, func(b audit.Batch) {
			b.Org(audit.Event{Org: org, Actor: audit.System("koinon-test:spec"), Action: "test.n8", TargetKind: "test", TargetID: "t"})
		})
		Expect(err).NotTo(HaveOccurred())
		var buf bytes.Buffer
		err = audit.Export(ctx, store.New(tx), &buf, sc, 1)
		Expect(err).To(MatchError(ContainSubstring("rows are missing")))
	}, SpecTimeout(30*time.Second))

	It("round-trips the install chain, and an empty org chain", func(ctx SpecContext) {
		_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
			b.Install(audit.InstallEvent{Actor: audit.System("koinon-test:spec"), Action: "platform_price.set", TargetKind: "platform_price", TargetID: "export"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		lines := exportLines(ctx, audit.ExportScope{Install: true}, 0)
		res := verifyLines(lines, audit.ExportVerifyOptions{})
		Expect(res.Break).To(BeNil())
		Expect(res.Header.Chain).To(Equal("install"))
		Expect(res.Header.OrgID).To(BeEmpty())
		Expect(res.Rows).To(BeNumerically(">=", 1))

		empty, _, _ := pgtest.NewOrg(ctx, pool)
		res = verifyLines(exportLines(ctx, audit.ExportScope{Org: empty}, 1), audit.ExportVerifyOptions{})
		Expect(res.Break).To(BeNil())
		Expect(res.Rows).To(BeZero())
	})

	It("refuses a from-seq beyond the chain before it writes a byte, and accepts head+1 as an empty tail", func(ctx SpecContext) {
		var buf bytes.Buffer
		err := audit.Export(ctx, store.New(pool), &buf, sc, 9)
		Expect(err).To(MatchError(audit.ErrBadFilter))
		Expect(buf.Len()).To(BeZero())

		lines := exportLines(ctx, sc, 8)
		Expect(lines).To(HaveLen(1))
	})

	It("refuses what is not an export", func() {
		for _, in := range []string{"", "not json\n", `{"format":"other.v9"}` + "\n", `{"format":"koinon.audit.export.v1","chain":"org"}` + "\n"} {
			_, err := audit.VerifyExport(strings.NewReader(in), audit.ExportVerifyOptions{})
			Expect(err).To(MatchError(audit.ErrExportFormat), in)
		}
	})

	Describe("a tampered file fails with the first broken chain_seq", func() {
		var lines []string
		BeforeEach(func(ctx SpecContext) { lines = exportLines(ctx, sc, 1) })

		brokenAt := func(l []string, seq int64) {
			GinkgoHelper()
			res := verifyLines(l, audit.ExportVerifyOptions{})
			Expect(res.Break).NotTo(BeNil())
			Expect(res.Break.Seq).To(Equal(seq), res.Break.Reason)
			var out bytes.Buffer
			Expect(audit.WriteExportReport(&out, res)).To(BeFalse())
			Expect(out.String()).To(ContainSubstring(fmt.Sprintf("chain_seq %d", seq)))
		}

		It("a changed field", func() {
			for _, field := range []string{"actor_id", "action", "target_id", "request_id", "created_at_micros", "id"} {
				l := append([]string{}, lines...)
				l[3] = editLine(l[3], func(m map[string]any) {
					if field == "created_at_micros" {
						m[field] = 1.0
						return
					}
					m[field] = "koinon_test_not-a-real-value"
				})
				brokenAt(l, 3)
			}
		})
		It("a changed summary_text, and a summary that no longer matches its text", func() {
			l := append([]string{}, lines...)
			l[2] = editLine(l[2], func(m map[string]any) { m["summary_text"] = strings.Replace(m["summary_text"].(string), "n2", "n9", 1) })
			brokenAt(l, 2)
			l = append([]string{}, lines...)
			l[5] = editLine(l[5], func(m map[string]any) { m["summary"] = map[string]any{"after": map[string]any{"n": "forged"}} })
			brokenAt(l, 5)
		})
		It("a changed hash or prev_hash", func() {
			zero := strings.Repeat("0", 64)
			l := append([]string{}, lines...)
			l[4] = editLine(l[4], func(m map[string]any) { m["hash"] = zero })
			brokenAt(l, 4)
			l = append([]string{}, lines...)
			l[4] = editLine(l[4], func(m map[string]any) { m["prev_hash"] = zero })
			brokenAt(l, 4)
			l = append([]string{}, lines...)
			l[4] = editLine(l[4], func(m map[string]any) { m["hash"] = "zz" })
			brokenAt(l, 4)
		})
		It("a changed first prev_hash in a whole file: it must be 32 zero bytes", func() {
			l := append([]string{}, lines...)
			l[1] = editLine(l[1], func(m map[string]any) { m["prev_hash"] = strings.Repeat("1", 64) })
			brokenAt(l, 1)
		})
		It("a summary with a key twice, at any depth, even when the last value is the true one", func() {
			// encoding/json keeps the last of two keys, so each forged line below still decodes to its
			// summary_text; another reader may keep the first value.
			for _, edit := range [][2]string{
				{`"summary":{"after":`, `"summary":{"after":{"n":"forged"},"after":`},
				{`{"n":"test.n2"}`, `{"n":"forged","n":"test.n2"}`},
				{`{"n":"test.n2"}`, `{"n":"forged","\u006e":"test.n2"}`},
			} {
				Expect(lines[2]).To(ContainSubstring(edit[0]))
				l := append([]string{}, lines...)
				l[2] = strings.Replace(l[2], edit[0], edit[1], 1)
				res := verifyLines(l, audit.ExportVerifyOptions{})
				Expect(res.Break).NotTo(BeNil(), edit[1])
				Expect(res.Break.Seq).To(BeEquivalentTo(2))
				Expect(res.Break.Reason).To(ContainSubstring("twice"))
			}
		})
		It("a removed line", func() { brokenAt(remove(lines, 3), 4) })
		It("swapped lines", func() {
			l := append([]string{}, lines...)
			l[3], l[4] = l[4], l[3]
			brokenAt(l, 4)
		})
		It("a truncated tail, against the header's head", func() {
			brokenAt(lines[:len(lines)-1], 7)
			brokenAt(lines[:len(lines)-3], 5)
		})
		It("a line that is not an event", func() {
			l := append([]string{}, lines...)
			l[3] = "{not json"
			brokenAt(l, 3)
		})
		It("a whole chain at another scope", func() {
			l := append([]string{}, lines...)
			l[0] = editLine(l[0], func(m map[string]any) { m["org_id"] = "00000000-0000-0000-0000-000000000001" })
			brokenAt(l, 1)
		})
	})

	It("reports the same first broken chain_seq as the database verifier, for a chain tampered in the database", func(ctx SpecContext) {
		asOwner(ctx, func(tx pgx.Tx) {
			for _, q := range []string{
				`UPDATE audit_events SET actor_id = 'mallory' WHERE org_id = $1 AND chain_seq = 6`,
				`UPDATE audit_events SET summary = '{"after": {"n": "forged"}}' WHERE org_id = $1 AND chain_seq = 4`,
			} {
				_, err := tx.Exec(ctx, q, org)
				Expect(err).NotTo(HaveOccurred())
			}
		})
		db, err := audit.Verify(ctx, store.New(pool), org)
		Expect(err).NotTo(HaveOccurred())
		Expect(db).To(HaveLen(1))
		Expect(db[0].Break).NotTo(BeNil())
		Expect(db[0].Break.Seq).To(BeEquivalentTo(4))

		file := verifyLines(exportLines(ctx, sc, 1), audit.ExportVerifyOptions{})
		Expect(file.Break).NotTo(BeNil())
		Expect(file.Break.Seq).To(Equal(db[0].Break.Seq), file.Break.Reason)
	})

	It("round-trips a summary with non-ASCII text, HTML characters, escapes and large numbers", func(ctx SpecContext) {
		odd, _, _ := pgtest.NewOrg(ctx, pool)
		_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{
				Org: odd, Actor: audit.System("koinon-test:spec"), Action: "test.odd", TargetKind: "test", TargetID: "Zürich <&> 東京",
				After: map[string]any{
					"text":    "héllo 世界 🎉 \u2028\u2029 <script>&amp;</script>",
					"escapes": "quote \" backslash \\ newline \n tab \t nul-free \u0001 slash /",
					"big":     json.Number("123456789012345678901234567890"),
					"float":   json.Number("1.5e300"),
					"max":     uint64(18446744073709551615),
					"nested":  []any{map[string]any{"n": 1}, map[string]any{"n": 2}},
					"twins":   map[string]any{"a": map[string]any{"n": "x"}, "b": map[string]any{"n": "y"}},
				},
			})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		lines := exportLines(ctx, audit.ExportScope{Org: odd}, 1)
		Expect(lines).To(HaveLen(2))
		res := verifyLines(lines, audit.ExportVerifyOptions{})
		Expect(res.Break).To(BeNil(), "%+v", res.Break)
		Expect(res.Rows).To(BeEquivalentTo(1))
		var ev audit.ExportEvent
		Expect(json.Unmarshal([]byte(lines[1]), &ev)).To(Succeed())
		Expect(ev.SummaryText).To(ContainSubstring("世界 🎉"))
		Expect(ev.SummaryText).To(ContainSubstring("123456789012345678901234567890"))
		Expect(ev.TargetID).To(Equal("Zürich <&> 東京"))
	})

	Describe("a partial export", func() {
		It("is checked against the end of an earlier export", func(ctx SpecContext) {
			earlier := exportLines(ctx, sc, 1)
			for i := 8; i <= 10; i++ {
				record(ctx, org, fmt.Sprintf("test.n%d", i))
			}
			partial := exportLines(ctx, sc, 8)
			Expect(partial).To(HaveLen(1 + 3))

			By("alone: its own links hold, and the result says the first prev_hash was not checked")
			res := verifyLines(partial, audit.ExportVerifyOptions{})
			Expect(res.Break).To(BeNil())
			Expect(res.Partial).To(BeTrue())
			Expect(res.PrevChecked).To(BeFalse())
			var out bytes.Buffer
			Expect(audit.WriteExportReport(&out, res)).To(BeTrue())
			Expect(out.String()).To(ContainSubstring("note"))

			By("after the earlier export: checked")
			prev := verifyLines(earlier, audit.ExportVerifyOptions{})
			res = verifyLines(partial, audit.ExportVerifyOptions{After: &prev.End})
			Expect(res.Break).To(BeNil())
			Expect(res.PrevChecked).To(BeTrue())
			Expect(res.Rows).To(BeEquivalentTo(3))

			By("a first prev_hash that is not the earlier export's end breaks at the first line")
			l := append([]string{}, partial...)
			l[1] = editLine(l[1], func(m map[string]any) { m["prev_hash"] = strings.Repeat("1", 64) })
			Expect(verifyLines(l, audit.ExportVerifyOptions{After: &prev.End}).Break.Seq).To(BeEquivalentTo(8))

			By("an earlier export that ends elsewhere, or of another chain, is an error, not a pass")
			wrong := prev.End
			wrong.Seq = 5
			_, err := audit.VerifyExport(strings.NewReader(strings.Join(partial, "\n")), audit.ExportVerifyOptions{After: &wrong})
			Expect(err).To(MatchError(ContainSubstring("ends at chain_seq 5")))
			wrong = prev.End
			wrong.OrgID = "00000000-0000-0000-0000-000000000001"
			_, err = audit.VerifyExport(strings.NewReader(strings.Join(partial, "\n")), audit.ExportVerifyOptions{After: &wrong})
			Expect(err).To(MatchError(ContainSubstring("another chain")))
			_, err = audit.VerifyExport(strings.NewReader(strings.Join(earlier, "\n")), audit.ExportVerifyOptions{After: &prev.End})
			Expect(err).To(MatchError(ContainSubstring("partial export")))
		})
	})

	It("verifies a file longer than a page without holding it: 2500 lines, one event at a time", func() {
		// A synthetic install chain, hashed with the same function the database chain is checked with.
		const install = "00000000-0000-4000-8000-0000000000aa"
		var b strings.Builder
		prev := audit.ZeroHash
		var events []string
		for seq := int64(1); seq <= 2500; seq++ {
			row := store.ListInstallAuditChainRowsRow{
				ScopeText: install, IDText: fmt.Sprintf("00000000-0000-4000-8000-%012d", seq), ChainSeq: seq,
				ActorKind: "system", ActorID: "koinon-test:spec", Action: "test.synthetic", TargetKind: "test", TargetID: "t",
				SummaryText: `{"n": 1}`, CreatedMicros: 1700000000000000 + seq, PrevHash: prev,
			}
			row.Hash = audit.InstallRowHash(row)
			prev = row.Hash
			ev := audit.ExportEvent{
				ChainSeq: seq, ID: row.IDText, ActorKind: row.ActorKind, ActorID: row.ActorID, Action: row.Action,
				TargetKind: row.TargetKind, TargetID: row.TargetID, SummaryText: row.SummaryText, Summary: json.RawMessage(row.SummaryText),
				CreatedAtMicros: row.CreatedMicros, PrevHash: hex.EncodeToString(row.PrevHash), Hash: hex.EncodeToString(row.Hash),
			}
			j, err := json.Marshal(ev)
			Expect(err).NotTo(HaveOccurred())
			events = append(events, string(j))
		}
		hdr, err := json.Marshal(audit.ExportHeader{
			Format: audit.ExportFormat, Chain: "install", InstallID: install, FromSeq: 1,
			Head: audit.ExportHead{ChainSeq: 2500, Hash: hex.EncodeToString(prev)},
		})
		Expect(err).NotTo(HaveOccurred())
		b.WriteString(string(hdr) + "\n" + strings.Join(events, "\n") + "\n")
		res, err := audit.VerifyExport(strings.NewReader(b.String()), audit.ExportVerifyOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Break).To(BeNil())
		Expect(res.Rows).To(BeEquivalentTo(2500))

		By("a change past the first page is found at its chain_seq")
		events[1700] = strings.Replace(events[1700], "test.synthetic", "test.forged", 1)
		res, err = audit.VerifyExport(strings.NewReader(string(hdr)+"\n"+strings.Join(events, "\n")+"\n"), audit.ExportVerifyOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Break).NotTo(BeNil())
		Expect(res.Break.Seq).To(BeEquivalentTo(1701))
	})
})
