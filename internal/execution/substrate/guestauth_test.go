// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"crypto/ed25519"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	"github.com/stateracloud/koinon/internal/guestauth"
	"github.com/stateracloud/koinon/internal/harness"
)

// The guest API token (issue #1309): the driver signs every guest call, and the guest front of an
// actor (fakeate's interceptor here, internal/guestfront in a real actor) refuses any other call.
var _ = Describe("the guest API token", func() {
	run := func(ctx SpecContext, d *substrate.Driver, name string) error {
		p, err := d.Exec(ctx, ws, name, execution.ExecRequest{Command: []string{"cat"}})
		if err != nil {
			return err
		}
		Expect(p.Stdin().Close()).To(Succeed())
		_, _ = io.ReadAll(p.Stdout())
		_, err = p.Wait()
		_ = p.Close()
		return err
	}
	aud := func(name string) string { return guestauth.Audience(ws + "/" + name) }

	Describe("without a signing key", func() {
		BeforeEach(func() {
			noSigner = true
			DeferCleanup(func() { noSigner = false })
		})

		It("refuses to create a sandbox or name a spec", func(ctx SpecContext) {
			d, cl := newDriver()
			_, err := d.CreateSandbox(ctx, ws, spec("x"))
			Expect(err).To(MatchError(execution.ErrFailedPrecondition))
			_, err = d.SpecID(ws, spec("x"))
			Expect(err).To(MatchError(execution.ErrFailedPrecondition))
			Expect(cl.Templates(ws)).To(BeEmpty())
		})
	})

	It("pins the front of a sandbox to the sandbox on its first call", func(ctx SpecContext) {
		d, cl := newDriver()
		ready(ctx, d, "sb")
		Expect(run(ctx, d, "sb")).To(Succeed())
		Expect(cl.Pin(ws, "sb")).To(Equal(aud("sb")))
		Expect(cl.Calls("Unauthenticated")).To(BeZero())
	})

	It("is refused when another key signed it, and no process starts", func(ctx SpecContext) {
		cl := fakeate.New()
		control, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cl.Stop)
		DeferCleanup(control.Close)
		DeferCleanup(router.Close)
		cfg := func(s *guestauth.Signer) substrate.Config {
			return substrate.Config{
				PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond,
				DefaultTier: harness.TierDev, GuestSigner: s, GatewayRoutes: gatewayRoutes,
			}
		}
		owner := substrate.New(control, router, cfg(fakeate.Signer()))
		_, err = owner.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		ready(ctx, owner, "sb")

		other, err := guestauth.ParseSeed(strings.Repeat("22", 32)) // obviously fake
		Expect(err).NotTo(HaveOccurred())
		s, err := guestauth.NewSigner(other)
		Expect(err).NotTo(HaveOccurred())
		intruder := substrate.New(control, router, cfg(s))
		Expect(run(ctx, intruder, "sb")).NotTo(Succeed())
		Expect(cl.Calls("Unauthenticated")).To(BeNumerically(">", 0))
		Expect(cl.Started(ws, "sb")).To(BeEmpty())
		Expect(run(ctx, owner, "sb")).To(Succeed())
	})

	It("puts the public key, and never the private key, in the template", func(ctx SpecContext) {
		d, cl := newDriver()
		ready(ctx, d, "sb")
		tmpl, ok := cl.Template(ws, cl.TemplateOf(ws, "sb"))
		Expect(ok).To(BeTrue())
		keys, err := guestauth.DecodeKeys(tmpl.GetContainers()[0].GetEnv()[0].GetValue())
		Expect(err).NotTo(HaveOccurred())
		Expect(keys).To(Equal([]ed25519.PublicKey{fakeate.Signer().PublicKey()}))
		Expect(tmpl.String()).NotTo(ContainSubstring(strings.Repeat("11", 32)))
	})

	It("moves the pin of a restored sandbox, along a chain of restores", func(ctx SpecContext) {
		d, cl := newDriver()
		ready(ctx, d, "a")
		Expect(run(ctx, d, "a")).To(Succeed())
		snap, err := d.SnapshotSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("b"))
		Expect(err).NotTo(HaveOccurred())
		_, err = d.WaitPhase(ctx, ws, "b", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Pin(ws, "b")).To(Equal(aud("a")), "the restored front has the source's pin in memory")
		Expect(run(ctx, d, "b")).To(Succeed())
		Expect(cl.Pin(ws, "b")).To(Equal(aud("b")))

		// A snapshot of b binds b's front first, so a restore of it names b, not a.
		snap, err = d.SnapshotSandbox(ctx, ws, "b")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("c"))
		Expect(err).NotTo(HaveOccurred())
		_, err = d.WaitPhase(ctx, ws, "c", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred())
		Expect(run(ctx, d, "c")).To(Succeed())
		Expect(cl.Pin(ws, "c")).To(Equal(aud("c")))
		Expect(cl.Calls("Unauthenticated")).To(BeZero())
	})

	It("binds a sandbox that no call has pinned before its memory snapshot", func(ctx SpecContext) {
		d, cl := newDriver()
		ready(ctx, d, "a")
		_, err := d.SnapshotSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Calls("Bind")).To(BeNumerically(">", 0))
		Expect(cl.Pin(ws, "a")).To(Equal(aud("a")))
	})

	It("binds a restored sandbox that no call reached before a snapshot of it while suspended", func(ctx SpecContext) {
		// A resume returns before the guest listens: Bind must wait for it, not fail Unavailable.
		latency = 50 * time.Millisecond
		DeferCleanup(func() { latency = 0 })
		d, cl := newDriver()
		ready(ctx, d, "a")
		snap, err := d.SnapshotSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("b"))
		Expect(err).NotTo(HaveOccurred())
		_, err = d.WaitPhase(ctx, ws, "b", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred())
		_, err = d.SuspendSandbox(ctx, ws, "b")
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Pin(ws, "b")).To(Equal(aud("a")), "no call reached b")

		snap, err = d.SnapshotSandbox(ctx, ws, "b")
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Pin(ws, "b")).To(Equal(aud("b")), "the snapshot binds b first")
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("c"))
		Expect(err).NotTo(HaveOccurred())
		_, err = d.WaitPhase(ctx, ws, "c", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred())
		Expect(run(ctx, d, "c")).To(Succeed())
		Expect(cl.Pin(ws, "c")).To(Equal(aud("c")))
		Expect(cl.Calls("Unauthenticated")).To(BeZero())
	})

	Describe("on a guest without the guest front", func() {
		BeforeEach(func() {
			unguarded = true
			DeferCleanup(func() { unguarded = false })
		})

		It("refuses the sandbox before the first guest call, and starts no process", func(ctx SpecContext) {
			d, cl := newDriver()
			ready(ctx, d, "sb")
			err := run(ctx, d, "sb")
			Expect(err).To(MatchError(execution.ErrFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("koinon-guest-front"))
			Expect(cl.Started(ws, "sb")).To(BeEmpty())
		})
	})
})
