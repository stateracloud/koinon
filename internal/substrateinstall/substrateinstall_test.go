// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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
	"github.com/stateracloud/koinon/internal/substrateinstall"
)

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

type runner struct {
	calls [][]string
	err   error
	// What the pin probes report: the ate-setup --version line, the checkout's HEAD and its git status.
	version, head, dirty string
	dirs                 []string
}

func (r *runner) Run(_ context.Context, dir, name string, args []string, _ io.Writer) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	r.dirs = append(r.dirs, dir)
	return r.err
}

func (r *runner) Output(_ context.Context, _, name string, args []string) (string, error) {
	switch {
	case name == "git" && args[0] == "rev-parse":
		return r.head + "\n", nil
	case name == "git":
		return r.dirty, nil
	}
	return r.version, nil
}

type issuer struct{ url string }

func (i issuer) Issuer(context.Context) (string, error) { return i.url, nil }

type probe struct{ reachable bool }

func (p probe) ProbeIssuer(context.Context, string) (preflight.IssuerResult, error) {
	return preflight.IssuerResult{Reachable: p.reachable, Detail: "x"}, nil
}

func crd(name string, established bool) *unstructured.Unstructured {
	status := "False"
	if established {
		status = "True"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1alpha1", "served": true, "storage": true}}},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": status}}},
	}}
}

func router(args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "atenet-router", Namespace: "ate-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "router", Args: args}},
		}}},
	}
}

func setup(minor string, reachable bool, kube []runtime.Object, crds ...runtime.Object) (*substrateinstall.Installer, *runner, *fake.Clientset) {
	client := fake.NewClientset(kube...)
	client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: minor, GitVersion: "v1." + minor}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crds...)
	r := &runner{
		version: "ate-setup version go.ate.dev/substrate.Version=" + substrateinstall.PinnedVersion + " commit=" + substrateinstall.PinnedCommit + " built=x darwin/arm64",
		head:    substrateinstall.PinnedCommit,
	}
	checker := &preflight.Checker{
		Kube: client, Dynamic: dyn, CallTimeout: time.Second,
		Issuer: issuer{"https://issuer.example.com"}, IssuerProbe: probe{reachable},
		Config: preflight.Config{Substrate: &preflight.SubstrateConfig{}},
	}
	return &substrateinstall.Installer{
		Kube: client, Dynamic: dyn, Checker: checker, Runner: r, SetupBinary: "/opt/ate-setup", SourceDir: "/src/substrate",
		Profile:      substrateinstall.Profile{StorageBucket: "snapshots"},
		Out:          io.Discard,
		PollInterval: time.Millisecond, WaitTimeout: 50 * time.Millisecond,
	}, r, client
}

func TestStopsBeforeInstallingWhenAPrerequisiteFails(t *testing.T) {
	g := NewWithT(t)
	for name, tc := range map[string]struct {
		minor     string
		reachable bool
	}{"kubernetes below 1.37": {"36", true}, "unreachable issuer": {"37", false}} {
		i, r, _ := setup(tc.minor, tc.reachable, nil)
		err := i.Install(t.Context())
		g.Expect(err).To(MatchError(substrateinstall.ErrPrerequisites), name)
		g.Expect(r.calls).To(BeEmpty(), name)
	}
}

func TestRunsSetupThenWaitsForCRDs(t *testing.T) {
	g := NewWithT(t)
	var crds []runtime.Object
	for _, n := range preflight.SubstrateCRDs {
		crds = append(crds, crd(n, true))
	}
	i, r, _ := setup("37", true, []runtime.Object{router()}, crds...)
	// The CRDs are present but Substrate's api server is not: the full preflight after the install fails, setup ran once.
	err := i.Install(t.Context())
	g.Expect(err).To(MatchError(preflight.ErrFailed))
	g.Expect(r.calls).To(HaveLen(1))
	g.Expect(r.calls[0][0]).To(Equal("/opt/ate-setup"))
	g.Expect(r.dirs[0]).To(Equal("/src/substrate"))
	g.Expect(strings.Join(r.calls[0], " ")).To(And(
		ContainSubstring("deploy ate-system"), ContainSubstring("--storage-bucket-name=snapshots"),
		ContainSubstring("--image-tag="+substrateinstall.PinnedVersion),
		ContainSubstring("--ingress-allowed-spiffe-ids="+preflight.SubstrateRouterSPIFFEID("ate-system"))))
}

func TestRefusesAnUnpinnedBinaryOrCheckout(t *testing.T) {
	g := NewWithT(t)
	for name, mutate := range map[string]func(*runner){
		"other version": func(r *runner) { r.version = strings.Replace(r.version, "Version=v1.0.0", "Version=v1.0.1", 1) },
		"other commit": func(r *runner) {
			r.version = strings.Replace(r.version, "commit="+substrateinstall.PinnedCommit, "commit=deadbeef", 1)
		},
		"checkout moved":  func(r *runner) { r.head = "deadbeef" },
		"checkout edited": func(r *runner) { r.dirty = " M manifests/ate-install/ate-api-server.yaml\n" },
	} {
		i, r, _ := setup("37", true, nil)
		mutate(r)
		g.Expect(i.Install(t.Context())).To(MatchError(substrateinstall.ErrPin), name)
		g.Expect(r.calls).To(BeEmpty(), name)
	}
}

func TestThePinIsComplete(t *testing.T) {
	g := NewWithT(t)
	g.Expect(substrateinstall.PinnedVersion).To(MatchRegexp(`^v[0-9]+\.[0-9]+\.[0-9]+$`))
	g.Expect(substrateinstall.PinnedCommit).To(MatchRegexp(`^[0-9a-f]{40}$`))
}

// The SPIFFE identity follows the configured Substrate namespace.
func TestSPIFFEIDFollowsTheNamespace(t *testing.T) {
	g := NewWithT(t)
	g.Expect(preflight.SubstrateRouterSPIFFEID("substrate-prod")).To(Equal("spiffe://cluster.local/ns/substrate-prod/sa/atenet-client"))
	args := substrateinstall.Profile{StorageBucket: "b"}.SetupArgs("substrate-prod")
	g.Expect(args).To(ContainElements("--namespace=substrate-prod",
		"--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/substrate-prod/sa/atenet-client"))
}

func TestEnsureRouterSPIFFEUsesTheConfiguredNamespace(t *testing.T) {
	g := NewWithT(t)
	i, _, client := setup("37", true, nil)
	i.Checker.Config.Substrate.Namespace = "substrate-prod"
	r := router()
	r.Namespace = "substrate-prod"
	_, err := client.AppsV1().Deployments("substrate-prod").Create(t.Context(), r, metav1.CreateOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(Succeed())
	d, err := client.AppsV1().Deployments("substrate-prod").Get(t.Context(), "atenet-router", metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.Join(d.Spec.Template.Spec.Containers[0].Args, " ")).To(And(
		ContainSubstring("ns/substrate-prod/sa/atenet-client"), Not(ContainSubstring("ns/ate-system"))))
}

func TestSetupFailureStopsTheStep(t *testing.T) {
	g := NewWithT(t)
	i, r, _ := setup("37", true, nil)
	r.err = errors.New("exit status 1")
	g.Expect(i.Install(t.Context())).To(MatchError(ContainSubstring("exit status 1")))
}

func TestWaitCRDsTimesOutOnAnUnestablishedCRD(t *testing.T) {
	g := NewWithT(t)
	crds := []runtime.Object{crd(preflight.SubstrateCRDs[0], true), crd(preflight.SubstrateCRDs[1], false)}
	i, _, _ := setup("37", true, nil, crds...)
	err := i.WaitCRDs(t.Context())
	g.Expect(err).To(MatchError(ContainSubstring(preflight.SubstrateCRDs[1])))
	g.Expect(err).To(MatchError(ContainSubstring(preflight.SubstrateCRDs[2])))
	g.Expect(err.Error()).NotTo(ContainSubstring(preflight.SubstrateCRDs[0]))
}

func TestEnsureRouterSPIFFEIsIdempotent(t *testing.T) {
	g := NewWithT(t)
	for name, args := range map[string][]string{
		"no flag":       nil,
		"other id":      {"--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/x/sa/y"},
		"already there": {"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID("ate-system")},
	} {
		i, _, client := setup("37", true, []runtime.Object{router(args...)})
		for range 2 {
			g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(Succeed(), name)
		}
		d, err := client.AppsV1().Deployments("ate-system").Get(t.Context(), "atenet-router", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		got := strings.Join(d.Spec.Template.Spec.Containers[0].Args, " ")
		g.Expect(strings.Count(got, preflight.SubstrateRouterSPIFFEID("ate-system"))).To(Equal(1), name)
		if name == "other id" {
			g.Expect(got).To(ContainSubstring("spiffe://cluster.local/ns/x/sa/y,"))
		}
	}
}

func TestProfileNamesWhatIsMissing(t *testing.T) {
	g := NewWithT(t)
	g.Expect(substrateinstall.Profile{}.Validate()).To(MatchError(ContainSubstring("storage bucket")))
}

// A fresh cluster has no ate-system: ate-setup creates it. The prerequisite probes run pods there
// first, so the installer must create the namespace before them. The fake clientset does not enforce
// namespaces, so this test adds the API server's rule.
func TestCreatesTheNamespaceBeforeTheProbePodsRun(t *testing.T) {
	g := NewWithT(t)
	i, r, client := setup("37", true, nil)
	client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca := a.(k8stesting.CreateAction)
		// The tracker, not the clientset: a reactor that calls the clientset deadlocks on its lock.
		nsGVR := corev1.SchemeGroupVersion.WithResource("namespaces")
		if _, err := client.Tracker().Get(nsGVR, "", ca.GetNamespace()); apierrors.IsNotFound(err) {
			return true, nil, apierrors.NewNotFound(corev1.Resource("namespaces"), ca.GetNamespace())
		}
		pod := ca.GetObject().(*corev1.Pod)
		pod.Name = "probe"
		pod.Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})
	i.Checker.IssuerProbe = preflight.KubeSubstrateProber{
		Client: client, Namespace: "ate-system", NamePrefix: "t", Image: "img", PollInterval: time.Millisecond, Timeout: time.Second,
		Logs: func(context.Context, string, string) (string, error) {
			return preflight.IssuerResult{Reachable: true, Detail: "ok"}.String() + "\n" + preflight.SubstrateDevicesProbeMarker + "\n", nil
		},
	}
	i.Checker.Devices = i.Checker.IssuerProbe.(preflight.KubeSubstrateProber)
	i.Checker.Config.Substrate.VMTier = true
	_ = i.Install(t.Context()) // later checks fail: Substrate itself is not installed in the fake
	g.Expect(r.calls).To(HaveLen(1), "the prerequisites passed, so ate-setup ran")
	ns, err := client.CoreV1().Namespaces().Get(t.Context(), "ate-system", metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ns.Labels).To(HaveKeyWithValue("pod-security.kubernetes.io/enforce", "privileged"))
}
