// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	res    IssuerResult
	err    error
	probed string
}

// IssuerResult aliases the package type for brevity.
type IssuerResult = preflight.IssuerResult

func (f *fakeIssuerProbe) ProbeIssuer(_ context.Context, issuer string) (IssuerResult, error) {
	f.probed = issuer
	return f.res, f.err
}

type fakeDevices struct{ err error }

func (f fakeDevices) ProbeDevices(context.Context) error { return f.err }

func substrateCRD(name string) *unstructured.Unstructured {
	return crd(name, "v1")
}

// workerPool is a WorkerPool of Substrate v1.0.1: status carries replicas and readyReplicas, not conditions.
// ready is "True" (all workers ready), "False" (none) or "" (no status yet).
func workerPool(name string, ready string) *unstructured.Unstructured {
	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1", "kind": "WorkerPool",
		"metadata": map[string]any{"name": name, "namespace": "ate-workers"},
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
	probe   *fakeIssuerProbe
	devices fakeDevices
	userns  preflight.UserNamespacesProber
	atefs   preflight.AteFSProber
	config  preflight.Config
}

func healthySubstrate() *substrateCluster {
	c := &substrateCluster{
		minor:  "37",
		issuer: fakeIssuer{issuer: "https://issuer.example.com"},
		probe:  &fakeIssuerProbe{res: IssuerResult{Reachable: true, TokenIssuer: "https://issuer.example.com", Detail: "HTTP 200 OK"}},
		atefs:  fakeAteFS{nodes: []preflight.NodeAteFS{{Node: "w"}}},
	}
	c.kube = []runtime.Object{
		deployment("ate-api-server", "ghcr.io/agent-substrate/substrate/ateapi:v1.0.1", nil, 1),
		deployment("atenet-router", "example.com/ate/router:v1.0.1", []string{"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID("ate-system")}, 1),
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
		Issuer: c.issuer, IssuerProbe: c.probe, Devices: c.devices, UserNS: c.userns, AteFS: c.atefs,
	}
	return checker.Run(ctx)
}

var _ = Describe("Substrate checks", func() {
	It("passes on a complete cluster and runs no OpenShell or Agent Sandbox check", func(ctx SpecContext) {
		results := healthySubstrate().run(ctx)
		Expect(checks(results)).To(Equal([]string{
			"Kubernetes version", "Substrate node devices", "Substrate gVisor user namespaces", "Substrate /var/lib/ate filesystem", "Service-account issuer", "Agent Substrate", "Substrate router",
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
		c.kube[0] = deployment("ate-api-server", "example.com/ate/api-server:v1.0.1", nil, 0)
		Expect(find(c.run(ctx), "Agent Substrate").Message).To(ContainSubstring("no ready replica"))
		c = healthySubstrate()
		c.kube[0] = deployment("ate-api-server", "example.com/ate/api-server:v1.0.0", nil, 1)
		Expect(find(c.run(ctx), "Agent Substrate").Message).To(ContainSubstring("v1.0.0 is not the pinned release v1.0.1"))
		c = healthySubstrate()
		c.kube[0] = deployment("ate-api-server", "example.com/ate/api-server", nil, 1)
		untagged := find(c.run(ctx), "Agent Substrate")
		Expect(untagged.Status).To(Equal(preflight.Fail))
		Expect(untagged.Message).To(ContainSubstring("no tag"))
		// The api server is found by container name, not by position: a sidecar first does not hide it.
		c = healthySubstrate()
		d := deployment("ate-api-server", "example.com/ate/api-server:v1.0.1", nil, 1)
		d.Spec.Template.Spec.Containers = append([]corev1.Container{{Name: "objectstore-plugin", Image: "example.com/ate/plugin:v9"}}, d.Spec.Template.Spec.Containers...)
		c.kube[0] = d
		Expect(find(c.run(ctx), "Agent Substrate").Status).To(Equal(preflight.Pass))
	})

	It("fails when the router does not allow the atenet client", func(ctx SpecContext) {
		c := healthySubstrate()
		c.kube[1] = deployment("atenet-router", "example.com/ate/router:v1.0.1", nil, 1)
		r := find(c.run(ctx), "Substrate router")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Fix).To(ContainSubstring("--ingress-allowed-spiffe-ids="))
	})

	It("pins the router allow-list to the configured Substrate namespace", func(ctx SpecContext) {
		c := healthySubstrate()
		Expect(find(c.run(ctx), "Substrate router").Message).To(ContainSubstring("ns/ate-system/sa/atenet-client"))
		c.config.Substrate.Namespace = "substrate-prod"
		moved := deployment("atenet-router", "example.com/ate/router:v1.0.1", []string{"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID("ate-system")}, 1)
		moved.Namespace = "substrate-prod"
		c.kube = append(c.kube, moved)
		r := find(c.run(ctx), "Substrate router")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Fix).To(ContainSubstring("ns/substrate-prod/sa/atenet-client"))
	})

	It("fails when the router does not allow agentd's identity", func(ctx SpecContext) {
		c := healthySubstrate()
		c.config.Substrate.AgentdNamespace, c.config.Substrate.AgentdServiceAccount = "koinon", "koinon-agentd"
		r := find(c.run(ctx), "Substrate router")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(ContainSubstring("ns/koinon/sa/koinon-agentd"))
		Expect(r.Fix).To(ContainSubstring(preflight.SubstrateAgentdSPIFFEID("koinon", "koinon-agentd")))

		c = healthySubstrate()
		c.config.Substrate.AgentdNamespace, c.config.Substrate.AgentdServiceAccount = "koinon", "koinon-agentd"
		c.kube[1] = deployment("atenet-router", "example.com/ate/router:v1.0.1", []string{"--ingress-allowed-spiffe-ids=" +
			preflight.SubstrateRouterSPIFFEID("ate-system") + "," + preflight.SubstrateAgentdSPIFFEID("koinon", "koinon-agentd")}, 1)
		Expect(find(c.run(ctx), "Substrate router").Status).To(Equal(preflight.Pass))
	})

	It("fails when the router Service does not serve the port of routerAddress", func(ctx SpecContext) {
		svc := func(port int32) *corev1.Service {
			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "atenet-router", Namespace: "ate-system"},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: port}}},
			}
		}
		c := healthySubstrate()
		c.config.Substrate.RouterPort = 80
		c.kube = append(c.kube, svc(443))
		r := find(c.run(ctx), "Substrate router")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(ContainSubstring("no port 80"))
		Expect(r.Fix).To(ContainSubstring("443"))

		c = healthySubstrate()
		c.config.Substrate.RouterPort = 443
		c.kube = append(c.kube, svc(443))
		Expect(find(c.run(ctx), "Substrate router").Status).To(Equal(preflight.Pass))

		c = healthySubstrate()
		c.config.Substrate.RouterPort = 443
		Expect(find(c.run(ctx), "Substrate router").Message).To(ContainSubstring("Service ate-system/atenet-router not found"))
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
		c.devices = fakeDevices{err: fmt.Errorf("%w: FailedMount: /dev/kvm is not a character device", preflight.ErrDevicesMissing)}
		r := find(c.run(ctx), "Substrate node devices")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(And(ContainSubstring("/dev/kvm"), ContainSubstring("/dev/vhost-vsock")))
		c.config.Substrate.VMTier = false
		Expect(checks(c.run(ctx))).NotTo(ContainElement("Substrate node devices"))
	})

	It("reports the real cause when the device probe pod cannot run, not a missing /dev/kvm", func(ctx SpecContext) {
		c := healthySubstrate()
		c.devices = fakeDevices{err: errors.New(`create probe pod: namespaces "ate-system" not found`)}
		r := find(c.run(ctx), "Substrate node devices")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(ContainSubstring(`namespaces "ate-system" not found`))
		Expect(r.Message).NotTo(ContainSubstring("/dev/kvm"))
		Expect(r.Fix).NotTo(ContainSubstring("KVM"))
	})

	It("reads the version from a component image name, not the Deployment name", func(ctx SpecContext) {
		c := healthySubstrate()
		c.kube[0] = deployment("ate-api-server", "ghcr.io/agent-substrate/substrate/ateapi:v0.9.0", nil, 1)
		r := find(c.run(ctx), "Agent Substrate")
		Expect(r.Status).To(Equal(preflight.Fail))
		Expect(r.Message).To(ContainSubstring("v0.9.0"))
	})

	Describe("service-account issuer", func() {
		It("fails with the supported fixes when a pod cannot reach an IPv6 ULA issuer", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{issuer: "https://[fdae:1::1]:10001"}
			c.probe = &fakeIssuerProbe{res: IssuerResult{Detail: "dial tcp [fdae:1::1]:10001: connect: network is unreachable"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(And(
				ContainSubstring("https://[fdae:1::1]:10001"), ContainSubstring("from a pod in namespace koinon-substrate-probe"),
				ContainSubstring("invalid bearer token"), ContainSubstring("IPv6 unique-local address"),
			))
			Expect(r.Fix).To(And(
				ContainSubstring("must reach the issuer Substrate trusts"), ContainSubstring("no separate discovery URL"),
				ContainSubstring("service-account-issuer"), ContainSubstring("cannot replace it"), ContainSubstring("not a supported install"),
			))
			Expect(r.Fix).NotTo(ContainSubstring("--expected-jwt-issuer https://kubernetes.default.svc"))
			// The exact safe patch: new issuer signs, old issuer and both audiences stay accepted.
			Expect(r.Fix).To(And(
				ContainSubstring("kind: KubeServiceAccountConfig"),
				ContainSubstring("  issuerURL: https://kubernetes.default.svc\naccepted:\n  issuers:\n    - https://[fdae:1::1]:10001\n  audiences:\n    - https://kubernetes.default.svc\n    - https://[fdae:1::1]:10001"),
				ContainSubstring("--service-account-issuer=https://kubernetes.default.svc\n  --service-account-issuer=https://[fdae:1::1]:10001\n  --api-audiences=https://kubernetes.default.svc,https://[fdae:1::1]:10001"),
				ContainSubstring("one API server restart"),
			))
		})

		It("never prints the issuer-only patch and prints none when the cluster already uses the in-cluster issuer", func() {
			p := preflight.SafeIssuerPatch("", "https://old.example")
			Expect(p).To(And(ContainSubstring("accepted:"), ContainSubstring("--api-audiences=")))
			Expect(strings.Count(p, "issuerURL")).To(Equal(1))
			Expect(p).To(And(ContainSubstring("keep every entry"), ContainSubstring("--api-audiences"), ContainSubstring("accepted.audiences")))
			Expect(preflight.SafeIssuerPatch("https://kubernetes.default.svc")).To(BeEmpty())
			Expect(preflight.SafeIssuerPatch("")).To(BeEmpty())
		})

		It("fails, not warns, when the issuer is reachable only without TLS verification", func(ctx SpecContext) {
			c := healthySubstrate()
			c.probe = &fakeIssuerProbe{res: IssuerResult{Reachable: true, Unverified: true, Detail: "TLS verification failed"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring("invalid bearer token"))
			Expect(r.Fix).To(ContainSubstring("system roots"))

			// The in-cluster issuer is verified with the mounted CA alone: the fix names that CA, not the system roots.
			c.issuer = fakeIssuer{issuer: "https://kubernetes.default.svc"}
			r = find(c.run(ctx), "Service-account issuer")
			Expect(r.Fix).To(And(ContainSubstring(preflight.IssuerCAFile), ContainSubstring("serving certificate"), ContainSubstring("kubernetes.default.svc")))
			Expect(r.Fix).NotTo(ContainSubstring("system roots"))
		})

		It("fails when the probe cannot read the iss claim of its token, as it cannot tell whether Substrate accepts the tokens", func(ctx SpecContext) {
			c := healthySubstrate()
			c.probe = &fakeIssuerProbe{res: IssuerResult{Reachable: true, Detail: "ok"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring("could not read the iss claim"))
		})

		It("probes the override without the discovery document, as ate-setup does not read it then", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{err: errors.New("forbidden")}
			c.config.Substrate.ExpectedIssuer = "https://kubernetes.default.svc"
			c.probe = &fakeIssuerProbe{res: IssuerResult{Reachable: true, TokenIssuer: "https://kubernetes.default.svc", Detail: "ok"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Pass), r.Message)
			Expect(r.Message).To(ContainSubstring("could not be read: forbidden"))
			Expect(c.probe.probed).To(Equal("https://kubernetes.default.svc"))
		})

		It("fails when the override names an issuer the discovery document does not, as Kubernetes always names its first issuer", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{issuer: "https://[fdae:1::1]:10001"}
			c.config.Substrate.ExpectedIssuer = "https://kubernetes.default.svc"
			c.probe = &fakeIssuerProbe{res: IssuerResult{TokenIssuer: "https://[fdae:1::1]:10001", Detail: `discovery document: issuer "https://[fdae:1::1]:10001" differs from "https://kubernetes.default.svc"`}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(And(ContainSubstring("differs"), ContainSubstring("discovery document names https://[fdae:1::1]:10001")))
			Expect(r.Fix).To(ContainSubstring("first --service-account-issuer to https://kubernetes.default.svc"))
		})

		It("passes when the discovery issuer is the in-cluster issuer", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{issuer: "https://kubernetes.default.svc"}
			c.probe = &fakeIssuerProbe{res: IssuerResult{Reachable: true, TokenIssuer: "https://kubernetes.default.svc", Detail: "ok"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Pass), r.Message)
			Expect(c.probe.probed).To(Equal("https://kubernetes.default.svc"))
			Expect(r.Message).To(ContainSubstring("carry iss https://kubernetes.default.svc"))
		})

		It("fails when the tokens of pods carry another issuer than Substrate would trust", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{issuer: "https://[fdae:1::1]:10001"}
			c.config.Substrate.ExpectedIssuer = "https://kubernetes.default.svc"
			c.probe = &fakeIssuerProbe{res: IssuerResult{Reachable: true, TokenIssuer: "https://[fdae:1::1]:10001", Detail: "ok"}}
			r := find(c.run(ctx), "Service-account issuer")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(And(ContainSubstring("unexpected issuer"), ContainSubstring("https://[fdae:1::1]:10001")))
			// The token's iss is the one to trust, and only when pods reach it: never the in-cluster issuer as an override.
			Expect(r.Fix).To(And(
				ContainSubstring("must trust the iss the tokens carry (https://[fdae:1::1]:10001)"),
				ContainSubstring("must reach that URL"),
				ContainSubstring("first --service-account-issuer to https://kubernetes.default.svc"),
			))
			Expect(r.Fix).NotTo(ContainSubstring("--expected-jwt-issuer https://kubernetes.default.svc"))
		})

		It("fails when the discovery document cannot be read or the probe pod fails", func(ctx SpecContext) {
			c := healthySubstrate()
			c.issuer = fakeIssuer{err: errors.New("forbidden")}
			Expect(find(c.run(ctx), "Service-account issuer").Fix).To(ContainSubstring("system:service-account-issuer-discovery"))
			c = healthySubstrate()
			c.probe = &fakeIssuerProbe{err: errors.New("timed out")}
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
			want := preflight.IssuerResult{Reachable: true, Unverified: true, TokenIssuer: "https://k.example", Detail: "HTTP 200 OK"}
			res, err := preflight.ParseIssuerProbeOutput(want.String() + "\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(want))
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

	It("gives the issuer probe pod the default ServiceAccount token and no hostPath", func() {
		p := preflight.KubeSubstrateProber{Namespace: "ate-system", NamePrefix: "x", Image: "img"}.IssuerPod("https://kubernetes.default.svc")
		Expect(*p.Spec.AutomountServiceAccountToken).To(BeTrue())
		Expect(p.Spec.Volumes).To(BeEmpty())
		Expect(p.Spec.Containers[0].Args).To(ContainElements("issuer-probe", "https://kubernetes.default.svc"))
	})

	It("tells a FailedMount (a missing device) from a probe pod that cannot be created", func(ctx SpecContext) {
		client := fake.NewClientset(&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "e", Namespace: "ate-system"},
			InvolvedObject: corev1.ObjectReference{Name: "fresh"}, Type: corev1.EventTypeWarning,
			Reason: "FailedMount", Message: "hostPath type check failed: /dev/kvm is not a character device",
		})
		client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Name = "fresh"
			return false, nil, nil
		})
		p := preflight.KubeSubstrateProber{Client: client, Namespace: "ate-system", NamePrefix: "x", Image: "img", Timeout: 50 * time.Millisecond, PollInterval: time.Millisecond}
		Expect(p.ProbeDevices(ctx)).To(MatchError(preflight.ErrDevicesMissing))

		client = fake.NewClientset()
		client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "ate-system")
		})
		p.Client = client
		err := p.ProbeDevices(ctx)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, preflight.ErrDevicesMissing)).To(BeFalse())
		Expect(err.Error()).To(ContainSubstring("not found"))
	})

	Describe("device probe warnings", func() {
		warning := func(name, reason, msg string, at time.Time) *corev1.Event {
			return &corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "ate-system"},
				InvolvedObject: corev1.ObjectReference{Name: "fresh"}, Type: corev1.EventTypeWarning,
				Reason: reason, Message: msg, LastTimestamp: metav1.NewTime(at),
			}
		}
		prober := func(failed bool, events ...runtime.Object) preflight.KubeSubstrateProber {
			client := fake.NewClientset(events...)
			client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
				pod.Name = "fresh"
				if failed {
					pod.Status.Phase, pod.Status.Reason = corev1.PodFailed, "DeadlineExceeded"
				}
				return false, nil, nil
			})
			return preflight.KubeSubstrateProber{Client: client, Namespace: "ate-system", NamePrefix: "x", Image: "img", Timeout: 50 * time.Millisecond, PollInterval: time.Millisecond}
		}
		now := time.Now()

		It("does not call a FailedMount of another volume a missing device", func(ctx SpecContext) {
			p := prober(false, warning("e", "FailedMount", `MountVolume.SetUp failed for volume "webhook-ca": configmap "ca" not found`, now))
			err := p.ProbeDevices(ctx)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, preflight.ErrDevicesMissing)).To(BeFalse())
			Expect(err.Error()).To(ContainSubstring("webhook-ca"))
		})

		It("reads the warnings of a pod that failed before its container started", func(ctx SpecContext) {
			p := prober(true, warning("e", "FailedMount", "hostPath type check failed: /dev/kvm is not a character device", now))
			err := p.ProbeDevices(ctx)
			Expect(err).To(MatchError(preflight.ErrDevicesMissing))
			Expect(err.Error()).To(And(ContainSubstring("failed before its container started"), ContainSubstring("/dev/kvm")))
		})

		It("takes the newest warning by when it last occurred, not by list order", func(ctx SpecContext) {
			p := prober(false,
				warning("a-new", "FailedMount", "hostPath type check failed: /dev/kvm is not a character device", now),
				warning("b-old", "FailedScheduling", "0/3 nodes are available", now.Add(-time.Minute)),
			)
			Expect(p.ProbeDevices(ctx)).To(MatchError(preflight.ErrDevicesMissing))
		})
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
