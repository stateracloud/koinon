// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/preflight"
)

var _ = Describe("ParseProbeOutput", func() {
	It("reads the probe line among other log lines", func() {
		res, err := preflight.ParseProbeOutput("starting\nkoinon-seccomp-probe seccomp=2 filters=1 keyctl=EPERM\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(preflight.ProbeResult{Seccomp: 2, Filters: 1, Keyctl: "EPERM"}))
	})

	It("round-trips what the probe prints", func() {
		want := preflight.ProbeResult{Seccomp: 0, Filters: -1, Keyctl: "allowed"}
		Expect(preflight.ParseProbeOutput(want.String() + "\n")).To(Equal(want))
	})

	It("rejects a log without a probe line", func() {
		_, err := preflight.ParseProbeOutput("exec format error")
		Expect(err).To(MatchError(ContainSubstring("no \"koinon-seccomp-probe\" line")))
	})

	It("rejects a malformed probe line", func() {
		_, err := preflight.ParseProbeOutput("koinon-seccomp-probe filters=1")
		Expect(err).To(MatchError(ContainSubstring("malformed")))
	})
})

var _ = Describe("parseStatus", func() {
	It("reads Seccomp and Seccomp_filters", func() {
		s, f, err := preflight.ParseStatus("Name:\tkoinon-api\nNoNewPrivs:\t1\nSeccomp:\t2\nSeccomp_filters:\t6\n")
		Expect(err).NotTo(HaveOccurred())
		Expect([]int{s, f}).To(Equal([]int{2, 6}))
	})

	It("reports -1 filters on kernels without the field", func() {
		s, f, err := preflight.ParseStatus("Seccomp:\t0\n")
		Expect(err).NotTo(HaveOccurred())
		Expect([]int{s, f}).To(Equal([]int{0, -1}))
	})

	It("fails without a Seccomp field", func() {
		_, _, err := preflight.ParseStatus("Name:\tx\n")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("KubeProber", func() {
	var client *fake.Clientset

	// createAs names created pods (the fake ignores generateName) and gives them a phase.
	createAs := func(phase corev1.PodPhase, waiting *corev1.ContainerStateWaiting) {
		client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			pod.Name = pod.GenerateName + "abcde"
			pod.Status.Phase = phase
			if waiting != nil {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{Waiting: waiting}}}
			}
			return false, nil, nil
		})
	}

	prober := func() preflight.KubeProber {
		return preflight.KubeProber{
			Client: client, Namespace: "koinon", NamePrefix: "koinon-preflight-probe",
			Image: "ghcr.io/stateracloud/koinon/api:0.1.0", ImagePullSecrets: []string{"ghcr"},
			RuntimeClassName: "kata", Labels: map[string]string{"app.kubernetes.io/component": "preflight-probe"},
			Timeout: 30 * time.Second, PollInterval: 10 * time.Millisecond,
			Logs: func(_ context.Context, ns, name string) (string, error) {
				Expect(ns + "/" + name).To(Equal("koinon/koinon-preflight-probe-abcde"))
				return "koinon-seccomp-probe seccomp=2 filters=1 keyctl=EPERM\n", nil
			},
		}
	}

	BeforeEach(func() { client = fake.NewClientset() })

	It("runs an unprivileged pod with the sandbox RuntimeClass and deletes it", func(ctx SpecContext) {
		createAs(corev1.PodPending, nil)
		client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			pod, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), "koinon", "koinon-preflight-probe-abcde")
			Expect(err).NotTo(HaveOccurred())
			pod.(*corev1.Pod).Status.Phase = corev1.PodSucceeded
			return true, pod, nil
		})

		var created *corev1.Pod
		client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			created = a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
			return false, nil, nil
		})

		res, err := prober().Probe(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(preflight.ProbeResult{Seccomp: 2, Filters: 1, Keyctl: "EPERM"}))

		Expect(created.GenerateName).To(Equal("koinon-preflight-probe-"))
		Expect(*created.Spec.RuntimeClassName).To(Equal("kata"))
		Expect(*created.Spec.AutomountServiceAccountToken).To(BeFalse())
		Expect(created.Spec.ImagePullSecrets).To(Equal([]corev1.LocalObjectReference{{Name: "ghcr"}}))
		Expect(created.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
		Expect(created.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "preflight-probe"))
		ctr := created.Spec.Containers[0]
		Expect(ctr.Image).To(Equal("ghcr.io/stateracloud/koinon/api:0.1.0"))
		Expect(ctr.Args).To(Equal([]string{"preflight", "probe"}))
		Expect(*ctr.SecurityContext.Privileged).To(BeFalse())
		Expect(*ctr.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(ctr.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))

		pods, err := client.CoreV1().Pods("koinon").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pods.Items).To(BeEmpty(), "the probe pod is deleted")
	})

	It("reports why the pod did not start when it times out, and deletes it", func(ctx SpecContext) {
		createAs(corev1.PodPending, &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull denied"})
		p := prober()
		p.Timeout = 300 * time.Millisecond // only this spec waits for the timeout
		_, err := p.Probe(ctx)
		Expect(err).To(MatchError(And(
			ContainSubstring("probe pod koinon-preflight-probe-abcde still Pending"),
			ContainSubstring("ImagePullBackOff: pull denied"),
		)))
		pods, err := client.CoreV1().Pods("koinon").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pods.Items).To(BeEmpty())
	})

	It("fails on a log without a probe line", func(ctx SpecContext) {
		createAs(corev1.PodFailed, nil)
		p := prober()
		p.Logs = func(context.Context, string, string) (string, error) { return "exec format error", nil }
		_, err := p.Probe(ctx)
		Expect(err).To(MatchError(ContainSubstring("exec format error")))
	})
})
