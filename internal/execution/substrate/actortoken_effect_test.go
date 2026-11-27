// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/route"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// The actor token effect of the egress rule (issue #1386): Substrate puts a JWT for the actor in the header, so
// the guest holds no token.
var _ = Describe("the actor token effect of the egress rule", func() {
	const gw, gwPort, egressPort = fakeate.GatewayHost, fakeate.GatewayPort, fakeate.EgressPort
	grant := execution.GatewayAccess{CodeHostEgress: true}

	It("repeats the header and audience of the gateway", func() {
		Expect(substrate.ActorTokenHeader).To(Equal(route.TokenHeader))
		Expect(substrate.ActorTokenAudience).To(Equal(actortoken.Audience))
	})

	It("gives the egress rule one actor_jwt header and the model and MCP rules none", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")

		rules := c.EgressPolicy(ws, "a").GetRules()
		Expect(rules).To(HaveLen(2), "the egress route is its own rule, on the same host with another port")
		Expect(rules[0].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{int32(gwPort)}))
		Expect(rules[0].GetHttp().GetEffects()).To(BeNil(), "the model and MCP rules hold no effect")
		Expect(rules[1].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{int32(egressPort)}))
		hdrs := rules[1].GetHttp().GetEffects().GetReplaceHeaders()
		Expect(hdrs).To(HaveLen(1))
		Expect(hdrs[0].GetHeader()).To(Equal("Koinon-Actor-Token"))
		Expect(hdrs[0].GetPrefix()).To(BeEmpty())
		Expect(hdrs[0].GetCredentialUri()).To(BeEmpty())
		Expect(hdrs[0].GetActorJwt().GetAudiences()).To(Equal([]string{"koinon-gateway"}))
		Expect(hdrs[0].GetActorJwt().GetExpirationSeconds()).To(Equal(int64(900)))

		By("the rule that decides for each port is the one with the effect")
		Expect(c.EgressEffects(ws, "a", gw, egressPort)).NotTo(BeNil())
		Expect(c.EgressEffects(ws, "a", gw, gwPort)).To(BeNil())
		Expect(c.EgressEffects(ws, "a", "github.com", 443)).To(BeNil())
	})

	It("gives an actor without a code-host grant no effect at all", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		for _, r := range c.EgressPolicy(ws, "a").GetRules() {
			Expect(r.GetHttp().GetEffects()).To(BeNil())
		}
	})

	It("writes the same rules for the same routes, in a fixed order", func(ctx SpecContext) {
		d, c := newDriver(substrate.Config{
			GatewayRoutes:       []substrate.GatewayRoute{{Host: "mcp.koinon.svc.cluster.local", Port: 8080}, {Host: gw, Port: gwPort}},
			CodeHostEgressRoute: &substrate.GatewayRoute{Host: gw, Port: egressPort},
		})
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")
		rules := c.EgressPolicy(ws, "a").GetRules()
		Expect(rules).To(HaveLen(3))
		Expect(rules[0].GetHttp().GetHostnames()).To(Equal([]string{gw}))
		Expect(rules[0].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{int32(gwPort)}))
		Expect(rules[1].GetHttp().GetPorts().GetNumbers()).To(Equal([]int32{int32(egressPort)}))
		Expect(rules[2].GetHttp().GetHostnames()).To(Equal([]string{"mcp.koinon.svc.cluster.local"}))
		Expect(c.EgressEffects(ws, "a", "mcp.koinon.svc.cluster.local", 8080)).To(BeNil())
	})

	It("updates a policy that has the egress rule without the effect in place", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		ready(ctx, d, "a")
		_, err := d.StopSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())

		By("a policy of the earlier driver: the same hosts and ports, no effect")
		c.SetEgressPolicy(ws, "a", []*ateapiv1.EgressRule{{Http: &ateapiv1.HttpRule{
			Hostnames: []string{gw}, Ports: &ateapiv1.Ports{Numbers: []int32{int32(gwPort), int32(egressPort)}},
		}}})
		before := c.PolicyWrites(ws, "a")
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		Expect(c.PolicyWrites(ws, "a")).To(Equal(before+1), "written in place")
		Expect(c.Calls("CreateActor")).To(Equal(1), "no new actor")
		Expect(c.EgressEffects(ws, "a", gw, egressPort)).NotTo(BeNil())

		By("an equal policy is left alone")
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		Expect(c.PolicyWrites(ws, "a")).To(Equal(before + 1))

		By("a changed effect is put back too")
		c.SetEgressPolicy(ws, "a", []*ateapiv1.EgressRule{
			{Http: &ateapiv1.HttpRule{Hostnames: []string{gw}, Ports: &ateapiv1.Ports{Numbers: []int32{int32(gwPort)}}}},
			{Http: &ateapiv1.HttpRule{Hostnames: []string{gw}, Ports: &ateapiv1.Ports{Numbers: []int32{int32(egressPort)}}, Effects: &ateapiv1.HttpRuleEffects{
				ReplaceHeaders: []*ateapiv1.CredentialHeader{{Header: "Koinon-Actor-Token", ActorJwt: &ateapiv1.ActorJwtSource{
					Audiences: []string{"koinon-gateway"}, ExpirationSeconds: 3600,
				}}},
			}}},
		})
		Expect(d.SetGatewayAccess(ctx, ws, "a", grant)).To(Succeed())
		Expect(c.EgressEffects(ws, "a", gw, egressPort).GetReplaceHeaders()[0].GetActorJwt().GetExpirationSeconds()).To(Equal(int64(900)))
	})
})

// The fake enforces what ateapi enforces for these rules (Substrate 20778a6d, apivalidation and ateapi.proto).
// The actor JWT may go only to the gateway egress listener (issue #1386, lab test of 2026-11-27): atenet-egress
// also replaces the header on an https (MITM) rule, and then the upstream host sees the token.
var _ = Describe("the driver refuses the actor token effect on any other rule", func() {
	egress := &substrate.GatewayRoute{Host: fakeate.GatewayHost, Port: fakeate.EgressPort}
	jwt := func() *ateapiv1.HttpRuleEffects {
		return &ateapiv1.HttpRuleEffects{ReplaceHeaders: []*ateapiv1.CredentialHeader{{
			Header: substrate.ActorTokenHeader,
			ActorJwt: &ateapiv1.ActorJwtSource{
				Audiences: []string{substrate.ActorTokenAudience}, ExpirationSeconds: substrate.ActorTokenLifetimeSeconds,
			},
		}}}
	}
	httpRule := func(host string, port int32, fx *ateapiv1.HttpRuleEffects) *ateapiv1.EgressRule {
		return &ateapiv1.EgressRule{Http: &ateapiv1.HttpRule{
			Hostnames: []string{host}, Ports: &ateapiv1.Ports{Numbers: []int32{port}}, Effects: fx,
		}}
	}
	gwPort := int32(fakeate.GatewayPort)
	egPort := int32(fakeate.EgressPort)

	It("accepts the effect on the one gateway egress rule", func() {
		Expect(substrate.CheckActorTokenEffects([]*ateapiv1.EgressRule{
			httpRule(fakeate.GatewayHost, gwPort, nil),
			httpRule(fakeate.GatewayHost, egPort, jwt()),
		}, egress)).To(Succeed())
	})

	DescribeTable("refuses",
		func(rules []*ateapiv1.EgressRule, eg *substrate.GatewayRoute) {
			Expect(substrate.CheckActorTokenEffects(rules, eg)).To(HaveOccurred())
		},
		Entry("an https (MITM) rule with the effect", []*ateapiv1.EgressRule{
			{Https: &ateapiv1.HttpsRule{Hostnames: []string{"forge.example.test"}, Ports: &ateapiv1.Ports{Numbers: []int32{443}}, Effects: jwt()}},
		}, egress),
		Entry("the effect on another host", []*ateapiv1.EgressRule{httpRule("forge.example.test", 80, jwt())}, egress),
		Entry("the effect on the model and MCP port of the gateway", []*ateapiv1.EgressRule{httpRule(fakeate.GatewayHost, gwPort, jwt())}, egress),
		Entry("the effect on all ports of the gateway host", []*ateapiv1.EgressRule{{Http: &ateapiv1.HttpRule{
			Hostnames: []string{fakeate.GatewayHost}, Ports: &ateapiv1.Ports{All: &ateapiv1.AllPorts{}}, Effects: jwt(),
		}}}, egress),
		Entry("the effect with no egress route", []*ateapiv1.EgressRule{httpRule(fakeate.GatewayHost, egPort, jwt())}, nil),
		Entry("two rules with the effect", []*ateapiv1.EgressRule{
			httpRule(fakeate.GatewayHost, egPort, jwt()), httpRule(fakeate.GatewayHost, egPort, jwt()),
		}, egress),
	)

	It("does not write a policy that breaks the rule", func(ctx SpecContext) {
		d, c := newDriver()
		Expect(d.SetGatewayAccess(ctx, ws, "a", execution.GatewayAccess{CodeHostEgress: true})).To(Succeed())
		ready(ctx, d, "a")
		for _, r := range c.EgressPolicy(ws, "a").GetRules() {
			Expect(r.GetHttps()).To(BeNil(), "the driver writes no https rule")
		}
		Expect(substrate.CheckActorTokenEffects(c.EgressPolicy(ws, "a").GetRules(), egress)).To(Succeed())
	})
})

var _ = Describe("the fake Substrate admission of effects and actor names", func() {
	const gw, gwPort, egressPort = fakeate.GatewayHost, fakeate.GatewayPort, fakeate.EgressPort

	rule := func(port int32, h *ateapiv1.CredentialHeader) *ateapiv1.EgressRule {
		var e *ateapiv1.HttpRuleEffects
		if h != nil {
			e = &ateapiv1.HttpRuleEffects{ReplaceHeaders: []*ateapiv1.CredentialHeader{h}}
		}
		return &ateapiv1.EgressRule{Http: &ateapiv1.HttpRule{Hostnames: []string{gw}, Ports: &ateapiv1.Ports{Numbers: []int32{port}}, Effects: e}}
	}
	jwt := func(aud []string, exp int64) *ateapiv1.CredentialHeader {
		return &ateapiv1.CredentialHeader{Header: "Koinon-Actor-Token", ActorJwt: &ateapiv1.ActorJwtSource{Audiences: aud, ExpirationSeconds: exp}}
	}

	DescribeTable("a policy write",
		func(ctx SpecContext, h *ateapiv1.CredentialHeader, ok bool) {
			d, c := newDriver()
			ready(ctx, d, "a")
			_, err := d.StopSandbox(ctx, ws, "a")
			Expect(err).NotTo(HaveOccurred())
			p := c.EgressPolicy(ws, "a")
			p.Rules = []*ateapiv1.EgressRule{rule(int32(gwPort), nil), rule(int32(egressPort), h)}
			ctl := lastControl
			_, err = ctl.UpdateActorEgressPolicy(ctx, &ateapiv1.UpdateActorEgressPolicyRequest{
				Actor: &ateapiv1.ObjectRef{Atespace: ws, Name: "a"}, EgressPolicy: p,
			})
			if ok {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}
		},
		Entry("the Koinon effect", jwt([]string{"koinon-gateway"}, 900), true),
		Entry("the lowest lifetime", jwt([]string{"x"}, 300), true),
		Entry("the highest lifetime", jwt([]string{"x"}, 3600), true),
		Entry("a lifetime under 300", jwt([]string{"x"}, 299), false),
		Entry("a lifetime over 3600", jwt([]string{"x"}, 3601), false),
		Entry("no lifetime", jwt([]string{"x"}, 0), false),
		Entry("no audience", jwt(nil, 900), false),
		Entry("an empty audience", jwt([]string{""}, 900), false),
		Entry("a repeated audience", jwt([]string{"x", "x"}, 900), false),
		Entry("17 audiences", jwt([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17"}, 900), false),
		Entry("a bad header name", &ateapiv1.CredentialHeader{Header: "Bad Header", ActorJwt: &ateapiv1.ActorJwtSource{Audiences: []string{"x"}, ExpirationSeconds: 900}}, false),
		Entry("no source", &ateapiv1.CredentialHeader{Header: "Koinon-Actor-Token"}, false),
		Entry("two sources", &ateapiv1.CredentialHeader{Header: "X", CredentialUri: "ate-secret://a/b/c", ActorJwt: &ateapiv1.ActorJwtSource{Audiences: []string{"x"}, ExpirationSeconds: 900}}, false),
	)

	It("admits two rules on one host with disjoint ports, and refuses a tie", func(ctx SpecContext) {
		d, c := newDriver()
		ready(ctx, d, "a")
		ctl := lastControl
		p := c.EgressPolicy(ws, "a")
		upd := func(rules ...*ateapiv1.EgressRule) error {
			q := c.EgressPolicy(ws, "a")
			q.Rules = rules
			_, err := ctl.UpdateActorEgressPolicy(ctx, &ateapiv1.UpdateActorEgressPolicyRequest{
				Actor: &ateapiv1.ObjectRef{Atespace: ws, Name: "a"}, EgressPolicy: q,
			})
			return err
		}
		Expect(p).NotTo(BeNil())
		Expect(upd(rule(int32(gwPort), nil), rule(int32(egressPort), jwt([]string{"x"}, 900)))).To(Succeed())
		Expect(upd(rule(int32(gwPort), nil), rule(int32(gwPort), jwt([]string{"x"}, 900)))).NotTo(Succeed())
	})

	It("refuses an actor name that is not a DNS-1123 label of at most 63 characters", func(ctx SpecContext) {
		d, _ := newDriver()
		_, err := d.CreateSandbox(ctx, ws, spec(strings64()))
		Expect(err).To(MatchError(execution.ErrInvalid))
		ready(ctx, d, strings63())
	})
})

func strings63() string { return strings.Repeat("a", 63) }
func strings64() string { return strings.Repeat("a", 64) }

// The actor name (issue #1386): "<sandbox>-<8 hex>", new for each call, and found through the adapter.
var _ = Describe("the actor name", func() {
	It("is the sandbox name, a dash and 8 lowercase hex characters, and new each time", func() {
		d, _ := newDriver()
		re := regexp.MustCompile(`^kt-0123456789abcdef-[0-9a-f]{8}$`)
		seen := map[string]bool{}
		for range 200 {
			n := d.NewActorName("kt-0123456789abcdef")
			Expect(n).To(MatchRegexp(re.String()))
			Expect(seen).NotTo(HaveKey(n))
			seen[n] = true
		}
	})

	It("fits the limit of Substrate for a Koinon sandbox name", func() {
		d, _ := newDriver()
		Expect(len(d.NewActorName("kt-0123456789abcdef"))).To(BeNumerically("<=", substrate.MaxActorNameLength))
	})

	It("is found through the adapter of the production wiring, and not on a backend without it", func() {
		d, _ := newDriver()
		n, ok := execution.ActorNamerOf(execution.WithoutEgress(d))
		Expect(ok).To(BeTrue())
		Expect(n).To(BeIdenticalTo(d))
		Expect(n.NewActorName("a")).To(MatchRegexp(`^a-[0-9a-f]{8}$`))
		_, ok = execution.ActorNamerOf(execution.WithoutEgress(struct{ execution.Compute }{}))
		Expect(ok).To(BeFalse())
	})
})
