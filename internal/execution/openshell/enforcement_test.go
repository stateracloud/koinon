// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
)

var _ = Describe("the enforcement mode read from a sandbox's policy (ADR 0045, #926)", func() {
	policy := func(eps ...*sandboxv1.NetworkEndpoint) *sandboxv1.SandboxPolicy {
		return &sandboxv1.SandboxPolicy{NetworkPolicies: map[string]*sandboxv1.NetworkPolicyRule{"r": {Endpoints: eps}}}
	}
	ep := func(proto string, m sandboxv1.NetworkEnforcementMode) *sandboxv1.NetworkEndpoint {
		return &sandboxv1.NetworkEndpoint{Host: "h", Protocol: proto, Enforcement: m}
	}
	enforce := sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE

	It("is unknown without a policy", func() {
		Expect(openshell.EnforcementOf(nil)).To(Equal(execution.EnforcementUnknown))
	})
	It("is enforce when every inspected endpoint enforces", func() {
		Expect(openshell.EnforcementOf(policy(ep("rest", enforce), ep("", 0)))).To(Equal(execution.EnforcementEnforce))
	})
	It("is audit when any inspected endpoint does not enforce", func() {
		Expect(openshell.EnforcementOf(policy(ep("rest", enforce), ep("rest", 0)))).To(Equal(execution.EnforcementAudit))
	})
	It("is none when no endpoint is inspected", func() {
		Expect(openshell.EnforcementOf(policy(ep("", 0)))).To(Equal(execution.EnforcementNone))
	})
})
