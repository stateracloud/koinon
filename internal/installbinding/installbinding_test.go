// SPDX-License-Identifier: Apache-2.0

package installbinding_test

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/installbinding"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newBinding(mark string, held bool) *installbinding.Binding {
	return installbinding.New(pool, mark, held, quiet).WithScope(installbinding.ScopeEdge | installbinding.ScopeCheckpoints)
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

var _ = Describe("Origin and Conflicts", func() {
	It("keeps scheme, host and port and nothing else", func() {
		Expect(installbinding.Origin("https://user:pw@koinon.example.com:8443/path?q=1#f")).To(Equal("https://koinon.example.com:8443"))
		Expect(installbinding.Origin("")).To(BeEmpty())
		Expect(installbinding.Origin("not a url")).To(BeEmpty())
	})

	It("lists only the targets that are equal and set", func() {
		rec := installbinding.Targets{PublicURL: "https://a", OIDCRedirectURL: "https://a", CheckpointEndpoint: "s3.a", CheckpointBucket: "b"}
		own := installbinding.Targets{PublicURL: "https://a", OIDCRedirectURL: "https://c", CheckpointEndpoint: "s3.a", CheckpointBucket: "other"}
		Expect(installbinding.Conflicts(rec, own)).To(ConsistOf(installbinding.Conflict{Target: "public-url", Value: "https://a"}))
		Expect(installbinding.Conflicts(installbinding.Targets{}, installbinding.Targets{})).To(BeEmpty())
	})
})

var _ = Describe("Binding", func() {
	BeforeEach(func(ctx SpecContext) {
		for _, stmt := range []string{`DELETE FROM install_binding`, `DELETE FROM api_tokens`, `DELETE FROM sessions`} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	row := func(ctx context.Context, b *installbinding.Binding) (installbinding.Row, installbinding.State) {
		GinkgoHelper()
		r, st, err := b.Read(ctx)
		Expect(err).NotTo(HaveOccurred())
		return r, st
	}

	It("is disabled without a mark: it writes nothing and refuses nothing", func(ctx SpecContext) {
		b := newBinding("", false)
		Expect(b.Enabled()).To(BeFalse())
		st, err := b.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		Expect(st).To(Equal(installbinding.StateSame))
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_binding`).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
		ok, err := b.Allows(ctx, text("whatever"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(b.Stamp().Valid).To(BeFalse())
	})

	It("claims the row on an unheld start with no row (an upgrade of a live install)", func(ctx SpecContext) {
		b := newBinding("hash-a", false)
		st, err := b.Claim(ctx, installbinding.Targets{PublicURL: "https://koinon.example.com", OIDCRedirectURL: "https://koinon.example.com"})
		Expect(err).NotTo(HaveOccurred())
		Expect(st).To(Equal(installbinding.StateSame))
		r, _ := row(ctx, b)
		Expect(r.Bound).To(BeTrue())
		Expect(r.Mark).To(Equal("hash-a"))
		Expect(r.Targets.PublicURL).To(Equal("https://koinon.example.com"))
		Expect(r.HeldSince).To(BeNil())
	})

	It("refreshes only the columns of its scope", func(ctx SpecContext) {
		_, err := newBinding("hash-a", false).Claim(ctx, installbinding.Targets{PublicURL: "https://a", CheckpointEndpoint: "s3.a", CheckpointBucket: "b"})
		Expect(err).NotTo(HaveOccurred())
		agentd := installbinding.New(pool, "hash-a", false, quiet).WithScope(installbinding.ScopeCheckpoints)
		_, err = agentd.Claim(ctx, installbinding.Targets{CheckpointEndpoint: "s3.new", CheckpointBucket: "b2"})
		Expect(err).NotTo(HaveOccurred())
		none := installbinding.New(pool, "hash-a", false, quiet)
		_, err = none.Claim(ctx, installbinding.Targets{PublicURL: "https://ignored", CheckpointEndpoint: "ignored"})
		Expect(err).NotTo(HaveOccurred())
		r, _ := row(ctx, none)
		Expect(r.Targets).To(Equal(installbinding.Targets{PublicURL: "https://a", CheckpointEndpoint: "s3.new", CheckpointBucket: "b2"}))
	})

	It("writes an unbound row on a held start with no row, so the state is copy and stays copy", func(ctx SpecContext) {
		held := newBinding("hash-a", true)
		st, err := held.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		Expect(st).To(Equal(installbinding.StateCopy))
		r, _ := row(ctx, held)
		Expect(r.Bound).To(BeFalse())
		Expect(r.HeldSince).NotTo(BeNil())

		By("an unheld start of the same install after the release does not claim the row")
		unheld := newBinding("hash-a", false)
		st, err = unheld.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		Expect(st).To(Equal(installbinding.StateCopy))
		r, _ = row(ctx, unheld)
		Expect(r.Bound).To(BeFalse())
	})

	It("sets held_since at start when the row already exists", func(ctx SpecContext) {
		src := newBinding("hash-a", false)
		_, err := src.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		r, _ := row(ctx, src)
		Expect(r.HeldSince).To(BeNil())

		held := newBinding("hash-a", true)
		_, err = held.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		r, _ = row(ctx, held)
		Expect(r.HeldSince).NotTo(BeNil())
	})

	It("lets two processes that start at once agree on one row", func(ctx SpecContext) {
		a, b := newBinding("hash-a", false), newBinding("hash-b", false)
		errs := make(chan error, 2)
		states := make(chan installbinding.State, 2)
		for _, x := range []*installbinding.Binding{a, b} {
			go func() {
				st, err := x.Claim(context.Background(), installbinding.Targets{})
				states <- st
				errs <- err
			}()
		}
		Expect(<-errs).NotTo(HaveOccurred())
		Expect(<-errs).NotTo(HaveOccurred())
		got := []installbinding.State{<-states, <-states}
		Expect(got).To(ConsistOf(installbinding.StateSame, installbinding.StateCopy))
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_binding`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("does not refresh targets or stamp legacy rows in state copy", func(ctx SpecContext) {
		Expect(newBinding("hash-a", false).Claim(ctx, installbinding.Targets{PublicURL: "https://a"})).To(Equal(installbinding.StateSame))
		b := newBinding("hash-b", false)
		st, err := b.Claim(ctx, installbinding.Targets{PublicURL: "https://b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(st).To(Equal(installbinding.StateCopy))
		r, _ := row(ctx, b)
		Expect(r.Mark).To(Equal("hash-a"))
		Expect(r.Targets.PublicURL).To(Equal("https://a"), "the row is never overwritten automatically")
	})

	It("re-asserts the hold: writes the row a load removed, and sets held_since once after a release", func(ctx SpecContext) {
		b := newBinding("hash-a", true)
		_, err := b.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())

		By("a dump load replaces the table")
		_, err = pool.Exec(ctx, `DELETE FROM install_binding`)
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Reassert(ctx)).To(Succeed())
		r, _ := row(ctx, b)
		Expect(r.Bound).To(BeFalse())
		Expect(r.HeldSince).NotTo(BeNil())

		By("the release clears held_since and sets released_at; this old held process may not set it again")
		_, err = pool.Exec(ctx, `UPDATE install_binding SET held_since = NULL, released_at = $1, released_by = 'hash-a'`, time.Now().Add(time.Minute))
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Reassert(ctx)).To(Succeed())
		r, _ = row(ctx, b)
		Expect(r.HeldSince).To(BeNil())
	})

	It("allows a credential of its own mark, refuses another's, and counts the refusals", func(ctx SpecContext) {
		b := newBinding("hash-a", false)
		reg := prometheus.NewRegistry()
		b.Metrics(reg)
		_, err := b.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())

		ok, err := b.Allows(ctx, text("hash-a"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		ok, err = b.Allows(ctx, text("hash-x"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		ok, err = b.Allows(ctx, pgtype.Text{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(), "a legacy row without a mark is the install's own in state same")

		b.Refused("token")
		b.Refused("session")
		b.Refused("session")
		Expect(testutil.CollectAndCount(reg, "koinon_auth_quarantined_total")).To(Equal(2))
		Expect(testutil.CollectAndCount(reg, "koinon_install_binding")).To(Equal(3))
	})

	It("refuses a legacy row without a mark in state copy", func(ctx SpecContext) {
		_, err := newBinding("hash-a", false).Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		b := newBinding("hash-b", false)
		_, err = b.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		ok, err := b.Allows(ctx, pgtype.Text{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("adopt-restore makes the copy the owner and lists the conflicts it clears", func(ctx SpecContext) {
		src := newBinding("hash-src", false)
		_, err := src.Claim(ctx, installbinding.Targets{PublicURL: "https://koinon.example.com"})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE install_binding SET checkpoint_endpoint = 's3.example.com', checkpoint_bucket = 'snap'`)
		Expect(err).NotTo(HaveOccurred())

		cp := newBinding("hash-copy", false)
		st, err := cp.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		Expect(st).To(Equal(installbinding.StateCopy))

		res, err := cp.Adopt(ctx, audit.System("test-recovery"), installbinding.Targets{
			PublicURL: "https://koinon.example.com", CheckpointEndpoint: "s3.example.com", CheckpointBucket: "snap",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.PreviousState).To(Equal(installbinding.StateCopy))
		Expect(res.PreviousMark).To(Equal("hash-src"))
		Expect(res.Conflicts).To(ConsistOf(
			installbinding.Conflict{Target: "checkpoints", Value: "s3.example.com/snap"},
			installbinding.Conflict{Target: "public-url", Value: "https://koinon.example.com"},
		))
		r, st := row(ctx, cp)
		Expect(st).To(Equal(installbinding.StateSame))
		Expect(r.ReleasedBy).To(Equal("hash-copy"))

		By("the audit chain records it")
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'recovery.adopt-restore'`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))

		By("the source's own mark is now the copy's state")
		_, st = row(ctx, src)
		Expect(st).To(Equal(installbinding.StateCopy))
	})

	It("adopt-restore without a mark is an error", func(ctx SpecContext) {
		_, err := newBinding("", false).Adopt(ctx, audit.System("test-recovery"), installbinding.Targets{})
		Expect(err).To(MatchError(installbinding.ErrNoMark))
	})
})
