// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

var _ = Describe("EgressState.ActorLookup (issue #1386)", func() {
	acme := gateway.EgressActor{Atespace: "ws-acme", Name: "ka-acme-0a1b2c3d", UID: "uid-1", Org: "acme", Agent: "a1", Run: "r1"}

	It("maps the Substrate identity of each actor to the org, agent and run it works for", func() {
		next := acme
		next.UID = "uid-2"
		next.Run = ""
		got, err := gateway.EgressState{EgressActors: []gateway.EgressActor{acme, next}}.ActorLookup()
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[actortoken.SubstrateActor]actortoken.Actor{
			{Atespace: "ws-acme", Name: "ka-acme-0a1b2c3d", UID: "uid-1"}: {Org: "acme", Agent: "a1", Run: "r1"},
			{Atespace: "ws-acme", Name: "ka-acme-0a1b2c3d", UID: "uid-2"}: {Org: "acme", Agent: "a1"},
		}))
		claims := actortoken.SubstrateClaims{Atespace: "ws-acme", ActorName: "ka-acme-0a1b2c3d", ActorUID: "uid-1"}
		Expect(got[claims.Key()]).To(Equal(actortoken.Actor{Org: "acme", Agent: "a1", Run: "r1"}))
	})

	It("is empty for a State without actors", func() {
		got, err := gateway.EgressState{}.ActorLookup()
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("drops an identity that a bad row also claims", func() {
		bad := acme
		bad.Org = ""
		got, err := gateway.EgressState{EgressActors: []gateway.EgressActor{acme, bad}}.ActorLookup()
		Expect(err).To(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	DescribeTable("skips an actor row it cannot trust and keeps the good rows (issue #1485)",
		func(mut func(*gateway.EgressActor), dup bool) {
			bad := acme
			mut(&bad)
			good := acme
			good.UID = "uid-good"
			actors := []gateway.EgressActor{bad, good}
			if dup {
				actors = []gateway.EgressActor{bad, good, bad, bad}
			}
			got, err := gateway.EgressState{EgressActors: actors}.ActorLookup()
			Expect(err).To(HaveOccurred())
			Expect(got).To(HaveLen(1))
			Expect(got).To(HaveKey(actortoken.SubstrateActor{Atespace: "ws-acme", Name: "ka-acme-0a1b2c3d", UID: "uid-good"}))
		},
		Entry("no workspace", func(a *gateway.EgressActor) { a.Atespace = "" }, false),
		Entry("no name", func(a *gateway.EgressActor) { a.Name = "" }, false),
		Entry("no UID", func(a *gateway.EgressActor) { a.UID = "" }, false),
		Entry("no org", func(a *gateway.EgressActor) { a.Org = "" }, false),
		Entry("no agent", func(a *gateway.EgressActor) { a.Agent = "" }, false),
		Entry("the same identity twice or more: all its rows are dropped", func(*gateway.EgressActor) {}, true),
	)
})
