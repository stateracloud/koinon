// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/harness"
)

// Issue #1387: on the gVisor tiers the agent container drops every capability and adds only what the
// launcher needs for the drop to the non-root user (and KILL for the guest front). The vm tier keeps
// the Substrate default set until its proof (issue #1462).
var _ = Describe("the capability set of the actor template", func() {
	cfg := func(def harness.Tier) substrate.Config {
		return substrate.Config{
			GuestSigner:       fakeate.Signer(),
			DefaultTier:       def,
			RuntimeClassTiers: map[string]harness.Tier{"kata": harness.TierVM, "gvisor": harness.TierHardened, "gvisor-dev": harness.TierDev},
		}
	}
	render := func(class string, def harness.Tier) *ateapiv1.ActorTemplate {
		t, err := substrate.RenderTemplateForTest("ws", execution.SandboxSpec{Name: "a", Image: "ghcr.io/org/app:1" + digest, RuntimeClassName: class}, cfg(def))
		Expect(err).NotTo(HaveOccurred())
		return t
	}

	DescribeTable("drops ALL and adds only SETUID, SETGID, SETPCAP and KILL on the gVisor tiers",
		func(class string, def harness.Tier) {
			caps := render(class, def).GetContainers()[0].GetSecurityContext().GetCapabilities()
			Expect(caps.GetDrop()).To(Equal([]string{"ALL"}))
			Expect(caps.GetAdd()).To(ConsistOf("SETUID", "SETGID", "SETPCAP", "KILL"))
			Expect(caps.GetAdd()).NotTo(ContainElement("NET_BIND_SERVICE"), "the gVisor netstack lets the front listen on port 80 without it")
		},
		Entry("hardened runtime class", "gvisor", harness.Tier("")),
		Entry("dev runtime class", "gvisor-dev", harness.Tier("")),
		Entry("no runtime class, dev default tier", "", harness.TierDev),
	)

	DescribeTable("leaves the Substrate default set where the tier is not gVisor",
		func(class string, def harness.Tier) {
			Expect(render(class, def).GetContainers()[0].GetSecurityContext()).To(BeNil())
		},
		Entry("vm runtime class", "kata", harness.Tier("")),
		Entry("no runtime class, vm default tier", "", harness.TierVM),
		Entry("unknown runtime class (Exec refuses it)", "other", harness.Tier("")),
	)

	It("gives a changed capability set a new template name", func() {
		vm, gv := render("", harness.TierVM), render("", harness.TierDev)
		Expect(vm.GetMetadata().GetName()).NotTo(Equal(gv.GetMetadata().GetName()))
		Expect(render("", harness.TierDev).GetMetadata().GetName()).To(Equal(gv.GetMetadata().GetName()), "the name is stable")
	})

	It("does not share the slice with the package value", func() {
		render("gvisor", "").GetContainers()[0].GetSecurityContext().GetCapabilities().Add[0] = "SYS_ADMIN"
		Expect(substrate.GVisorCapAdd()).NotTo(ContainElement("SYS_ADMIN"))
	})
})

// Issue #1481: every template has the wakeup probe on the front, on every tier.
var _ = Describe("the wakeup probe of the actor template", func() {
	It("polls /readyz on port 80 of the front, on every tier", func() {
		for _, class := range []string{"", "kata", "gvisor"} {
			t, err := substrate.RenderTemplateForTest("ws", execution.SandboxSpec{Name: "a", Image: "ghcr.io/org/app:1" + digest, RuntimeClassName: class}, substrate.Config{
				GuestSigner:       fakeate.Signer(),
				DefaultTier:       harness.TierDev,
				RuntimeClassTiers: map[string]harness.Tier{"kata": harness.TierVM, "gvisor": harness.TierHardened},
			})
			Expect(err).NotTo(HaveOccurred())
			p := t.GetContainers()[0].GetWakeupProbe()
			Expect(p.GetHttpGet().GetPath()).To(Equal("/readyz"), "class %q", class)
			Expect(p.GetHttpGet().GetPort()).To(BeEquivalentTo(80))
			Expect(p.GetTimeoutSeconds()).To(BeEquivalentTo(substrate.FrontProbeTimeoutSeconds))
		}
	})
})
