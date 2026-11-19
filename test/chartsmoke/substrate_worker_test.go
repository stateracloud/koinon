// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The Substrate driver's worker namespace (issue #1089): with execution.driver=substrate the chart
// renders the worker ValidatingAdmissionPolicies, the NetworkPolicy next to
// the components that the smoke suite starts. They are objects for the API server, not containers,
// so this spec renders the chart and checks that each of them reaches the manifest. It needs helm and no Docker. The policies themselves run against
// a real API server in test/escape (make test-admission).
// showOnly returns the helm arguments that render one template with the external database values,
// in a new slice (the shared externalDatabase slice is not changed).
func showOnly(template string) []string {
	return append(append([]string{}, externalDatabase...), "--show-only", template)
}

var _ = Describe("substrate worker namespace", func() {
	It("renders the worker policies and NetworkPolicy", func(ctx SpecContext) {
		// Only the worker templates: the whole render holds other policies, and their count changes
		// with the chart. Pods, CONNECT and the installer-written kinds: three policies, three bindings.
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		policy := helmRenderWith(ctx, values, showOnly("templates/substrate-policy.yaml")...)
		Expect(policy.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicy", 3))
		Expect(policy.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicyBinding", 3))
		netpol := helmRenderWith(ctx, values, showOnly("templates/substrate-networkpolicy.yaml")...)
		Expect(netpol.Ignored).To(HaveKeyWithValue("NetworkPolicy", 1))
	})

	It("renders no WorkerPool with the OpenShell driver", func(ctx SpecContext) {
		r := helmRender(ctx, filepath.Join(chartDir, "ci", "external-standalone-values.yaml"))
		Expect(r.Ignored).NotTo(HaveKey("WorkerPool"))
	})
})
