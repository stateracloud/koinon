// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/preflight"
)

// workerPools renders the chart with the values file and extra helm arguments and returns the WorkerPool
// objects (a CRD kind that helmRenderWith only counts) as plain maps.
func workerPools(ctx context.Context, valuesFile string, extra ...string) []map[string]any {
	args := append([]string{"template", release, chartDir, "--namespace", namespace, "-f", valuesFile}, extra...)
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, envOr("HELM", "helm"), args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	Expect(cmd.Run()).To(Succeed(), stderr.String())
	var pools []map[string]any
	for _, doc := range strings.Split(out.String(), "\n---") {
		var obj map[string]any
		Expect(yaml.Unmarshal([]byte(doc), &obj)).To(Succeed())
		if obj["kind"] == "WorkerPool" {
			pools = append(pools, obj)
		}
	}
	return pools
}

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
		// with the chart. Worker pods, control-plane pods, CONNECT, the installer-written kinds and the worker NetworkPolicy guard: five policies, five bindings.
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		policy := helmRenderWith(ctx, values, showOnly("templates/substrate-policy.yaml")...)
		Expect(policy.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicy", 5))
		Expect(policy.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicyBinding", 5))
		netpol := helmRenderWith(ctx, values, showOnly("templates/substrate-networkpolicy.yaml")...)
		Expect(netpol.Ignored).To(HaveKeyWithValue("NetworkPolicy", 6))
		// The fence of otel-system and podcertificate-controller-system and the telemetry route (issue #1172): seven policies.
		fence := helmRenderWith(ctx, values, showOnly("templates/substrate-fence-networkpolicy.yaml")...)
		Expect(fence.Ignored).To(HaveKeyWithValue("NetworkPolicy", 7))
		namespaces := helmRenderWith(ctx, values, showOnly("templates/substrate-workernamespace.yaml")...)
		Expect(namespaces.Ignored).To(HaveKeyWithValue("Namespace", 1))
		// The read-only observer access of the reconciler to the worker namespace (issue #1162).
		observer := helmRenderWith(ctx, values, showOnly("templates/substrate-observer.yaml")...)
		Expect(observer.Ignored).To(HaveKeyWithValue("Role", 1))
		Expect(observer.Ignored).To(HaveKeyWithValue("RoleBinding", 1))
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

	// The CA of the cache reaches atelet (issue #1191): a pre-install hook Job after ensure-secrets copies the public CA
	// to the Substrate namespace. It reads the Secret and sets the pod name space from the downward API, as the flags
	// of "koinon-api imagecache-ca" need (internal/cmd/koinonapi/flags_test.go).
	It("renders the hook Job that publishes the CA of the image cache", func(ctx SpecContext) {
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		ca := helmRenderWith(ctx, values, showOnly("templates/substrate-imagecache-ca.yaml")...)
		Expect(ca.Jobs).To(HaveLen(1))
		job := ca.Jobs[0]
		Expect(job.Annotations).To(HaveKeyWithValue("helm.sh/hook", "pre-install,pre-upgrade"))
		c := job.Spec.Template.Spec.Containers[0]
		Expect(c.Args).To(ContainElements("imagecache-ca", "--substrate-namespace=ate-system"))
		Expect(c.Args).To(ContainElement(HavePrefix("--tls-secret=")))
		Expect(c.Args).To(ContainElement("--cache-deployment=koinon-imagecache"))
		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("koinon-imagecache-ca"))
		Expect(c.Env).To(ContainElement(HaveField("Name", "POD_NAMESPACE")))
		// The Role and the RoleBinding of the Substrate namespace are ignored here (the suite applies no other
		// namespace). The ones of the release namespace are objects.
		Expect(ca.Ignored).To(HaveKeyWithValue("Role", 1))
		Expect(ca.Ignored).To(HaveKeyWithValue("RoleBinding", 1))
	})

	It("renders no WorkerPool with the OpenShell driver", func(ctx SpecContext) {
		r := helmRender(ctx, filepath.Join(chartDir, "ci", "external-standalone-values.yaml"))
		Expect(r.Ignored).NotTo(HaveKey("WorkerPool"))
	})

	// The default WorkerPool (issue #1089): the chart makes it with the Substrate driver, on by default. Its worker
	// image is the one of the Substrate release that this Koinon release pins (internal/preflight/substratepin.go),
	// so the chart and the preflight cannot name two releases. Its ServiceAccount is in the worker namespace and
	// is not applied here.
	It("renders the default WorkerPool with the pinned worker image and its size", func(ctx SpecContext) {
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		r := helmRender(ctx, values)
		Expect(r.Ignored).To(HaveKeyWithValue("WorkerPool", 1))
		Expect(r.Ignored).To(HaveKeyWithValue("ServiceAccount", 1))
		pools := workerPools(ctx, values, externalDatabase...)
		Expect(pools).To(HaveLen(1))
		// The label that the substrate driver selects workers on, with the runtime class of the sandboxes
		// (openshell.runtimeClassName, default kata). Substrate copies the WorkerPool labels to its workers.
		Expect(pools[0]["metadata"]).To(HaveKeyWithValue("labels", HaveKeyWithValue(substrate.RuntimeClassSelector, "kata")))
		spec := pools[0]["spec"].(map[string]any)
		Expect(spec["workerImage"]).To(Equal(preflight.SubstratePinnedImageRepo + "/ateworker-gvisor:" + preflight.SubstratePinnedVersion))
		Expect(spec["replicas"]).To(BeEquivalentTo(1))
		Expect(spec["template"]).To(HaveKeyWithValue("resources", map[string]any{
			"requests": map[string]any{"cpu": "500m", "memory": "1Gi"},
			"limits":   map[string]any{"cpu": "4", "memory": "8704Mi"},
		}))

		sized := workerPools(ctx, values, append(showOnly("templates/substrate-workerpool.yaml"),
			"--set", "execution.substrate.workerPool.replicas=3")...)
		Expect(sized[0]["spec"]).To(HaveKeyWithValue("replicas", BeEquivalentTo(3)))

		off := helmRenderWith(ctx, values, append(append([]string{}, externalDatabase...),
			"--set", "execution.substrate.workerPool.enabled=false")...)
		Expect(off.Ignored).NotTo(HaveKey("WorkerPool"))
	})
})

// The worker namespace limits (issue #1161): the LimitRange, the ResourceQuota and their write guard (one
// ValidatingAdmissionPolicy and its binding) reach the manifest.
var _ = Describe("substrate worker limits", func() {
	It("renders one LimitRange, one ResourceQuota and their write guard", func(ctx SpecContext) {
		values := filepath.Join(chartDir, "ci", "substrate-values.yaml")
		limits := helmRenderWith(ctx, values, showOnly("templates/substrate-workerlimits.yaml")...)
		Expect(limits.Ignored).To(HaveKeyWithValue("LimitRange", 1))
		Expect(limits.Ignored).To(HaveKeyWithValue("ResourceQuota", 1))
		Expect(limits.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicy", 1))
		Expect(limits.Ignored).To(HaveKeyWithValue("ValidatingAdmissionPolicyBinding", 1))
	})
})
