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
		Expect(c.Calls("ResumeActor")).To(Equal(2), "the first boot, and the resume after its seal (issue #1481)")
		Expect(c.Calls("ResumeWithoutEgressPolicy")).To(BeZero(), "the actor ran before its policy was written")

		p := c.EgressPolicy(ws, "a")
		Expect(p).NotTo(BeNil())
		Expect(p.GetMetadata().GetName()).To(Equal("default"))
		Expect(p.GetRules()).To(HaveLen(1))
		Expect(p.GetRules()[0].GetHttp().GetHostnames()).To(Equal([]string{gw}))
		Expect(p.GetRules()[0].GetHttp().GetEffects().GetReplaceHeaders()).To(HaveLen(1), "the rule holds only the actor JWT effect")
		Expect(p.GetRules()[0].GetHttp().GetEffects().GetReplaceHeaders()[0].GetCredentialUri()).To(BeEmpty(), "and no credential")
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

		By("the name is not used again (issue #1092), and a new actor under a new name gets a new policy")
		_, err = d.CreateSandbox(ctx, ws, spec("a"))
		Expect(err).To(MatchError(execution.ErrNameReused))
		ready(ctx, d, "a2")
		Expect(c.EgressAllowed(ws, "a2", gw, gwPort)).To(BeTrue())
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

	It("writes one rule per gateway host and port, in a fixed order", func(ctx SpecContext) {
		d, c := newDriver(substrate.Config{GatewayRoutes: []substrate.GatewayRoute{
			substrate.MCPRoute("mcp.koinon.svc.cluster.local", 8080),
			substrate.MCPRoute(gw, 9090),
			substrate.ModelRoute(gw, gwPort),
		}})
		ready(ctx, d, "a")
		rules := c.EgressPolicy(ws, "a").GetRules()
		Expect(rules).To(HaveLen(3))
		Expect(rules[0].GetHttp().GetHostnames()).To(Equal([]string{gw}))
		Expect(rules[0].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{int32(gwPort)}))
		Expect(rules[1].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{9090}))
		Expect(rules[2].GetHttp().GetHostnames()).To(Equal([]string{"mcp.koinon.svc.cluster.local"}))
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

// The egress listener goes only to an actor whose agent has a code-host grant (issue #1444): the same least
// privilege as the OpenShell policy entry of that listener. The access is a per-actor policy object, not part of
// the template, so a change never replaces the sandbox.
var _ = Describe("the egress listener in the actor egress policy", func() {
	const gw, gwPort, egressPort = fakeate.GatewayHost, fakeate.GatewayPort, fakeate.EgressPort
	grant := execution.GatewayAccess{CodeHostEgress: true}
	noGrant := execution.GatewayAccess{}

	It("gives an actor without a code-host grant no route to the egress listener", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue(), "the model and MCP listeners")
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse(), "no access recorded: the driver fails closed")

		Expect(d.SetGatewayAccess(ctx, ws, "a", noGrant)).To(Succeed())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse())
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
	})

	It("gives an actor with a code-host grant the egress listener before it first runs", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed(), "no actor yet: the access is only recorded")
		Expect(c.EgressPolicy(ws, "a")).To(BeNil())
		ready(ctx, d, "a")
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeTrue())
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
		Expect(c.PolicyWrites(ws, "a")).To(Equal(1), "written once, before the first resume")
		Expect(c.Calls("ResumeWithoutEgressPolicy")).To(BeZero())
	})

	// Issue #1386: the reconciler drops the record of an old actor name, so that no later actor gets it.
	It("forgets the recorded access of a name without a call to the control plane", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		calls := c.Calls("DeleteActor")
		d.ForgetGatewayAccess(ws, "a")
		Expect(c.Calls("DeleteActor")).To(Equal(calls))
		ready(ctx, d, "a")
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse(), "no access recorded: the driver fails closed")
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
	})

	It("takes the egress listener away from a running actor when the grant goes, without a replacement", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")
		before, err := d.GetSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeTrue())

		Expect(d.SetGatewayAccess(ctx, ws, "a", noGrant)).To(Succeed())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse(), "the removal applies at once, in place")
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
		after, err := d.GetSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(after.ID).To(Equal(before.ID), "the same actor")
		Expect(after.Phase).To(Equal(execution.PhaseReady), "it keeps running")
		Expect(c.Calls("DeleteActor")).To(BeZero())
		Expect(c.Calls("CreateActor")).To(Equal(1))
		Expect(c.Calls("ResumeActor")).To(Equal(2), "no restart: the first boot and its seal only")

		By("a new grant applies in place too")
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeTrue())
		Expect(c.Calls("CreateActor")).To(Equal(1))
	})

	It("does not bring back an old policy on a resume or a restore", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")
		snap, err := d.SnapshotSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred(), "taken while the actor has the egress listener")
		_, err = d.WaitPhase(ctx, ws, "a", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred(), "the snapshot resumes the actor")

		By("a resume writes the access of now, not the one the actor had when it stopped")
		_, err = d.StopSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(d.SetGatewayAccess(ctx, ws, "a", noGrant)).To(Succeed())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse(), "applied to the suspended actor in place")
		// A stale writer puts the old policy back while the actor is suspended.
		c.SetEgressPolicy(ws, "a", []*ateapiv1.EgressRule{{Http: &ateapiv1.HttpRule{
			Hostnames: []string{gw}, Ports: &ateapiv1.Ports{Numbers: []int32{int32(gwPort), int32(egressPort)}},
		}}})
		_, err = d.ResumeSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse())
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())

		By("a restore of a snapshot taken with the grant gives the new actor only its own access")
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("copy"))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "copy", gw, egressPort)).To(BeFalse(), "no access recorded for the copy")
		Expect(c.EgressAllowed(ws, "copy", gw, gwPort)).To(BeTrue())
		Expect(c.Calls("ResumeWithoutEgressPolicy")).To(BeZero())

		By("a restore with a grant recorded first gives it the egress listener before it runs")
		Expect(d.SetGatewayAccess(ctx, ws, "copy2", grant)).To(Succeed())
		_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("copy2"))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.EgressAllowed(ws, "copy2", gw, egressPort)).To(BeTrue())
	})

	It("gives no actor an egress listener on an install without one", func(ctx SpecContext) {
		d, c := newDriver(substrate.Config{GatewayRoutes: gatewayRoutes})
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")
		Expect(c.EgressPolicy(ws, "a").GetRules()).To(HaveLen(1))
		Expect(c.EgressPolicy(ws, "a").GetRules()[0].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{int32(gwPort)}))
	})

	It("keeps the old policy and returns the error when the change cannot be written", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")
		c.FailPolicyWrites(1)
		err := d.SetGatewayAccess(ctx, ws, "a", noGrant)
		Expect(errors.Is(err, execution.ErrUnavailable)).To(BeTrue(), "got %v", err)
		By("the reconciler's next pass applies it")
		Expect(d.SetGatewayAccess(ctx, ws, "a", noGrant)).To(Succeed())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse())
	})

	It("forgets the grant with the sandbox, so a new sandbox of that name has no egress listener until it is set", func(ctx SpecContext) {
		// The ledger refuses the name again (issue #1092). A ledger that forgets every name shows the second
		// guard: the grant goes with the sandbox, also when a name does come back.
		cfg := specConfig()
		cfg.ActorNames = forgetfulNames{}
		d, c := newDriver(cfg)
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeTrue())
		_, err := d.DeleteSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		ready(ctx, d, "a")
		Expect(c.EgressAllowed(ws, "a", gw, gwPort)).To(BeTrue())
		Expect(c.EgressAllowed(ws, "a", gw, egressPort)).To(BeFalse(), "no access recorded: fail closed")
	})

	It("is reached through the adapter of the production wiring", func() {
		d, _ := newDriver()
		Expect(execution.GatewayAccessSetterOf(execution.WithoutEgress(d))).To(BeIdenticalTo(d))
		Expect(execution.GatewayAccessSetterOf(struct{}{})).To(BeNil())
	})
})

var _ = DescribeTable("ValidateGatewayRoutes",
	func(routes []substrate.GatewayRoute, ok bool) {
		err := substrate.ValidateGatewayRoutes(routes, nil)
		if ok {
			Expect(err).NotTo(HaveOccurred())
		} else {
			Expect(err).To(HaveOccurred())
		}
	},
	Entry("a cluster DNS name", []substrate.GatewayRoute{substrate.ModelRoute("koinon-gateway.koinon.svc.cluster.local", 8080)}, true),
	Entry("a short name", []substrate.GatewayRoute{substrate.MCPRoute("gateway", 80)}, true),
	Entry("model and MCP on one host", []substrate.GatewayRoute{substrate.ModelRoute("gateway", 8080), substrate.MCPRoute("gateway", 8081)}, true),
	Entry("no route", []substrate.GatewayRoute(nil), false),
	Entry("an IPv4 address", []substrate.GatewayRoute{substrate.ModelRoute("10.0.0.1", 8080)}, false),
	Entry("an IPv6 address", []substrate.GatewayRoute{substrate.ModelRoute("::1", 8080)}, false),
	Entry("a bracketed IPv6 address", []substrate.GatewayRoute{substrate.ModelRoute("[::1]", 8080)}, false),
	Entry("upper case", []substrate.GatewayRoute{substrate.ModelRoute("Gateway.svc", 8080)}, false),
	Entry("a trailing dot", []substrate.GatewayRoute{substrate.ModelRoute("gateway.svc.", 8080)}, false),
	Entry("a wildcard", []substrate.GatewayRoute{substrate.ModelRoute("*.svc", 8080)}, false),
	Entry("a port in the host", []substrate.GatewayRoute{substrate.ModelRoute("gateway:8080", 8080)}, false),
	Entry("an empty label", []substrate.GatewayRoute{substrate.ModelRoute("gateway..svc", 8080)}, false),
	Entry("a label that starts with a hyphen", []substrate.GatewayRoute{substrate.ModelRoute("-gateway.svc", 8080)}, false),
	Entry("port 0", []substrate.GatewayRoute{substrate.ModelRoute("gateway", 0)}, false),
	Entry("a port over 65535", []substrate.GatewayRoute{substrate.ModelRoute("gateway", 70000)}, false),
	Entry("two listeners on one host and port", []substrate.GatewayRoute{substrate.ModelRoute("gateway", 8080), substrate.MCPRoute("gateway", 8080)}, false),
	Entry("one listener twice", []substrate.GatewayRoute{substrate.ModelRoute("gateway", 8080), substrate.ModelRoute("gateway", 8080)}, false),
	Entry("no header", []substrate.GatewayRoute{{Host: "gateway", Port: 8080, Audience: substrate.AudienceModel}}, false),
	Entry("no audience", []substrate.GatewayRoute{{Host: "gateway", Port: 8080, Header: "Authorization"}}, false),
	Entry("the Ed25519 audience", []substrate.GatewayRoute{{Host: "gateway", Port: 8080, Header: "Authorization", Audience: "koinon-gateway"}}, false),
)

var _ = It("ValidateGatewayRoutes keeps the egress route apart from the routes of every actor", func() {
	routes := []substrate.GatewayRoute{substrate.ModelRoute("gateway", 8080)}
	Expect(substrate.ValidateGatewayRoutes(routes, ptr(substrate.EgressRoute("gateway", 8081)))).To(Succeed())
	Expect(substrate.ValidateGatewayRoutes(routes, ptr(substrate.EgressRoute("gateway", 8080)))).
		To(MatchError(ContainSubstring("cannot gate it")))
	Expect(substrate.ValidateGatewayRoutes(routes, ptr(substrate.EgressRoute("10.0.0.1", 8081)))).To(HaveOccurred())
	Expect(substrate.ValidateGatewayRoutes(nil, ptr(substrate.EgressRoute("gateway", 8081)))).
		To(HaveOccurred(), "the egress route alone is not enough")
	Expect(substrate.ValidateGatewayRoutes(routes, ptr(substrate.MCPRoute("gateway", 8081)))).
		To(MatchError(ContainSubstring("audience")), "the egress route has the egress audience")
})
