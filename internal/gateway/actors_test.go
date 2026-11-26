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

	DescribeTable("fails closed on an actor it cannot trust",
		func(mut func(*gateway.EgressActor), dup bool) {
			bad := acme
			mut(&bad)
			actors := []gateway.EgressActor{bad}
			if dup {
				actors = append(actors, bad)
			}
			got, err := gateway.EgressState{EgressActors: actors}.ActorLookup()
			Expect(err).To(HaveOccurred())
			Expect(got).To(BeNil())
		},
		Entry("no workspace", func(a *gateway.EgressActor) { a.Atespace = "" }, false),
		Entry("no name", func(a *gateway.EgressActor) { a.Name = "" }, false),
		Entry("no UID", func(a *gateway.EgressActor) { a.UID = "" }, false),
		Entry("no org", func(a *gateway.EgressActor) { a.Org = "" }, false),
		Entry("no agent", func(a *gateway.EgressActor) { a.Agent = "" }, false),
		Entry("the same identity twice", func(*gateway.EgressActor) {}, true),
	)
})
