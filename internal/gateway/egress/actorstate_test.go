// SPDX-License-Identifier: Apache-2.0

package egress_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

var _ = Describe("the actor-only applier (issue #1489)", func() {
	a := gateway.EgressActor{Atespace: "koinon-sandboxes", Name: "a1", UID: "uid-1", Org: "acme", Agent: "helper", Run: "r1"}
	key := actortoken.SubstrateActor{Atespace: "koinon-sandboxes", Name: "a1", UID: "uid-1"}

	It("fills the table from each revision and removes an actor the next revision drops", func() {
		t := &route.ActorTable{}
		s := egress.ActorState{Actors: t}
		Expect(s.ApplyEgress(context.Background(), gateway.EgressState{EgressActors: []gateway.EgressActor{a}})).To(Succeed())
		got, ok := t.Lookup(key)
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal(actortoken.Actor{Org: "acme", Agent: "helper", Run: "r1"}))
		Expect(s.ApplyEgress(context.Background(), gateway.EgressState{})).To(Succeed())
		_, ok = t.Lookup(key)
		Expect(ok).To(BeFalse())
	})

	It("skips a bad actor row and keeps the good actors (issue #1485)", func() {
		t := &route.ActorTable{}
		s := egress.ActorState{Actors: t}
		bad := a
		bad.UID = "koinon-test-uid-bad"
		bad.Agent = ""
		Expect(s.ApplyEgress(context.Background(), gateway.EgressState{EgressActors: []gateway.EgressActor{a, bad}})).NotTo(Succeed())
		Expect(t.Len()).To(Equal(1))
		_, ok := t.Lookup(key)
		Expect(ok).To(BeTrue())
	})

	It("drops an identity that two rows give, and keeps the others", func() {
		t := &route.ActorTable{}
		s := egress.ActorState{Actors: t}
		other := a
		other.UID = "koinon-test-uid-other"
		Expect(s.ApplyEgress(context.Background(), gateway.EgressState{EgressActors: []gateway.EgressActor{a, other, a}})).NotTo(Succeed())
		Expect(t.Len()).To(Equal(1))
		_, ok := t.Lookup(key)
		Expect(ok).To(BeFalse())
	})
})
