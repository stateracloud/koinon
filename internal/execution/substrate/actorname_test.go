// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
)

// forgetfulNames is a ledger that holds no name: every claim is new. Specs of the guards behind the
// ledger use it, to show what happens when a name does come back.
type forgetfulNames struct{}

func (forgetfulNames) ClaimActorName(context.Context, string, string) (bool, error) { return true, nil }

// Issue #1092: atenet-egress caches an actor JWT by actor name, so a new actor under the name of a deleted
// one got the deleted actor's token (lab, rc.33). The driver refuses a used name, from a durable ledger.
var _ = Describe("the actor name ledger", func() {
	It("refuses to create an actor under the name of a deleted actor, and creates nothing", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		_, err := d.DeleteSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		creates := c.Calls("CreateActor")

		_, err = d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).To(MatchError(execution.ErrNameReused))
		var e *execution.Error
		Expect(errors.As(err, &e)).To(BeTrue())
		Expect(e.Op).To(Equal("CreateSandbox"))
		Expect(c.Calls("CreateActor")).To(Equal(creates), "the driver sent the used name to Substrate")
		_, err = d.GetSandbox(ctx, ws, "a")
		Expect(err).To(MatchError(execution.ErrNotFound), "an actor was made under the used name")

		By("a new name works")
		ready(ctx, d, "a-2")
	})

	It("returns the live actor when the same create comes again", func(ctx SpecContext) {
		d, c := newDriver()
		first, err := d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).NotTo(HaveOccurred())
		again, err := d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).NotTo(HaveOccurred())
		Expect(again.ID).To(Equal(first.ID))
		Expect(c.Calls("CreateActor")).To(Equal(1))
	})

	It("refuses a name that a create claimed and that has no actor (the create failed or the process ended)", func(ctx SpecContext) {
		names := fakeate.NewActorNames()
		cfg := specConfig()
		cfg.ActorNames = names
		d, c := newDriver(cfg)
		fresh, err := names.ClaimActorName(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(BeTrue())

		_, err = d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).To(MatchError(execution.ErrNameReused))
		Expect(c.Calls("CreateActor")).To(BeZero())
	})

	It("refuses the name after a restart of the driver and in a second replica that share the ledger", func(ctx SpecContext) {
		cl := fakeate.New()
		control, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cl.Stop)
		DeferCleanup(control.Close)
		DeferCleanup(router.Close)
		names := fakeate.NewActorNames()
		cfg := specConfig()
		cfg.ActorNames = names

		first := substrate.New(control, router, cfg)
		_, err = first.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		ready(ctx, first, "a")
		_, err = first.DeleteSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())

		By("a new driver process, which has nothing of the first in memory")
		restarted := substrate.New(control, router, cfg)
		_, err = restarted.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).To(MatchError(execution.ErrNameReused))

		By("a second replica that runs next to the first")
		replica := substrate.New(control, router, cfg)
		ready(ctx, replica, "b")
		_, err = first.DeleteSandbox(ctx, ws, "b")
		Expect(err).NotTo(HaveOccurred())
		_, err = first.CreateSandbox(ctx, ws, spec("b"))
		Expect(err).To(MatchError(execution.ErrNameReused))
		Expect(cl.Calls("CreateActor")).To(Equal(2), "only the two first creates reached Substrate")
	})

	It("refuses to restore into the name of a deleted actor", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "src")
		snap, err := d.SnapshotSandbox(ctx, ws, "src")
		Expect(err).NotTo(HaveOccurred())
		ready(ctx, d, "gone")
		_, err = d.DeleteSandbox(ctx, ws, "gone")
		Expect(err).NotTo(HaveOccurred())
		creates := c.Calls("CreateActor")

		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("gone"))
		Expect(err).To(MatchError(execution.ErrNameReused))
		Expect(c.Calls("CreateActor")).To(Equal(creates))

		By("a restore onto a live actor is ErrAlreadyExists, as before")
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("src"))
		Expect(err).To(MatchError(execution.ErrAlreadyExists))
		Expect(err).NotTo(MatchError(execution.ErrNameReused))
	})

	It("fails closed without a ledger, or with a ledger that does not answer", func(ctx SpecContext) {
		noActorNames = true
		DeferCleanup(func() { noActorNames = false })
		d, c := newDriver()
		_, err := d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).To(MatchError(execution.ErrFailedPrecondition))
		Expect(c.Calls("CreateActor")).To(BeZero())

		names := fakeate.NewActorNames()
		names.Err = errors.New("koinon_test: the database does not answer")
		noActorNames = false
		cfg := specConfig()
		cfg.ActorNames = names
		d, c = newDriver(cfg)
		_, err = d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).To(MatchError(execution.ErrUnavailable))
		Expect(c.Calls("CreateActor")).To(BeZero())
	})
})
