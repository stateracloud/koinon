// SPDX-License-Identifier: Apache-2.0

package gatewayplan_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

var _ = Describe("Substrate actors (issue #1386)", func() {
	withActor := func(a gatewayplan.Agent, uid string) gatewayplan.Agent {
		a.Actor = &gatewayplan.Actor{Atespace: "ws-acme", Name: "ka-" + a.Ref.Name + "-0a1b2c3d", UID: uid}
		return a
	}

	It("puts the actor of each active agent in the state, with its org and agent", func() {
		in := gatewayplan.Input{Now: t0, Orgs: []gatewayplan.Org{acme}, Agents: []gatewayplan.Agent{
			withActor(agent(acme, 1, "a1"), "uid-1"), agent(acme, 2, "a2"),
		}}
		pl := settle(newPlanner(0).Begin(in), nil, nil, &fakeLookups{})
		Expect(pl.State.EgressActors).To(Equal([]gateway.EgressActor{{
			Atespace: "ws-acme", Name: "ka-a1-0a1b2c3d", UID: "uid-1", Org: "acme", Agent: "a1",
		}}))
	})

	It("leaves out the actor of an agent that is not active", func() {
		a := withActor(agent(acme, 1, "a1"), "uid-1")
		a.Active = false
		in := gatewayplan.Input{Now: t0, Orgs: []gatewayplan.Org{acme}, Agents: []gatewayplan.Agent{a}}
		pl := settle(newPlanner(0).Begin(in), nil, nil, &fakeLookups{})
		Expect(pl.State.EgressActors).To(BeEmpty())
	})
})
