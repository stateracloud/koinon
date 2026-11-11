// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func exportLines(ctx context.Context, sc audit.ExportScope, from int64) []string {
	GinkgoHelper()
	return exportLinesFrom(ctx, store.New(pool), sc, from)
}

// exportLinesFrom exports through q, for a spec that reads the log inside its own transaction.
func exportLinesFrom(ctx context.Context, q *store.Queries, sc audit.ExportScope, from int64) []string {
	GinkgoHelper()
	var buf bytes.Buffer
	Expect(audit.Export(ctx, q, &buf, sc, from)).To(Succeed())
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
		Expect(hdr.Format).To(Equal("koinon.audit.export.v2"))
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
		tx := ownerTx(ctx)
		_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE org_id = $1 AND chain_seq >= 6`, org)
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
		for _, in := range []string{"", "not json\n", `{"format":"other.v9"}` + "\n", `{"format":"koinon.audit.export.v1","chain":"org"}` + "\n", `{"format":"koinon.audit.export.v2","chain":"org"}` + "\n"} {
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
		It("an event line with a top-level key twice, even when the last value is the true one", func() {
			// A reader that keeps the first value sees mallory as the actor of a line that verifies.
			Expect(lines[3]).To(HavePrefix("{"))
			for _, key := range []string{"actor_id", `actor\u005fid`, "summary_text", "hash"} {
				l := append([]string{}, lines...)
				l[3] = `{"` + key + `":"mallory",` + strings.TrimPrefix(l[3], "{")
				res := verifyLines(l, audit.ExportVerifyOptions{})
				Expect(res.Break).NotTo(BeNil(), key)
				Expect(res.Break.Seq).To(BeEquivalentTo(3), key)
				Expect(res.Break.Reason).To(ContainSubstring("twice"), key)
			}

			By("chain_seq twice: broken at the chain_seq due on that line, whatever either value says")
			Expect(lines[3]).To(HaveSuffix("}"))
			for _, forged := range []string{"first 1", "first 99", "last 99"} {
				l := append([]string{}, lines...)
				where, seq, _ := strings.Cut(forged, " ")
				if where == "first" {
					l[3] = `{"chain_seq":` + seq + `,` + strings.TrimPrefix(l[3], "{")
				} else {
					l[3] = strings.TrimSuffix(l[3], "}") + `,"chain_seq":` + seq + `}`
				}
				res := verifyLines(l, audit.ExportVerifyOptions{})
				Expect(res.Break).NotTo(BeNil(), forged)
				Expect(res.Break.Seq).To(BeEquivalentTo(3), forged)
				Expect(res.Break.Reason).To(ContainSubstring(`"chain_seq" twice`), forged)
			}
		})
		It("chain_seq twice plus a bad hash: broken at the due seq, not the forged one", func() {
			l := append([]string{}, lines...)
			l[3] = editLine(l[3], func(m map[string]any) { m["hash"] = "zz" })
			l[3] = `{"chain_seq":99,` + strings.TrimPrefix(l[3], "{")
			res := verifyLines(l, audit.ExportVerifyOptions{})
			Expect(res.Break).NotTo(BeNil())
			Expect(res.Break.Seq).To(BeEquivalentTo(3))
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
		// Tampered, verified and exported inside one transaction that is rolled back: other specs
		// verify every org's chain.
		tx := ownerTx(ctx)
		for _, q := range []string{
			`UPDATE audit_events SET actor_id = 'mallory' WHERE org_id = $1 AND chain_seq = 6`,
			`UPDATE audit_events SET summary = '{"after": {"n": "forged"}}' WHERE org_id = $1 AND chain_seq = 4`,
		} {
			_, err := tx.Exec(ctx, q, org)
			Expect(err).NotTo(HaveOccurred())
		}
		q := store.New(tx)
		db, err := audit.Verify(ctx, q, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(db).To(HaveLen(1))
		Expect(db[0].Break).NotTo(BeNil())
		Expect(db[0].Break.Seq).To(BeEquivalentTo(4))

		file := verifyLines(exportLinesFrom(ctx, q, sc, 1), audit.ExportVerifyOptions{})
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

	It("keeps the header small and verify memory bounded with many anchors", func() {
		// Synthetic install chain: every event is an audit.anchor followed by an anchor_heads line
		// with one org. A v1-style header holding every anchor would grow with n; v2 streams.
		const (
			install = "00000000-0000-4000-8000-0000000000aa"
			org     = "00000000-0000-4000-8000-0000000000bb"
			n       = 4000
		)
		orgHash := bytes.Repeat([]byte{0x11}, 32)
		root := hex.EncodeToString(audit.HeadsRoot(install, []audit.AnchorHead{{OrgID: org, ChainSeq: 1, Hash: orgHash}}))
		summary := fmt.Sprintf(`{"after":{"org_count":1,"heads_root":"%s"}}`, root)
		prev := audit.ZeroHash
		var lastHash []byte
		pairs := make([]struct{ ev, heads []byte }, 0, n)
		for seq := int64(1); seq <= n; seq++ {
			row := store.ListInstallAuditChainRowsRow{
				ScopeText: install, IDText: fmt.Sprintf("00000000-0000-4000-8000-%012d", seq), ChainSeq: seq,
				ActorKind: "system", ActorID: "koinon-api:audit-anchor", Action: audit.ActionAnchor, TargetKind: "install", TargetID: install,
				SummaryText: summary, CreatedMicros: 1700000000000000 + seq, PrevHash: prev,
			}
			row.Hash = audit.InstallRowHash(row)
			prev = row.Hash
			lastHash = row.Hash
			ev, err := json.Marshal(audit.ExportEvent{
				ChainSeq: seq, ID: row.IDText, ActorKind: row.ActorKind, ActorID: row.ActorID, Action: row.Action,
				TargetKind: row.TargetKind, TargetID: row.TargetID, SummaryText: row.SummaryText, Summary: json.RawMessage(row.SummaryText),
				CreatedAtMicros: row.CreatedMicros, PrevHash: hex.EncodeToString(row.PrevHash), Hash: hex.EncodeToString(row.Hash),
			})
			Expect(err).NotTo(HaveOccurred())
			al, err := json.Marshal(audit.ExportAnchorLine{
				Line: audit.ExportAnchorLineKind, ChainSeq: seq, EventID: row.IDText, OrgCount: 1, HeadsRoot: root,
				Heads: []audit.ExportAnchorHead{{OrgID: org, ChainSeq: 1, Hash: hex.EncodeToString(orgHash)}},
			})
			Expect(err).NotTo(HaveOccurred())
			pairs = append(pairs, struct{ ev, heads []byte }{ev: ev, heads: al})
		}
		hdr, err := json.Marshal(audit.ExportHeader{
			Format: audit.ExportFormat, Chain: "install", InstallID: install, FromSeq: 1,
			Head: audit.ExportHead{ChainSeq: n, Hash: hex.EncodeToString(lastHash)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(hdr)).To(BeNumerically("<", 512), "header size must not grow with the anchor count")

		// Stream one line at a time so the input itself is not one giant buffer held by the verifier.
		stream := func() *manyAnchorReader {
			return &manyAnchorReader{hdr: append(append([]byte{}, hdr...), '\n'), pairs: pairs}
		}

		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := audit.VerifyExport(stream(), audit.ExportVerifyOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Break).To(BeNil())
		Expect(res.Anchors).To(Equal(n))
		Expect(res.StreamAnchors).To(BeEmpty())
		runtime.GC()
		runtime.ReadMemStats(&after)
		// Retained heap after a streaming verify must stay well below holding every anchor line
		// (~n * ~300 B ≈ 1.2 MiB of JSON alone, plus structured copies under CollectAnchors).
		Expect(int64(after.HeapAlloc)).To(BeNumerically("<", int64(before.HeapAlloc)+2<<20),
			"heap grew by %d bytes after verify of %d anchors", int64(after.HeapAlloc)-int64(before.HeapAlloc), n)

		By("CollectAnchors keeps every stream line for --anchors")
		kept, err := audit.VerifyExport(stream(), audit.ExportVerifyOptions{CollectAnchors: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(kept.StreamAnchors).To(HaveLen(n))
	})

	It("keeps only the --file org's heads of each anchor when AnchorOrgID is set", func() {
		const orgs, anchors = 60, 80
		lines, orgIDs := syntheticAnchorExport(orgs, anchors)
		stream := strings.Join(lines, "\n") + "\n"

		all, err := audit.VerifyExport(strings.NewReader(stream), audit.ExportVerifyOptions{CollectAnchors: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(all.Break).To(BeNil())
		Expect(all.StreamAnchors).To(HaveLen(anchors))
		for _, a := range all.StreamAnchors {
			Expect(a.Heads).To(HaveLen(orgs), "without a filter every org's head is kept")
		}

		one, err := audit.VerifyExport(strings.NewReader(stream), audit.ExportVerifyOptions{CollectAnchors: true, AnchorOrgID: orgIDs[7]})
		Expect(err).NotTo(HaveOccurred())
		Expect(one.Break).To(BeNil(), "the heads are all checked against heads_root before the others are dropped")
		Expect(one.Anchors).To(Equal(anchors))
		Expect(one.StreamAnchors).To(HaveLen(anchors))
		retained := 0
		for _, a := range one.StreamAnchors {
			Expect(a.OrgCount).To(Equal(orgs), "the anchor still names the org count it commits to")
			Expect(a.Heads).To(HaveLen(1))
			Expect(a.Heads[0].OrgID).To(Equal(orgIDs[7]))
			retained += cap(a.Heads)
		}
		Expect(retained).To(BeNumerically("<=", anchors), "the kept slices are not backed by the full per-anchor head lists")

		By("an org that no anchor holds keeps no head")
		none, err := audit.VerifyExport(strings.NewReader(stream), audit.ExportVerifyOptions{CollectAnchors: true, AnchorOrgID: "00000000-0000-4000-8000-ffffffffffff"})
		Expect(err).NotTo(HaveOccurred())
		Expect(none.Break).To(BeNil())
		for _, a := range none.StreamAnchors {
			Expect(a.Heads).To(BeEmpty())
		}
	})

	It("retains far less heap with AnchorOrgID than without it, for many orgs x anchors", func() {
		const orgs, anchors = 200, 300
		lines, orgIDs := syntheticAnchorExport(orgs, anchors)
		stream := strings.Join(lines, "\n") + "\n"

		// retained is the heap still held after a collecting verify, with its result kept alive.
		retained := func(opt audit.ExportVerifyOptions) int64 {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			res, err := audit.VerifyExport(strings.NewReader(stream), opt)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Break).To(BeNil())
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(res)
			return int64(after.HeapAlloc) - int64(before.HeapAlloc)
		}
		all := retained(audit.ExportVerifyOptions{CollectAnchors: true})
		one := retained(audit.ExportVerifyOptions{CollectAnchors: true, AnchorOrgID: orgIDs[3]})
		// 60,000 kept heads (an org ID and a hash each) are several MiB; one org's 300 heads are tens of KiB.
		Expect(all).To(BeNumerically(">", 1<<20), "without a filter every org's head is retained")
		Expect(one).To(BeNumerically("<", all/10), "with AnchorOrgID the retained heap does not grow with the org count (all=%d one=%d)", all, one)
	})

	It("refuses an anchor_heads line with a key twice, at the top level or inside a head", func() {
		lines, _ := syntheticAnchorExport(3, 2)
		Expect(audit.VerifyExport(strings.NewReader(strings.Join(lines, "\n")+"\n"), audit.ExportVerifyOptions{})).To(
			HaveField("Break", BeNil()), "the unedited export holds")
		idx := -1
		for i, l := range lines {
			if strings.Contains(l, `"line":"`+audit.ExportAnchorLineKind+`"`) {
				idx = i
				break
			}
		}
		Expect(idx).To(BeNumerically(">", 0))
		for name, edit := range map[string]func(string) string{
			"top-level org_count": func(l string) string { return strings.Replace(l, `{`, `{"org_count":3,`, 1) },
			"top-level line":      func(l string) string { return strings.Replace(l, `{`, `{"line":"`+audit.ExportAnchorLineKind+`",`, 1) },
			"inside a head":       func(l string) string { return strings.Replace(l, `"heads":[{`, `"heads":[{"chain_seq":1,`, 1) },
		} {
			edited := append([]string{}, lines...)
			edited[idx] = edit(lines[idx])
			Expect(edited[idx]).NotTo(Equal(lines[idx]), name)
			res := verifyLines(edited, audit.ExportVerifyOptions{})
			Expect(res.Break).NotTo(BeNil(), name)
			Expect(res.Break.Reason).To(ContainSubstring("twice"), name)
		}
	})
})

// syntheticAnchorExport builds a verifying install export with the given number of anchors, each
// followed by an anchor_heads line that holds one head per org. It returns the lines (header first)
// and the org IDs.
func syntheticAnchorExport(orgs, anchors int) ([]string, []string) {
	GinkgoHelper()
	const install = "00000000-0000-4000-8000-0000000000aa"
	orgIDs := make([]string, orgs)
	heads := make([]audit.AnchorHead, orgs)
	exportHeads := make([]audit.ExportAnchorHead, orgs)
	for i := range orgIDs {
		orgIDs[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		h := bytes.Repeat([]byte{byte(i + 1)}, 32)
		heads[i] = audit.AnchorHead{OrgID: orgIDs[i], ChainSeq: 1, Hash: h}
		exportHeads[i] = audit.ExportAnchorHead{OrgID: orgIDs[i], ChainSeq: 1, Hash: hex.EncodeToString(h)}
	}
	root := hex.EncodeToString(audit.HeadsRoot(install, heads))
	summary := fmt.Sprintf(`{"after":{"org_count":%d,"heads_root":"%s"}}`, orgs, root)
	prev := audit.ZeroHash
	var events []string
	for seq := int64(1); seq <= int64(anchors); seq++ {
		row := store.ListInstallAuditChainRowsRow{
			ScopeText: install, IDText: fmt.Sprintf("00000001-0000-4000-8000-%012d", seq), ChainSeq: seq,
			ActorKind: "system", ActorID: "koinon-api:audit-anchor", Action: audit.ActionAnchor, TargetKind: "install", TargetID: install,
			SummaryText: summary, CreatedMicros: 1700000000000000 + seq, PrevHash: prev,
		}
		row.Hash = audit.InstallRowHash(row)
		prev = row.Hash
		ev, err := json.Marshal(audit.ExportEvent{
			ChainSeq: seq, ID: row.IDText, ActorKind: row.ActorKind, ActorID: row.ActorID, Action: row.Action,
			TargetKind: row.TargetKind, TargetID: row.TargetID, SummaryText: row.SummaryText, Summary: json.RawMessage(row.SummaryText),
			CreatedAtMicros: row.CreatedMicros, PrevHash: hex.EncodeToString(row.PrevHash), Hash: hex.EncodeToString(row.Hash),
		})
		Expect(err).NotTo(HaveOccurred())
		al, err := json.Marshal(audit.ExportAnchorLine{
			Line: audit.ExportAnchorLineKind, ChainSeq: seq, EventID: row.IDText, OrgCount: orgs, HeadsRoot: root, Heads: exportHeads,
		})
		Expect(err).NotTo(HaveOccurred())
		events = append(events, string(ev), string(al))
	}
	hdr, err := json.Marshal(audit.ExportHeader{
		Format: audit.ExportFormat, Chain: "install", InstallID: install, FromSeq: 1,
		Head: audit.ExportHead{ChainSeq: int64(anchors), Hash: hex.EncodeToString(prev)},
	})
	Expect(err).NotTo(HaveOccurred())
	return append([]string{string(hdr)}, events...), orgIDs
}

// manyAnchorReader yields a header then event/anchor_heads pairs without joining them into one string.
type manyAnchorReader struct {
	hdr   []byte
	pairs []struct {
		ev, heads []byte
	}
	i   int // 0 = header, then 2*i-1 event / 2*i heads for pair i-1
	buf []byte
}

func (r *manyAnchorReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		switch {
		case r.i == 0:
			r.buf, r.i = r.hdr, 1
		case (r.i-1)/2 >= len(r.pairs):
			return 0, io.EOF
		case (r.i-1)%2 == 0:
			r.buf = append(append([]byte{}, r.pairs[(r.i-1)/2].ev...), '\n')
			r.i++
		default:
			r.buf = append(append([]byte{}, r.pairs[(r.i-1)/2].heads...), '\n')
			r.i++
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
