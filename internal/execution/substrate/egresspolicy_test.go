// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// The actor egress policy (issue #1444): default deny plus the route to the gateway (ADR 0120).
var _ = Describe("the actor egress policy", func() {
	const gw, gwPort = fakeate.GatewayHost, fakeate.GatewayPort

	It("gives a created actor the gateway route before it runs, and nothing else", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		Expect(c.Calls("ResumeActor")).To(Equal(1))
		Expect(c.Calls("ResumeWithoutEgressPolicy")).To(BeZero(), "the actor ran before its policy was written")

		p := c.EgressPolicy(ws, "a")
		Expect(p).NotTo(BeNil())
		Expect(p.GetMetadata().GetName()).To(Equal("default"))
		Expect(p.GetRules()).To(HaveLen(1))
		Expect(p.GetRules()[0].GetHttp().GetHostnames()).To(Equal([]string{gw}))
		Expect(p.GetRules()[0].GetHttp().GetEffects()).To(BeNil(), "the rule holds no credential")
		Expect(p.GetRules()[0].GetHttps()).To(BeNil())
		Expect(p.GetRules()[0].GetTlsPassthrough()).To(BeNil())

		By("it reaches the gateway, and nothing else")
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
		Expect(c.EgressAllowed(ws, "a", gw, 443)).To(BeFalse(), "another port of the gateway")
		Expect(c.EgressAllowed(ws, "a", gw, 80)).To(BeFalse(), "the default HTTP port")
		Expect(c.EgressAllowed(ws, "a", "github.com", 80)).To(BeFalse())
		Expect(c.EgressAllowed(ws, "a", "api.openai.com", 443)).To(BeFalse())
		Expect(c.EgressAllowed(ws, "a", "evil."+gw, gwPort)).To(BeFalse(), "a name under the gateway's")
		Expect(c.EgressAllowed(ws, "a", "10.96.0.10", gwPort)).To(BeFalse(), "an IP address")
		Expect(c.EgressAllowed(ws, "a", "169.254.169.254", 80)).To(BeFalse(), "the metadata service")
	})

	It("lets an actor without the gateway route, or without a policy, reach nothing", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		c.SetEgressPolicy(ws, "a", []*ateapiv1.EgressRule{{Http: &ateapiv1.HttpRule{
			Hostnames: []string{"example.com"}, Ports: &ateapiv1.Ports{Numbers: []int32{80}},
		}}})
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeFalse())
		c.SetEgressPolicy(ws, "a", nil)
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeFalse())
		Expect(c.EgressAllowed(ws, "a", "example.com", 80)).To(BeFalse())
	})

	It("removes the policy with the actor", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		Expect(c.EgressPolicy(ws, "a")).NotTo(BeNil())
		_, err := d.DeleteSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressPolicy(ws, "a")).To(BeNil())
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeFalse())
		Expect(c.Calls("DeleteActorEgressPolicy")).To(BeZero(), "Substrate cascades the policy with the actor")

		By("a new actor with the same name gets a new policy")
		ready(ctx, d, "a")
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
	})

	It("removes the policy when an actor that goes away slowly is gone", func(ctx SpecContext) {
		d, c := newDriver()
		c.AsyncDelete = true
		ready(ctx, d, "a")
		_, err := d.DeleteSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() *ateapiv1.EgressPolicy { return c.EgressPolicy(ws, "a") }).WithContext(ctx).Should(BeNil())
	})

	It("puts back a changed policy before a start or a resume, and leaves an equal one alone", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		Expect(c.PolicyWrites(ws, "a")).To(Equal(1))

		_, err := d.StopSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.StartSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.PolicyWrites(ws, "a")).To(Equal(1), "an equal policy is not written again")
		_, err = d.WaitPhase(ctx, ws, "a", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred())

		for _, op := range []string{"StartSandbox", "ResumeSandbox"} {
			By(op)
			_, err = d.StopSandbox(ctx, ws, "a")
			Expect(err).NotTo(HaveOccurred())
			c.SetEgressPolicy(ws, "a", []*ateapiv1.EgressRule{{Http: &ateapiv1.HttpRule{
				Hostnames: []string{"*"}, Ports: &ateapiv1.Ports{All: &ateapiv1.AllPorts{}},
			}}})
			Expect(c.EgressAllowed(ws, "a", "github.com", 443)).To(BeTrue())
			if op == "StartSandbox" {
				_, err = d.StartSandbox(ctx, ws, "a")
			} else {
				_, err = d.ResumeSandbox(ctx, ws, "a")
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(c.EgressAllowed(ws, "a", "github.com", 443)).To(BeFalse())
			Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
			_, err = d.WaitPhase(ctx, ws, "a", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
		}

		By("a policy that another writer deleted")
		_, err = d.StopSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		c.SetEgressPolicy(ws, "a", nil)
		_, err = d.StartSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
		Expect(c.Calls("ResumeWithoutEgressPolicy")).To(BeZero())
	})

	It("gives an actor that already exists the policy on a repeated create", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		c.SetEgressPolicy(ws, "a", nil)
		_, err := d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
	})

	It("gives a restored actor the policy before it runs", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "src")
		snap, err := d.SnapshotSandbox(ctx, ws, "src")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("copy"))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "copy", gw, gwPort)).To(BeTrue())
		Expect(c.EgressAllowed(ws, "copy", "github.com", 80)).To(BeFalse())
		Expect(c.Calls("ResumeWithoutEgressPolicy")).To(BeZero())
	})

	It("leaves the actor suspended when the policy cannot be written", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		_, err := d.StopSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		c.SetEgressPolicy(ws, "a", nil)
		c.FailPolicyWrites(1)
		resumes := c.Calls("ResumeActor")
		_, err = d.StartSandbox(ctx, ws, "a")
		Expect(errors.Is(err, execution.ErrUnavailable)).To(BeTrue(), "got %v", err)
		Expect(c.Calls("ResumeActor")).To(Equal(resumes), "no resume without the policy")
		Expect(c.State(ws, "a")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))

		By("a retry writes the policy and resumes")
		_, err = d.StartSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
	})

	It("writes one rule per gateway host with all of its ports, in a fixed order", func(ctx SpecContext) {
		d, c := newDriver(substrate.Config{GatewayRoutes: []substrate.GatewayRoute{
			{Host: "mcp.koinon.svc.cluster.local", Port: 8080},
			{Host: gw, Port: 9090},
			{Host: gw, Port: gwPort},
			{Host: gw, Port: gwPort},
		}})
		ready(ctx, d, "a")
		rules := c.EgressPolicy(ws, "a").GetRules()
		Expect(rules).To(HaveLen(2))
		Expect(rules[0].GetHttp().GetHostnames()).To(Equal([]string{gw}))
		Expect(rules[0].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{int32(gwPort), 9090}))
		Expect(rules[1].GetHttp().GetHostnames()).To(Equal([]string{"mcp.koinon.svc.cluster.local"}))
		Expect(c.EgressAllowed(ws, "a", gw, 9090)).To(BeTrue())
		Expect(c.EgressAllowed(ws, "a", "mcp.koinon.svc.cluster.local", 8080)).To(BeTrue())
		Expect(c.EgressAllowed(ws, "a", "mcp.koinon.svc.cluster.local", 9090)).To(BeFalse())
	})

	Context("without gateway routes", func() {
		BeforeEach(func() {
			noGatewayRoutes = true
			DeferCleanup(func() { noGatewayRoutes = false })
		})

		It("creates, restores, starts and resumes nothing", func(ctx SpecContext) {
			d, c := newDriver()
			_, err := d.CreateSandbox(ctx, ws, spec("a"))
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			Expect(c.Calls("CreateActor")).To(BeZero())
			_, err = d.RestoreSandbox(ctx, ws, execution.SnapshotRef{ID: "x", Digest: "y"}, spec("b"))
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			_, err = d.StartSandbox(ctx, ws, "a")
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			_, err = d.ResumeSandbox(ctx, ws, "a")
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			Expect(c.Calls("ResumeActor")).To(BeZero())
		})
	})
})

var _ = DescribeTable("ValidateGatewayRoutes",
	func(routes []substrate.GatewayRoute, ok bool) {
		err := substrate.ValidateGatewayRoutes(routes)
		if ok {
			Expect(err).NotTo(HaveOccurred())
		} else {
			Expect(err).To(HaveOccurred())
		}
	},
	Entry("a cluster DNS name", []substrate.GatewayRoute{{Host: "koinon-gateway.koinon.svc.cluster.local", Port: 8080}}, true),
	Entry("a short name", []substrate.GatewayRoute{{Host: "gateway", Port: 80}}, true),
	Entry("no route", []substrate.GatewayRoute(nil), false),
	Entry("an IPv4 address", []substrate.GatewayRoute{{Host: "10.0.0.1", Port: 8080}}, false),
	Entry("an IPv6 address", []substrate.GatewayRoute{{Host: "::1", Port: 8080}}, false),
	Entry("a bracketed IPv6 address", []substrate.GatewayRoute{{Host: "[::1]", Port: 8080}}, false),
	Entry("upper case", []substrate.GatewayRoute{{Host: "Gateway.svc", Port: 8080}}, false),
	Entry("a trailing dot", []substrate.GatewayRoute{{Host: "gateway.svc.", Port: 8080}}, false),
	Entry("a wildcard", []substrate.GatewayRoute{{Host: "*.svc", Port: 8080}}, false),
	Entry("a port in the host", []substrate.GatewayRoute{{Host: "gateway:8080", Port: 8080}}, false),
	Entry("an empty label", []substrate.GatewayRoute{{Host: "gateway..svc", Port: 8080}}, false),
	Entry("a label that starts with a hyphen", []substrate.GatewayRoute{{Host: "-gateway.svc", Port: 8080}}, false),
	Entry("port 0", []substrate.GatewayRoute{{Host: "gateway", Port: 0}}, false),
	Entry("a port over 65535", []substrate.GatewayRoute{{Host: "gateway", Port: 70000}}, false),
	Entry("17 ports on one host", func() []substrate.GatewayRoute {
		var r []substrate.GatewayRoute
		for p := range uint32(17) {
			r = append(r, substrate.GatewayRoute{Host: "gateway", Port: 8000 + p})
		}
		return r
	}(), false),
)
