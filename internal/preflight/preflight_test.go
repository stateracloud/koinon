// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/preflight"
)

var (
	crdGVR          = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	gatewayClassGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}
)

func crd(name string, served ...string) *unstructured.Unstructured {
	var versions []any
	for _, v := range served {
		versions = append(versions, map[string]any{"name": v, "served": true, "storage": true})
	}
	versions = append(versions, map[string]any{"name": "v1alpha0", "served": false, "storage": false})
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"versions": versions},
	}}
}

func gatewayClass(name, accepted, reason string) *unstructured.Unstructured {
	gc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "GatewayClass",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"controllerName": "agentgateway.dev/agentgateway"},
	}}
	if accepted != "" {
		gc.Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "SupportedVersion", "status": "True"},
			map[string]any{"type": "Accepted", "status": accepted, "reason": reason, "message": "details"},
		}}
	}
	return gc
}

func controller(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "sidecar", Image: "example.com/proxy:v9"},
				{Name: "manager", Image: image},
			},
		}}},
	}
}

// cnpgWebhook is the CNPG operator's validating webhook configuration, as its Helm chart
// installs it into namespace cnpg-system.
func cnpgWebhook() *admissionregistrationv1.ValidatingWebhookConfiguration {
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cnpg-validating-webhook-configuration"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{
				Name: "vbackup.cnpg.io",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{Namespace: "cnpg-system", Name: "cnpg-webhook-service"},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{Rule: admissionregistrationv1.Rule{
					APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"backups"},
				}}},
			},
			{
				Name: "vcluster.cnpg.io",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{Namespace: "cnpg-system", Name: "cnpg-webhook-service"},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{Rule: admissionregistrationv1.Rule{
					APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"},
				}}},
			},
		},
	}
}

// webhookEndpoints is the EndpointSlice of the CNPG webhook Service with one endpoint.
func webhookEndpoints(ready bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnpg-webhook-service-x7k2p", Namespace: "cnpg-system",
			Labels: map[string]string{discoveryv1.LabelServiceName: "cnpg-webhook-service"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{"10.0.0.7"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
	}
}

func agentgatewayController(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agentgateway", Namespace: "agentgateway-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agentgateway", Image: image}},
		}}},
	}
}

func runtimeClass(name string, labels map[string]string) *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Handler: "kata",
		Overhead: &nodev1.Overhead{PodFixed: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("130Mi"), corev1.ResourceCPU: resource.MustParse("250m"),
		}},
	}
}

func runtimeClassNoOverhead(name string, labels map[string]string) *nodev1.RuntimeClass {
	rc := runtimeClass(name, labels)
	rc.Overhead = nil
	return rc
}

type fakeGateway struct {
	resp *openshellv1.HealthResponse
	err  error
}

func (g fakeGateway) Health(context.Context) (*openshellv1.HealthResponse, error) {
	return g.resp, g.err
}

type fakeProber struct {
	res preflight.ProbeResult
	err error
}

func (p fakeProber) Probe(context.Context) (preflight.ProbeResult, error) { return p.res, p.err }

type fakeNetPol struct {
	res preflight.NetPolResult
	err error
}

func (p fakeNetPol) Probe(context.Context) (preflight.NetPolResult, error) { return p.res, p.err }

// cluster is a cluster with every prerequisite in place; tests break one at a time.
type cluster struct {
	kube    []runtime.Object
	dynamic []runtime.Object
	gateway preflight.OpenShell
	prober  preflight.Prober
	netpol  preflight.NetPolProber
	config  preflight.Config
	// forbid makes reads of this resource return Forbidden.
	forbid string
}

func healthyCluster() *cluster {
	return &cluster{
		kube: []runtime.Object{
			controller("registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.10"),
			agentgatewayController("cr.agentgateway.dev/controller:v1.7.0"),
			runtimeClass("kata", map[string]string{preflight.AttachableLabel: "true"}),
			cnpgWebhook(),
			webhookEndpoints(true),
			// Another Service's endpoints in the namespace do not count.
			&discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cnpg-metrics-abcde", Namespace: "cnpg-system",
					Labels: map[string]string{discoveryv1.LabelServiceName: "cnpg-metrics"},
				},
				Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.8"}}},
			},
		},
		dynamic: []runtime.Object{
			crd("sandboxes.agents.x-k8s.io", "v1beta1"),
			crd("clusters.postgresql.cnpg.io", "v1"),
			crd("agentgatewaybackends.agentgateway.dev", "v1alpha1"),
			crd("agentgatewaymodels.agentgateway.dev", "v1alpha1"),
			crd("agentgatewayparameters.agentgateway.dev", "v1alpha1"),
			crd("agentgatewaypolicies.agentgateway.dev", "v1alpha1"),
			gatewayClass("agentgateway", "True", "Accepted"),
		},
		gateway: fakeGateway{resp: &openshellv1.HealthResponse{
			Status: openshellv1.ServiceStatus_SERVICE_STATUS_HEALTHY, Version: "0.1.7",
		}},
		prober: fakeProber{res: preflight.ProbeResult{Seccomp: 2, Filters: 1, Keyctl: "EPERM"}},
		netpol: fakeNetPol{res: preflight.NetPolResult{ControlReached: true}},
		config: preflight.Config{
			PostgresMode: "cnpg", GatewayMode: "kubernetes", GatewayClassName: "agentgateway",
			RuntimeClassName: "kata", OpenShellAddress: "openshell.openshell.svc:8080",
			AgentSandboxNamespace: "agent-sandbox-system", AgentSandboxController: "agent-sandbox-controller",
			AgentgatewayNamespace: "agentgateway-system", AgentgatewayController: "agentgateway",
		},
	}
}

// setAgentgatewayController replaces the controller Deployment, which shares its name with the
// GatewayClass; nil removes it.
func (c *cluster) setAgentgatewayController(d *appsv1.Deployment) {
	var out []runtime.Object
	for _, o := range c.kube {
		if dep, ok := o.(*appsv1.Deployment); !ok || dep.Namespace != "agentgateway-system" {
			out = append(out, o)
		}
	}
	if d != nil {
		out = append(out, d)
	}
	c.kube = out
}

func (c *cluster) without(name string) *cluster {
	keep := func(objs []runtime.Object) []runtime.Object {
		var out []runtime.Object
		for _, o := range objs {
			if o.(metav1.Object).GetName() != name {
				out = append(out, o)
			}
		}
		return out
	}
	c.kube, c.dynamic = keep(c.kube), keep(c.dynamic)
	return c
}

func (c *cluster) with(obj runtime.Object) *cluster {
	return c.without(obj.(metav1.Object).GetName()).add(obj)
}

func (c *cluster) add(obj runtime.Object) *cluster {
	if _, ok := obj.(*unstructured.Unstructured); ok {
		c.dynamic = append(c.dynamic, obj)
	} else {
		c.kube = append(c.kube, obj)
	}
	return c
}

func (c *cluster) run(ctx context.Context) []preflight.Result {
	kube := fake.NewClientset(c.kube...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			crdGVR:          "CustomResourceDefinitionList",
			gatewayClassGVR: "GatewayClassList",
		}, c.dynamic...)
	if c.forbid != "" {
		forbidden := func(a k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: c.forbid}, "x", errors.New("no RBAC"))
		}
		kube.PrependReactor("get", c.forbid, forbidden)
		dyn.PrependReactor("get", c.forbid, forbidden)
	}
	checker := &preflight.Checker{
		Kube: kube, Dynamic: dyn, OpenShell: c.gateway, Prober: c.prober, NetPol: c.netpol, Config: c.config,
	}
	return checker.Run(ctx)
}

func find(results []preflight.Result, prefix string) preflight.Result {
	for _, r := range results {
		if strings.HasPrefix(r.Check, prefix) {
			return r
		}
	}
	Fail("no result for " + prefix)
	return preflight.Result{}
}

func checks(results []preflight.Result) []string {
	var names []string
	for _, r := range results {
		names = append(names, r.Check)
	}
	return names
}

var _ = Describe("Checker", func() {
	It("passes every check on a complete cluster", func(ctx SpecContext) {
		results := healthyCluster().run(ctx)
		Expect(checks(results)).To(Equal([]string{
			"Agent Sandbox", "OpenShell gateway", `RuntimeClass "kata"`, "CloudNativePG",
			`GatewayClass "agentgateway"`, "Kata guest seccomp", "NetworkPolicy enforcement",
		}))
		for _, r := range results {
			Expect(r.Status).To(Equal(preflight.Pass), "%s: %s", r.Check, r.Message)
		}
		Expect(find(results, "Agent Sandbox").Message).To(ContainSubstring("controller v1.0.10"))
		Expect(find(results, "OpenShell").Message).To(ContainSubstring("version 0.1.7"))
		Expect(find(results, "CloudNativePG").Message).To(ContainSubstring("operator webhook cnpg-system/cnpg-webhook-service ready"))

		var out strings.Builder
		Expect(preflight.Write(&out, results)).To(BeZero())
		Expect(out.String()).To(ContainSubstring("PASS  Agent Sandbox"))
		Expect(out.String()).To(HaveSuffix("Preflight passed.\n"))
	})

	It("skips the CNPG and GatewayClass checks in external and standalone mode", func(ctx SpecContext) {
		c := healthyCluster().without("clusters.postgresql.cnpg.io").without("agentgateway")
		c.config.PostgresMode, c.config.GatewayMode = "external", "standalone"
		results := c.run(ctx)
		Expect(checks(results)).To(Equal([]string{
			"Agent Sandbox", "OpenShell gateway", `RuntimeClass "kata"`, "Kata guest seccomp",
			"NetworkPolicy enforcement",
		}))
		Expect(preflight.Write(&strings.Builder{}, results)).To(BeZero())
	})

	It("does not ask a non-Kata RuntimeClass for overhead.podFixed", func(ctx SpecContext) {
		c := healthyCluster()
		rc := runtimeClassNoOverhead("kata", map[string]string{preflight.AttachableLabel: "true"})
		rc.Handler = "runc"
		c.with(rc)
		r := find(c.run(ctx), "RuntimeClass")
		Expect(r.Status).To(Equal(preflight.Pass), r.Message)
		Expect(r.Message).To(ContainSubstring("handler runc"))
	})

	DescribeTable("fails when a prerequisite is missing",
		func(ctx SpecContext, breakIt func(*cluster), check, message, fix string) {
			c := healthyCluster()
			breakIt(c)
			results := c.run(ctx)
			r := find(results, check)
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring(message))
			Expect(r.Fix).To(ContainSubstring(fix))
			for _, other := range results {
				if other.Check != r.Check {
					Expect(other.Status).To(Equal(preflight.Pass), "%s: %s", other.Check, other.Message)
				}
			}

			var out strings.Builder
			Expect(preflight.Write(&out, results)).To(Equal(1))
			Expect(out.String()).To(ContainSubstring("FAIL  " + r.Check + ": " + r.Message))
			Expect(out.String()).To(ContainSubstring("fix: "))
			Expect(out.String()).To(ContainSubstring("Preflight failed: 1 of 7 checks failed"))
		},
		Entry("Agent Sandbox CRD", func(c *cluster) { c.without("sandboxes.agents.x-k8s.io") },
			"Agent Sandbox", "CRD sandboxes.agents.x-k8s.io not found", "v1.0.10/sandbox-with-extensions.yaml"),
		Entry("Agent Sandbox API version", func(c *cluster) { c.with(crd("sandboxes.agents.x-k8s.io", "v1alpha1")) },
			"Agent Sandbox", "serves [v1alpha1], not v1beta1", "v1.0.x"),
		Entry("Agent Sandbox controller", func(c *cluster) { c.without("agent-sandbox-controller") },
			"Agent Sandbox", "controller Deployment agent-sandbox-system/agent-sandbox-controller not found",
			"preflight.agentSandbox.namespace"),
		Entry("Agent Sandbox release", func(c *cluster) {
			c.with(controller("registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.1.0"))
		}, "Agent Sandbox", "controller version v1.1.0 is not supported", "v1.0.x"),
		Entry("OpenShell address", func(c *cluster) { c.gateway = nil },
			"OpenShell gateway", "no gateway address configured", "openshell.address"),
		Entry("OpenShell unreachable", func(c *cluster) { c.gateway = fakeGateway{err: errors.New("connection refused")} },
			"OpenShell gateway", "openshell.openshell.svc:8080 not reachable: connection refused", "openshell.clientTLSSecret"),
		Entry("OpenShell version", func(c *cluster) {
			c.gateway = fakeGateway{resp: &openshellv1.HealthResponse{
				Status: openshellv1.ServiceStatus_SERVICE_STATUS_HEALTHY, Version: "0.2.0",
			}}
		}, "OpenShell gateway", `version "0.2.0" is not supported; Koinon v0.1 supports 0.1.7`, "install OpenShell 0.1.7"),
		Entry("OpenShell unhealthy", func(c *cluster) {
			c.gateway = fakeGateway{resp: &openshellv1.HealthResponse{
				Status: openshellv1.ServiceStatus_SERVICE_STATUS_UNHEALTHY, Version: "0.1.7",
			}}
		}, "OpenShell gateway", "SERVICE_STATUS_UNHEALTHY", "logs"),
		Entry("RuntimeClass", func(c *cluster) { c.without("kata") },
			"RuntimeClass", "not found", "openshell.runtimeClassName"),
		Entry("RuntimeClass label", func(c *cluster) { c.with(runtimeClass("kata", nil)) },
			"RuntimeClass", `label openshell.ai/sandbox-attachable="true" missing`,
			"kubectl label runtimeclass kata openshell.ai/sandbox-attachable=true"),
		Entry("CNPG operator", func(c *cluster) { c.without("clusters.postgresql.cnpg.io") },
			"CloudNativePG", "CNPG operator is not installed", "postgres.mode=external"),
		Entry("CNPG operator without its webhook", func(c *cluster) { c.without("cnpg-validating-webhook-configuration") },
			"CloudNativePG", "no admission webhook validates it: the CNPG operator is not installed", "CloudNativePG operator"),
		Entry("CNPG operator not ready", func(c *cluster) { c.with(webhookEndpoints(false)) },
			"CloudNativePG", "the CNPG operator is not running: its webhook Service cnpg-system/cnpg-webhook-service has no ready endpoints",
			"kubectl -n cnpg-system get deployments,pods"),
		Entry("CNPG operator scaled to zero", func(c *cluster) { c.without("cnpg-webhook-service-x7k2p") },
			"CloudNativePG", "has no ready endpoints", "kubectl -n cnpg-system"),
		Entry("GatewayClass", func(c *cluster) { c.without("agentgateway") },
			"GatewayClass", "agentgateway's Kubernetes controller is not installed", "gateway.mode=standalone"),
		Entry("GatewayClass not accepted", func(c *cluster) { c.with(gatewayClass("agentgateway", "False", "InvalidParameters")) },
			"GatewayClass", "not accepted: InvalidParameters: details", "controller's logs"),
		Entry("GatewayClass of another controller", func(c *cluster) {
			gc := gatewayClass("agentgateway", "True", "Accepted")
			_ = unstructured.SetNestedField(gc.Object, "example.com/other-gateway", "spec", "controllerName")
			c.with(gc)
		}, "GatewayClass", `served by controller "example.com/other-gateway"`, "gateway.kubernetes.gatewayClassName"),
		Entry("GatewayClass without status", func(c *cluster) { c.with(gatewayClass("agentgateway", "", "")) },
			"GatewayClass", "no Accepted condition", "controller is running"),
		Entry("agentgateway CRDs", func(c *cluster) { c.without("agentgatewaymodels.agentgateway.dev") },
			"GatewayClass", "CRD agentgatewaymodels.agentgateway.dev not found", "agentgateway-crds"),
		Entry("agentgateway CRD version", func(c *cluster) { c.with(crd("agentgatewaypolicies.agentgateway.dev", "v1beta1")) },
			"GatewayClass", "serves [v1beta1], not v1alpha1", "v1.7.x"),
		Entry("agentgateway controller", func(c *cluster) { c.setAgentgatewayController(nil) },
			"GatewayClass", "controller Deployment agentgateway-system/agentgateway not found", "preflight.agentgateway.namespace"),
		Entry("agentgateway release", func(c *cluster) {
			c.setAgentgatewayController(agentgatewayController("cr.agentgateway.dev/controller:v1.6.0"))
		},
			"GatewayClass", "is not a tested release", "v1.7.x"),
		Entry("RBAC", func(c *cluster) { c.forbid = "runtimeclasses" },
			"RuntimeClass", "the preflight ServiceAccount may not read RuntimeClass kata", "ClusterRole"),
	)

	It("accepts a v-prefixed OpenShell version with build metadata", func(ctx SpecContext) {
		c := healthyCluster()
		c.gateway = fakeGateway{resp: &openshellv1.HealthResponse{
			Status: openshellv1.ServiceStatus_SERVICE_STATUS_HEALTHY, Version: "v0.1.7+5d25211",
		}}
		Expect(find(c.run(ctx), "OpenShell").Status).To(Equal(preflight.Pass))
	})

	DescribeTable("warns without failing",
		func(ctx SpecContext, breakIt func(*cluster), check, message, fix string) {
			c := healthyCluster()
			breakIt(c)
			results := c.run(ctx)
			r := find(results, check)
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Message).To(ContainSubstring(message))
			Expect(r.Fix).To(ContainSubstring(fix))

			var out strings.Builder
			Expect(preflight.Write(&out, results)).To(BeZero())
			Expect(out.String()).To(ContainSubstring("WARN  " + r.Check))
			Expect(out.String()).To(HaveSuffix("Preflight passed with 1 warnings.\n"))
		},
		Entry("RuntimeClass overhead", func(c *cluster) {
			c.with(runtimeClassNoOverhead("kata", map[string]string{preflight.AttachableLabel: "true"}))
		}, "RuntimeClass", "no overhead.podFixed memory and cpu", "beyond its container limits"),
		Entry("RuntimeClass overhead memory", func(c *cluster) {
			rc := runtimeClass("kata", map[string]string{preflight.AttachableLabel: "true"})
			delete(rc.Overhead.PodFixed, corev1.ResourceMemory)
			c.with(rc)
		}, "RuntimeClass", "no overhead.podFixed memory: ", "memory: 130Mi, cpu: 250m for Cloud Hypervisor"),
		Entry("RuntimeClass overhead cpu", func(c *cluster) {
			rc := runtimeClass("kata", map[string]string{preflight.AttachableLabel: "true"})
			rc.Overhead.PodFixed[corev1.ResourceCPU] = resource.MustParse("0")
			c.with(rc)
		}, "RuntimeClass", "no overhead.podFixed cpu: ", "memory: 320Mi, cpu: 250m for QEMU"),
		Entry("guest seccomp off", func(c *cluster) {
			c.prober = fakeProber{res: preflight.ProbeResult{Seccomp: 0, Filters: 0, Keyctl: "allowed"}}
		}, "Kata guest seccomp", "off: a kata pod with RuntimeDefault runs with Seccomp: 0 and keyctl allowed",
			"disable_guest_seccomp = false"),
		Entry("keyctl allowed under seccomp", func(c *cluster) {
			c.prober = fakeProber{res: preflight.ProbeResult{Seccomp: 2, Filters: 1, Keyctl: "allowed"}}
		}, "Kata guest seccomp", "Seccomp: 2 and keyctl allowed", "disable_guest_seccomp = false"),
		Entry("probe failed", func(c *cluster) { c.prober = fakeProber{err: errors.New("pod still Pending")} },
			"Kata guest seccomp", "probe pod did not report: pod still Pending", "grep Seccomp: /proc/self/status"),
		Entry("probe disabled", func(c *cluster) { c.prober = nil },
			"Kata guest seccomp", "not checked (preflight.seccompProbe.enabled=false)", "runtimeClassName kata"),
		Entry("NetworkPolicy not enforced", func(c *cluster) {
			c.netpol = fakeNetPol{res: preflight.NetPolResult{ControlReached: true, DeniedReached: true}}
		}, "NetworkPolicy enforcement", preflight.NetPolNotEnforced, "network plugin that enforces NetworkPolicy"),
		Entry("NetworkPolicy probe inconclusive", func(c *cluster) { c.netpol = fakeNetPol{} },
			"NetworkPolicy enforcement", "inconclusive", "verify by hand"),
		Entry("NetworkPolicy probe failed", func(c *cluster) { c.netpol = fakeNetPol{err: errors.New("server pod still Pending")} },
			"NetworkPolicy enforcement", "probe did not report: server pod still Pending", "verify by hand"),
		Entry("NetworkPolicy probe disabled", func(c *cluster) { c.netpol = nil },
			"NetworkPolicy enforcement", "not checked (preflight.networkPolicyProbe.enabled=false)", "verify by hand"),
		Entry("untagged Agent Sandbox controller", func(c *cluster) {
			c.with(controller("registry.k8s.io/agent-sandbox/agent-sandbox-controller@sha256:0123"))
		}, "Agent Sandbox", "has no tag", "v1.0.x"),
		Entry("degraded OpenShell gateway", func(c *cluster) {
			c.gateway = fakeGateway{resp: &openshellv1.HealthResponse{
				Status: openshellv1.ServiceStatus_SERVICE_STATUS_DEGRADED, Version: "0.1.7",
			}}
		}, "OpenShell gateway", "reports degraded", "logs"),
		Entry("plaintext OpenShell connection", func(c *cluster) { c.config.OpenShellPlaintext = true },
			"OpenShell gateway", "connected without TLS", "openshell.clientTLSSecret"),
	)
})

var _ = Describe("imageTag", func() {
	DescribeTable("extracts the tag",
		func(ref, tag string) { Expect(preflight.ImageTag(ref)).To(Equal(tag)) },
		Entry("tag", "registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.10", "v1.0.10"),
		Entry("tag and digest", "r.example:5000/a/b:v1.0.2@sha256:abc", "v1.0.2"),
		Entry("registry port, no tag", "r.example:5000/a/b", ""),
		Entry("digest only", "r.example/a/b@sha256:abc", ""),
	)
})
