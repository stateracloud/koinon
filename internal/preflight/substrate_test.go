// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/preflight"
)

var workerPoolGVR = schema.GroupVersionResource{Group: "ate.dev", Version: "v1", Resource: "workerpools"}

type fakeIssuer struct {
	issuer string
	err    error
}

func (f fakeIssuer) Issuer(context.Context) (string, error) { return f.issuer, f.err }

type fakeIssuerProbe struct {
	res IssuerResult
	err error
}

// IssuerResult aliases the package type for brevity.
type IssuerResult = preflight.IssuerResult

func (f fakeIssuerProbe) ProbeIssuer(context.Context, string) (IssuerResult, error) {
	return f.res, f.err
}

type fakeDevices struct{ err error }

func (f fakeDevices) ProbeDevices(context.Context) error { return f.err }

func substrateCRD(name string) *unstructured.Unstructured {
	return crd(name, "v1")
}

// workerPool is a WorkerPool of Substrate v1.0.0: status carries replicas and readyReplicas, not conditions.
// ready is "True" (all workers ready), "False" (none) or "" (no status yet).
func workerPool(name string, ready string) *unstructured.Unstructured {
	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1", "kind": "WorkerPool",
		"metadata": map[string]any{"name": name, "namespace": "ate-system"},
		"spec":     map[string]any{"replicas": int64(2)},
	}}
	switch ready {
	case "True":
		pool.Object["status"] = map[string]any{"replicas": int64(2), "readyReplicas": int64(2)}
	case "False":
		pool.Object["status"] = map[string]any{"replicas": int64(2), "readyReplicas": int64(0)}
	}
	return pool
}

func deployment(name, image string, args []string, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ate-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: name, Image: image, Args: args}},
		}}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

type substrateCluster struct {
	minor   string
	kube    []runtime.Object
	dynamic []runtime.Object
	issuer  fakeIssuer
	probe   fakeIssuerProbe
	devices fakeDevices
	config  preflight.Config
}

func healthySubstrate() *substrateCluster {
	c := &substrateCluster{
		minor:  "37",
		issuer: fakeIssuer{issuer: "https://issuer.example.com"},
		probe:  fakeIssuerProbe{res: IssuerResult{Reachable: true, Detail: "HTTP 200 OK"}},
	}
	c.kube = []runtime.Object{
		deployment("ate-api-server", "example.com/ate/api-server:v1.0.0", nil, 1),
		deployment("atenet-router", "example.com/ate/router:v1.0.0", []string{"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID("ate-system")}, 1),
	}
	for _, name := range preflight.SubstrateCRDs {
		c.dynamic = append(c.dynamic, substrateCRD(name))
	}
	c.dynamic = append(c.dynamic, workerPool("vm-pool", "True"))
	c.config = preflight.Config{
		PostgresMode: "external", GatewayMode: "standalone",
		Substrate: &preflight.SubstrateConfig{VMTier: true, WorkerPools: map[string]string{"vm": "vm-pool"}},
	}
	return c
}

func (c *substrateCluster) run(ctx context.Context) []preflight.Result {
	kube := fake.NewClientset(c.kube...)
	kube.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: c.minor, GitVersion: "v1." + c.minor + ".1"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList", workerPoolGVR: "WorkerPoolList"}, c.dynamic...)
	checker := &preflight.Checker{
		Kube: kube, Dynamic: dyn, Config: c.config,
		Issuer: c.issuer, IssuerProbe: c.probe, Devices: c.devices,
	}
	return checker.Run(ctx)
}

var _ = Describe("Substrate checks", func() {
	It("passes on a complete cluster and runs no OpenShell or Agent Sandbox check", func(ctx SpecContext) {
		results := healthySubstrate().run(ctx)
		Expect(checks(results)).To(Equal([]string{
			"Kubernetes version", "Substrate node devices", "Service-account issuer", "Agent Substrate", "Substrate router",
			"Substrate worker pool (vm tier)", "NetworkPolicy enforcement",
		}))
		for _, r := range results {
			if r.Check == "NetworkPolicy enforcement" {
				continue // no probe wired in this spec
			}
			Expect(r.Status).To(Equal(preflight.Pass), "%s: %s", r.Check, r.Message)
		}
	})

	It("fails on Kubernetes below 1.37 and passes on a managed 37+ minor", func(ctx SpecContext) {
		c := healthySubstrate()
		c.minor = "36"
		r := find(c.run(ctx), "Kubernetes version")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(ContainSubstring("below 1.37"))
		c.minor = "37+"
		Expect(find(c.run(ctx), "Kubernetes version").Status).To(Equal(preflight.Pass))
	})

	It("fails when Substrate is missing", func(ctx SpecContext) {
		c := healthySubstrate()
		c.dynamic = nil
		r := find(c.run(ctx), "Agent Substrate")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(ContainSubstring("sandboxconfigs.ate.dev"))
		Expect(r.Fix).To(ContainSubstring("substrate install"))
	})

	It("fails when the CRDs exist but the api server does not, is not ready or is not the pinned release", func(ctx SpecContext) {
		c := healthySubstrate()
		c.kube = c.kube[1:]
		Expect(find(c.run(ctx), "Agent Substrate").Message).To(ContainSubstring("not found"))
		c = healthySubstrate()
		c.kube[0] = deployment("ate-api-server", "example.com/ate/api-server:v1.0.0", nil, 0)
		Expect(find(c.run(ctx), "Agent Substrate").Message).To(ContainSubstring("no ready replica"))
		c = healthySubstrate()
		c.kube[0] = deployment("ate-api-server", "example.com/ate/api-server:v1.0.1", nil, 1)
		Expect(find(c.run(ctx), "Agent Substrate").Message).To(ContainSubstring("v1.0.1 is not the pinned release v1.0.0"))
		c = healthySubstrate()
		c.kube[0] = deployment("ate-api-server", "example.com/ate/api-server", nil, 1)
		untagged := find(c.run(ctx), "Agent Substrate")
		Expect(untagged.Status).To(Equal(preflight.Fail))
		Expect(untagged.Message).To(ContainSubstring("no tag"))
		// The api server is found by container name, not by position: a sidecar first does not hide it.
		c = healthySubstrate()
		d := deployment("ate-api-server", "example.com/ate/api-server:v1.0.0", nil, 1)
		d.Spec.Template.Spec.Containers = append([]corev1.Container{{Name: "objectstore-plugin", Image: "example.com/ate/plugin:v9"}}, d.Spec.Template.Spec.Containers...)
		c.kube[0] = d
		Expect(find(c.run(ctx), "Agent Substrate").Status).To(Equal(preflight.Pass))
	})

	It("fails when the router does not allow the atenet client", func(ctx SpecContext) {
		c := healthySubstrate()
		c.kube[1] = deployment("atenet-router", "example.com/ate/router:v1.0.0", nil, 1)
		r := find(c.run(ctx), "Substrate router")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Fix).To(ContainSubstring("--ingress-allowed-spiffe-ids="))
	})

	It("pins the router allow-list to the configured Substrate namespace", func(ctx SpecContext) {
		c := healthySubstrate()
		Expect(find(c.run(ctx), "Substrate router").Message).To(ContainSubstring("ns/ate-system/sa/atenet-client"))
		c.config.Substrate.Namespace = "substrate-prod"
		moved := deployment("atenet-router", "example.com/ate/router:v1.0.0", []string{"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID("ate-system")}, 1)
		moved.Namespace = "substrate-prod"
		c.kube = append(c.kube, moved)
		r := find(c.run(ctx), "Substrate router")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Fix).To(ContainSubstring("ns/substrate-prod/sa/atenet-client"))
	})

	It("fails on a worker pool that is missing or not ready", func(ctx SpecContext) {
		c := healthySubstrate()
		c.dynamic[len(c.dynamic)-1] = workerPool("vm-pool", "False")
		Expect(find(c.run(ctx), "Substrate worker pool").Message).To(ContainSubstring("0 of 2 workers ready"))
		c.dynamic = c.dynamic[:len(c.dynamic)-1]
		Expect(find(c.run(ctx), "Substrate worker pool").Message).To(ContainSubstring("not found"))
	})

	It("fails when a node lacks the microVM devices, and skips the probe outside the vm tier", func(ctx SpecContext) {
		c := healthySubstrate()
		c.devices = fakeDevices{err: errors.New("FailedMount: /dev/kvm is not a character device")}
		r := find(c.run(ctx), "Substrate node devices")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(And(ContainSubstring("/dev/kvm"), ContainSubstring("/dev/vhost-vsock")))
		c.config.Substrate.VMTier = false
		Expect(checks(c.run(ctx))).NotTo(ContainElement("Substrate node devices"))
	})

	Describe("service-account issuer", func() {
		It("fails with the supported fixes when a pod cannot reach an IPv6 ULA issuer", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{issuer: "https://[fdae:1::1]:10001"}
			c.probe = fakeIssuerProbe{res: IssuerResult{Detail: "dial tcp [fdae:1::1]:10001: connect: network is unreachable"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(And(
				ContainSubstring("https://[fdae:1::1]:10001"), ContainSubstring("not reachable from a pod in namespace ate-system"),
				ContainSubstring("invalid bearer token"), ContainSubstring("IPv6 unique-local address"),
			))
			Expect(r.Fix).To(And(ContainSubstring("service-account-issuer"), ContainSubstring("discovery-URL override"), ContainSubstring("not a supported install")))
		})

		It("fails, not warns, when the issuer is reachable only without TLS verification", func(ctx SpecContext) {
			c := healthySubstrate()
			c.probe = fakeIssuerProbe{res: IssuerResult{Reachable: true, Unverified: true, Detail: "TLS verification failed"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring("invalid bearer token"))
		})

		It("fails when the discovery document cannot be read or the probe pod fails", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{err: errors.New("forbidden")}
			Expect(find(c.run(ctx), "Service-account issuer").Fix).To(ContainSubstring("system:service-account-issuer-discovery"))
			c = healthySubstrate()
			c.probe = fakeIssuerProbe{err: errors.New("timed out")}
			Expect(find(c.run(ctx), "Service-account issuer").Message).To(ContainSubstring("timed out"))
		})

		It("reads the issuer from a discovery document and says why a host is unroutable", func() {
			iss, err := preflight.ParseIssuer([]byte(`{"issuer":"https://k.example","jwks_uri":"x"}`))
			Expect(err).NotTo(HaveOccurred())
			Expect(iss).To(Equal("https://k.example"))
			_, err = preflight.ParseIssuer([]byte(`{}`))
			Expect(err).To(HaveOccurred())
			Expect(preflight.UnroutableIssuerHost("https://[fdae::1]:1")).To(ContainSubstring("unique-local"))
			Expect(preflight.UnroutableIssuerHost("https://127.0.0.1")).To(ContainSubstring("loopback"))
			Expect(preflight.UnroutableIssuerHost("https://kubernetes.default.svc")).To(BeEmpty())
		})

		It("round-trips the probe output", func() {
			res, err := preflight.ParseIssuerProbeOutput(preflight.IssuerResult{Reachable: true, Unverified: true, Detail: "HTTP 200 OK"}.String() + "\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(preflight.IssuerResult{Reachable: true, Unverified: true, Detail: "HTTP 200 OK"}))
			_, err = preflight.ParseIssuerProbeOutput("noise")
			Expect(err).To(HaveOccurred())
		})
	})

	It("builds a device probe pod with CharDevice host paths and no token", func() {
		p := preflight.KubeSubstrateProber{Namespace: "ate-system", NamePrefix: "x", Image: "img"}.DevicesPod()
		Expect(p.Spec.Volumes).To(HaveLen(len(preflight.SubstrateDevices)))
		for _, v := range p.Spec.Volumes {
			Expect(*v.HostPath.Type).To(Equal(corev1.HostPathCharDev))
		}
		Expect(*p.Spec.AutomountServiceAccountToken).To(BeFalse())
	})

	It("puts only the device probe on the worker nodes", func() {
		prober := preflight.KubeSubstrateProber{Namespace: "ate-system", NamePrefix: "x", Image: "img", NodeSelector: map[string]string{"pool": "workers"}}
		Expect(prober.DevicesPod().Spec.NodeSelector).To(Equal(map[string]string{"pool": "workers"}))
		Expect(prober.IssuerPod("https://issuer.example.com").Spec.NodeSelector).To(BeEmpty())
	})

	It("deletes stale probe pods of the same component before it creates its own", func(ctx SpecContext) {
		stale := func(name, component, partOf string) *corev1.Pod {
			return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ate-system", Labels: map[string]string{
				"app.kubernetes.io/component": component, "app.kubernetes.io/part-of": partOf,
			}}}
		}
		client := fake.NewClientset(
			stale("old-devices", "devices-probe", "koinon"),
			stale("other-component", "issuer-probe", "koinon"),
			stale("other-owner", "devices-probe", "someone-else"),
		)
		client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			pod.Name = "fresh"
			pod.Status.Phase = corev1.PodSucceeded
			return false, nil, nil
		})
		p := preflight.KubeSubstrateProber{
			Client: client, Namespace: "ate-system", NamePrefix: "x", Image: "img",
			Labels: map[string]string{"app.kubernetes.io/part-of": "koinon"}, Timeout: time.Second, PollInterval: time.Millisecond,
			Logs: func(context.Context, string, string) (string, error) {
				return preflight.SubstrateDevicesProbeMarker + " ok\n", nil
			},
		}
		Expect(p.ProbeDevices(ctx)).To(Succeed())
		left, err := client.CoreV1().Pods("ate-system").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, pod := range left.Items {
			names = append(names, pod.Name)
		}
		Expect(names).To(ConsistOf("other-component", "other-owner"))
	})
})
