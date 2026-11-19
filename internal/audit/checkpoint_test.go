// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

type seedKeys struct {
	fill byte
}

func (s *seedKeys) SigningKey(_ context.Context, name string) (*audit.SigningKey, error) {
	return audit.NewSigningKey(name, bytes.Repeat([]byte{s.fill}, audit.SeedSize))
}

// Each spec has a database of its own. The tampering happens in a transaction that the spec rolls back.
var _ = Describe("signed audit checkpoints", func() {
	var (
		db   *pgxpool.Pool
		org  pgtype.UUID
		keys *seedKeys
	)

	writeOrg := func(ctx context.Context, n int) {
		GinkgoHelper()
		for i := range n {
			_, err := audit.InTx(ctx, db, func(_ *store.Queries, b audit.Batch) error {
				b.Org(audit.Event{Org: org, Actor: anchorSpecActor, Action: fmt.Sprintf("test.n%d", i), TargetKind: "test", TargetID: "t"})
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	}
	anchorAndSign := func(ctx context.Context) audit.CheckpointResult {
		GinkgoHelper()
		a, err := audit.Anchor(ctx, db, audit.AnchorOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Written).To(BeTrue())
		res, err := audit.Checkpoint(ctx, db, keys, audit.CheckpointOptions{})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	installLines := func(ctx context.Context) []string {
		GinkgoHelper()
		return exportLinesFrom(ctx, store.New(db), audit.ExportScope{Install: true}, 1)
	}
	latest := func(ctx context.Context) []byte {
		GinkgoHelper()
		note, ok, err := audit.LatestCheckpoint(ctx, store.New(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		return note
	}
	pubKeys := func(ctx context.Context) []audit.PublicKey {
		GinkgoHelper()
		ks, err := audit.SigningKeys(ctx, store.New(db))
		Expect(err).NotTo(HaveOccurred())
		return ks
	}

	BeforeEach(func(ctx SpecContext) {
		db = pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
		org, _, _ = pgtest.NewOrg(ctx, db)
		keys = &seedKeys{fill: 1}
		writeOrg(ctx, 3)
	})

	It("skips until there is an anchor, writes a key event and a checkpoint, then skips while current", func(ctx SpecContext) {
		res, err := audit.Checkpoint(ctx, db, keys, audit.CheckpointOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Written).To(BeFalse())
		Expect(res.Skipped).To(Equal("no_anchor"))

		first := anchorAndSign(ctx)
		Expect(first.Written).To(BeTrue())
		Expect(first.KeyAdded).To(BeTrue())
		Expect(pubKeys(ctx)).To(HaveLen(1))

		res, err = audit.Checkpoint(ctx, db, keys, audit.CheckpointOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Skipped).To(Equal("current"))

		writeOrg(ctx, 1)
		second := anchorAndSign(ctx)
		Expect(second.Written).To(BeTrue())
		Expect(second.KeyAdded).To(BeFalse(), "a published key is not published twice")
		Expect(second.HeadSeq).To(BeNumerically(">", first.HeadSeq))
		Expect(pubKeys(ctx)).To(HaveLen(1))
	})

	It("signs the anchor row, and the note names the install origin", func(ctx SpecContext) {
		res := anchorAndSign(ctx)
		n, _, err := audit.VerifyNote([]byte(res.Note), pubKeys(ctx))
		Expect(err).NotTo(HaveOccurred())
		Expect(n.ChainSeq).To(Equal(res.HeadSeq))
		Expect(n.Origin).To(HavePrefix("koinon.dev/install/"))
		Expect(n.Origin).To(HaveSuffix("/audit"))
		Expect(string(latest(ctx))).To(Equal(res.Note))
	})

	It("verifies an export against the checkpoint, including an export that runs past it", func(ctx SpecContext) {
		res := anchorAndSign(ctx)
		n, _, err := audit.VerifyNote([]byte(res.Note), pubKeys(ctx))
		Expect(err).NotTo(HaveOccurred())

		out := verifyLines(installLines(ctx), audit.ExportVerifyOptions{Checkpoint: &n})
		Expect(out.Break).To(BeNil())
		Expect(out.CheckpointChecked).To(BeTrue())

		writeOrg(ctx, 2)
		anchorAndSign(ctx)
		out = verifyLines(installLines(ctx), audit.ExportVerifyOptions{Checkpoint: &n})
		Expect(out.Break).To(BeNil())
		Expect(out.CheckpointChecked).To(BeTrue())
	})

	It("fails an export that ends before the checkpoint", func(ctx SpecContext) {
		res := anchorAndSign(ctx)
		n, _, err := audit.VerifyNote([]byte(res.Note), pubKeys(ctx))
		Expect(err).NotTo(HaveOccurred())
		lines := installLines(ctx)
		short := verifyLines(lines[:3], audit.ExportVerifyOptions{Checkpoint: &n})
		Expect(short.Break).NotTo(BeNil())
	})

	It("refuses a checkpoint of another install and a checkpoint for an org export", func(ctx SpecContext) {
		res := anchorAndSign(ctx)
		n, _, err := audit.VerifyNote([]byte(res.Note), pubKeys(ctx))
		Expect(err).NotTo(HaveOccurred())
		other := n
		other.Origin = audit.NoteOrigin("99999999-9999-9999-9999-999999999999")
		_, err = audit.VerifyExport(strings.NewReader(strings.Join(installLines(ctx), "\n")+"\n"), audit.ExportVerifyOptions{Checkpoint: &other})
		Expect(err).To(HaveOccurred())

		orgLines := exportLinesFrom(ctx, store.New(db), audit.ExportScope{Org: org}, 1)
		_, err = audit.VerifyExport(strings.NewReader(strings.Join(orgLines, "\n")+"\n"), audit.ExportVerifyOptions{Checkpoint: &n})
		Expect(err).To(HaveOccurred())
	})

	Describe("a chain rewritten after the checkpoint", func() {
		// rewriteInstall rewrites the install chain from chain_seq from on (past the triggers, in a transaction
		// that the spec rolls back) and recomputes every hash: a chain that is self-consistent and still fails.
		rewriteInstall := func(ctx context.Context, from int64) {
			GinkgoHelper()
			tx, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
			_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
			Expect(err).NotTo(HaveOccurred())
			prev := make([]byte, 32) // the chain starts from a zero hash
			if from > 1 {
				Expect(tx.QueryRow(ctx, `SELECT hash FROM install_audit_events WHERE chain_seq = $1`, from-1).Scan(&prev)).To(Succeed())
			}
			var last int64
			Expect(tx.QueryRow(ctx, `SELECT max(chain_seq) FROM install_audit_events`).Scan(&last)).To(Succeed())
			for seq := from; seq <= last; seq++ {
				// audit-lint:allow a tamper test: rewrites the install chain past the triggers so the checkpoint has a break to find
				_, err := tx.Exec(ctx, `UPDATE install_audit_events SET actor_id = 'koinon-test:mallory', prev_hash = $2 WHERE chain_seq = $1`, seq, prev)
				Expect(err).NotTo(HaveOccurred())
				// audit-lint:allow a tamper test: recomputes the rewritten row's hash
				Expect(tx.QueryRow(ctx, `UPDATE install_audit_events SET hash = install_audit_event_hash(install_audit_events) WHERE chain_seq = $1 RETURNING hash`, seq).Scan(&prev)).To(Succeed())
			}
			// audit-lint:allow a tamper test: moves the install head to the rewritten chain
			_, err = tx.Exec(ctx, `UPDATE install_audit_chain_head SET hash = $1`, prev)
			Expect(err).NotTo(HaveOccurred())
			rewritten = exportLinesFrom(ctx, store.New(tx), audit.ExportScope{Install: true}, 1)
		}

		It("fails verification against the earlier checkpoint, though the chain is self-consistent", func(ctx SpecContext) {
			res := anchorAndSign(ctx)
			n, _, err := audit.VerifyNote([]byte(res.Note), pubKeys(ctx))
			Expect(err).NotTo(HaveOccurred())
			Expect(verifyLines(installLines(ctx), audit.ExportVerifyOptions{Checkpoint: &n}).Break).To(BeNil())

			writeOrg(ctx, 1)
			anchorAndSign(ctx)
			rewriteInstall(ctx, n.ChainSeq)

			plain := verifyLines(rewritten, audit.ExportVerifyOptions{})
			Expect(plain.Break).To(BeNil(), "the rewritten chain is internally consistent: only the checkpoint finds it")
			pinned := verifyLines(rewritten, audit.ExportVerifyOptions{Checkpoint: &n})
			Expect(pinned.Break).NotTo(BeNil())
			Expect(pinned.Break.Seq).To(Equal(n.ChainSeq))
		})

		It("fails an org chain rewritten behind an anchor, through the install anchors under the checkpoint", func(ctx SpecContext) {
			res := anchorAndSign(ctx)
			n, _, err := audit.VerifyNote([]byte(res.Note), pubKeys(ctx))
			Expect(err).NotTo(HaveOccurred())
			good := verifyLines(installLines(ctx), audit.ExportVerifyOptions{Checkpoint: &n, CollectAnchors: true})
			Expect(good.Break).To(BeNil())
			set := good.AnchorSet()
			Expect(set).NotTo(BeNil())

			tx, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
			for _, stmt := range []string{
				`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`,
				`ALTER TABLE audit_chain_heads DISABLE TRIGGER audit_chain_heads_guard`,
			} {
				_, err = tx.Exec(ctx, stmt)
				Expect(err).NotTo(HaveOccurred())
			}
			var prev []byte
			Expect(tx.QueryRow(ctx, `SELECT hash FROM audit_events WHERE org_id = $1 AND chain_seq = 1`, org).Scan(&prev)).To(Succeed())
			for seq := int64(2); seq <= 3; seq++ {
				// audit-lint:allow a tamper test: rewrites an org chain past the triggers so the checkpoint's anchor has a break to find
				_, err := tx.Exec(ctx, `UPDATE audit_events SET actor_id = 'koinon-test:mallory', prev_hash = $3 WHERE org_id = $1 AND chain_seq = $2`, org, seq, prev)
				Expect(err).NotTo(HaveOccurred())
				// audit-lint:allow a tamper test: recomputes the rewritten row's hash
				Expect(tx.QueryRow(ctx, `UPDATE audit_events SET hash = audit_event_hash(audit_events) WHERE org_id = $1 AND chain_seq = $2 RETURNING hash`, org, seq).Scan(&prev)).To(Succeed())
			}
			// audit-lint:allow a tamper test: moves the head to the rewritten chain
			_, err = tx.Exec(ctx, `UPDATE audit_chain_heads SET hash = $2 WHERE org_id = $1`, org, prev)
			Expect(err).NotTo(HaveOccurred())

			orgLines := exportLinesFrom(ctx, store.New(tx), audit.ExportScope{Org: org}, 1)
			Expect(verifyLines(orgLines, audit.ExportVerifyOptions{}).Break).To(BeNil())
			Expect(verifyLines(orgLines, audit.ExportVerifyOptions{Anchors: set}).Break).NotTo(BeNil())
		})

		It("is refused by the signer: a chain that lost the signed head gets no new checkpoint", func(ctx SpecContext) {
			anchorAndSign(ctx)
			writeOrg(ctx, 1)
			a, err := audit.Anchor(ctx, db, audit.AnchorOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Written).To(BeTrue())

			tx, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
			_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
			Expect(err).NotTo(HaveOccurred())
			// audit-lint:allow a tamper test: changes the signed row's hash past the triggers
			_, err = tx.Exec(ctx, `UPDATE install_audit_events SET hash = '\x00' || substring(hash from 2) WHERE chain_seq = (SELECT max((summary->'after'->>'anchor_seq')::bigint) FROM install_audit_events WHERE action = 'audit.checkpoint')`)
			Expect(err).NotTo(HaveOccurred())
			_, err = audit.Checkpoint(ctx, tx, keys, audit.CheckpointOptions{})
			Expect(err).To(MatchError(audit.ErrCheckpointInconsistent))
		})
	})

	It("verifies a note of a rotated key next to the new key, and fails a note of an unknown key", func(ctx SpecContext) {
		oldNote := anchorAndSign(ctx)
		keys.fill = 2 // rotation: the Secret is deleted and the next one holds a new seed
		writeOrg(ctx, 1)
		fresh := anchorAndSign(ctx)
		Expect(fresh.KeyAdded).To(BeTrue(), "the new public key is published as its own event")
		known := pubKeys(ctx)
		Expect(known).To(HaveLen(2))
		for _, raw := range []string{oldNote.Note, fresh.Note} {
			_, _, err := audit.VerifyNote([]byte(raw), known)
			Expect(err).NotTo(HaveOccurred())
		}
		_, _, err := audit.VerifyNote([]byte(oldNote.Note), known[1:])
		Expect(err).To(MatchError(audit.ErrNoteUnknownKey), "a verifier that pinned only the new key rejects the old note")
	})

	It("never lets a second replica sign the same anchor twice", func(ctx SpecContext) {
		a, err := audit.Anchor(ctx, db, audit.AnchorOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Written).To(BeTrue())
		var tx pgx.Tx
		tx, err = db.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
		Expect(store.New(tx).LockAuditAnchor(ctx)).To(Succeed())
		res, err := audit.Checkpoint(ctx, db, keys, audit.CheckpointOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Skipped).To(Equal("busy"))
	})
})

var rewritten []string
