// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// showOnly returns the helm arguments that render one template with the external database values,
// in a new slice (the shared externalDatabase slice is not changed).
func showOnly(template string) []string {
	return append(append([]string{}, externalDatabase...), "--show-only", template)
}

// The Substrate driver's worker namespace (issue #1089): with execution.driver=substrate the chart
// renders the worker Namespace, the ValidatingAdmissionPolicies and the five NetworkPolicies (workers, control
// plane, default-deny egress, API-only pods, bucket-init Job) next to the components that the smoke suite starts. They are objects for the API server, not
// containers, so this spec renders the chart and checks that each of them reaches the manifest. It needs
// helm and no Docker. The policies themselves run against a real API server in test/escape
// (make test-admission).
var _ = Describe("substrate worker namespace", func() {
	It("renders the worker policies and NetworkPolicy", func(ctx SpecContext) {
		// Only the worker templates: the whole render holds other policies, and their count changes
		// with the chart. Worker pods, control-plane pods, CONNECT and the installer-written kinds: four policies, four bindings.
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		policy := helmRenderWith(ctx, values, showOnly("templates/substrate-policy.yaml")...)
		Expect(policy.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicy", 4))
		Expect(policy.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicyBinding", 4))
		netpol := helmRenderWith(ctx, values, showOnly("templates/substrate-networkpolicy.yaml")...)
		Expect(netpol.Ignored).To(HaveKeyWithValue("NetworkPolicy", 6))
		// The fence of otel-system and podcertificate-controller-system and the telemetry route (issue #1172): seven policies.
		fence := helmRenderWith(ctx, values, showOnly("templates/substrate-fence-networkpolicy.yaml")...)
		Expect(fence.Ignored).To(HaveKeyWithValue("NetworkPolicy", 7))
		namespaces := helmRenderWith(ctx, values, showOnly("templates/substrate-workernamespace.yaml")...)
		Expect(namespaces.Ignored).To(HaveKeyWithValue("Namespace", 1))
	})

	// The default image cache (issue #1170): atelet's one image source is a Zot pull-through registry in
	// Koinon's namespace. The sixth worker NetworkPolicy above is the image-source rule that selects it.
	It("renders the in-cluster image cache by default", func(ctx SpecContext) {
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		cache := helmRenderWith(ctx, values, showOnly("templates/substrate-imagecache.yaml")...)
		Expect(cache.Deployments).To(HaveLen(1))
		Expect(cache.Deployments[0].Spec.Template.Spec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(cache.Ignored).To(HaveKeyWithValue("NetworkPolicy", 1))
		Expect(cache.Ignored).To(HaveKeyWithValue("Service", 1))
	})

	It("renders no WorkerPool with the OpenShell driver", func(ctx SpecContext) {
		r := helmRender(ctx, filepath.Join(chartDir, "ci", "external-standalone-values.yaml"))
		Expect(r.Ignored).NotTo(HaveKey("WorkerPool"))
	})
})

// The worker namespace limits (issue #1161): the LimitRange and the ResourceQuota reach the manifest.
var _ = Describe("substrate worker limits", func() {
	It("renders one LimitRange and one ResourceQuota", func(ctx SpecContext) {
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		limits := helmRenderWith(ctx, values, showOnly("templates/substrate-workerlimits.yaml")...)
		Expect(limits.Ignored).To(HaveKeyWithValue("LimitRange", 1))
		Expect(limits.Ignored).To(HaveKeyWithValue("ResourceQuota", 1))
	})
})
