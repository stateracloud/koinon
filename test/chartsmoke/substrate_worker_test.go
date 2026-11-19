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
var _ = Describe("substrate worker namespace", func() {
	It("renders the worker policies and NetworkPolicy", func(ctx SpecContext) {
		r := helmRender(ctx, filepath.Join(chartDir, "ci", "substrate-values.yaml"))
		// Pods, CONNECT and the installer-written kinds: three policies, three bindings.
		Expect(r.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicy", 3))
		Expect(r.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicyBinding", 3))
		Expect(r.Ignored).To(HaveKeyWithValue("NetworkPolicy", 7))
	})

	It("renders no WorkerPool with the OpenShell driver", func(ctx SpecContext) {
		r := helmRender(ctx, filepath.Join(chartDir, "ci", "external-standalone-values.yaml"))
		Expect(r.Ignored).NotTo(HaveKey("WorkerPool"))
	})
})
