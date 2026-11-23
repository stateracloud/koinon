// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/preflight"
)

// Issue #1176: the Substrate probe pods run in a namespace of their own. Substrate's admission policies refuse a
// bare pod in its namespaces, and they keep no exception for Koinon's probes.
var _ = Describe("Substrate probe namespace", func() {
	It("defaults to a namespace that is not one of Substrate's", func() {
		sub := preflight.SubstrateConfig{}.WithDefaults()
		Expect(sub.ProbeNamespace).To(Equal(preflight.DefaultSubstrateProbeNamespace))
		Expect(sub.ProbeNamespace).NotTo(BeElementOf(sub.Namespace, sub.WorkerNamespace))
		Expect(sub.CheckProbeNamespace()).To(Succeed())
	})

	It("refuses a probe namespace that is a Substrate namespace, with the default names or others", func() {
		for _, sub := range []preflight.SubstrateConfig{
			{ProbeNamespace: "ate-system"},
			{ProbeNamespace: "ate-workers"},
			{Namespace: "sub", ProbeNamespace: "sub"},
			{WorkerNamespace: "wk", ProbeNamespace: "wk"},
		} {
			Expect(sub.CheckProbeNamespace()).To(MatchError(ContainSubstring("is a Substrate namespace")), "%+v", sub)
		}
		Expect(preflight.SubstrateConfig{Namespace: "sub", ProbeNamespace: "ate-system"}.CheckProbeNamespace()).To(Succeed())
	})

	It("creates the namespace with Pod Security privileged, waits for it, and removes it", func(ctx SpecContext) {
		client := fake.NewClientset()
		ns := preflight.ProbeNamespace{Client: client, Name: "probes", PollInterval: time.Millisecond, Timeout: 50 * time.Millisecond}
		created, err := ns.Ensure(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		got, err := client.CoreV1().Namespaces().Get(ctx, "probes", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, mode := range []string{"enforce", "warn", "audit"} {
			Expect(got.Labels).To(HaveKeyWithValue("pod-security.kubernetes.io/"+mode, "privileged"))
		}
		Expect(got.Labels).To(HaveKeyWithValue("app.kubernetes.io/part-of", "koinon"))
		Expect(got.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", preflight.ProbeNamespaceComponent))

		// The controllers have not filled it yet: the wait names what is missing.
		Expect(ns.WaitReady(ctx)).To(MatchError(And(ContainSubstring("not ready in time"), ContainSubstring("ServiceAccount default"), ContainSubstring("ConfigMap kube-root-ca.crt"))))
		_, err = client.CoreV1().ServiceAccounts("probes").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = client.CoreV1().ConfigMaps("probes").Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt"}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ns.WaitReady(ctx)).To(Succeed())

		Expect(ns.Remove(ctx)).To(Succeed())
		_, err = client.CoreV1().Namespaces().Get(ctx, "probes", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(ns.Remove(ctx)).To(Succeed(), "a namespace that is already gone is not an error")
	})

	It("leaves an existing namespace and its labels alone", func(ctx SpecContext) {
		client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "probes", Labels: map[string]string{"owner": "ops"}}})
		created, err := preflight.ProbeNamespace{Client: client, Name: "probes"}.Ensure(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		got, err := client.CoreV1().Namespaces().Get(ctx, "probes", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Labels).To(Equal(map[string]string{"owner": "ops"}))
	})

	It("takes over a probe namespace that an earlier run left behind, so this run removes it", func(ctx SpecContext) {
		client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "probes", Labels: map[string]string{
			"app.kubernetes.io/part-of": "koinon", "app.kubernetes.io/component": preflight.ProbeNamespaceComponent,
		}}})
		owned, err := preflight.ProbeNamespace{Client: client, Name: "probes"}.Ensure(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(owned).To(BeTrue())
	})

	It("waits until a terminating namespace is gone, then creates it again", func(ctx SpecContext) {
		client := fake.NewClientset(&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "probes"},
			Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
		})
		ns := preflight.ProbeNamespace{Client: client, Name: "probes", PollInterval: time.Millisecond, Timeout: 50 * time.Millisecond}
		_, err := ns.Ensure(ctx)
		Expect(err).To(MatchError(ContainSubstring("still terminating")))

		go func() {
			defer GinkgoRecover()
			time.Sleep(20 * time.Millisecond)
			Expect(client.CoreV1().Namespaces().Delete(ctx, "probes", metav1.DeleteOptions{})).To(Succeed())
		}()
		ns.Timeout = 5 * time.Second
		created, err := ns.Ensure(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		got, err := client.CoreV1().Namespaces().Get(ctx, "probes", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Labels).To(HaveKeyWithValue("pod-security.kubernetes.io/enforce", "privileged"))
	})

	It("builds every probe pod in the prober's namespace", func() {
		p := preflight.KubeSubstrateProber{Namespace: preflight.DefaultSubstrateProbeNamespace, NamePrefix: "t", Image: "img"}
		for _, pod := range []*corev1.Pod{p.IssuerPod("https://kubernetes.default.svc"), p.DevicesPod(), p.UserNamespacesPod("node-a")} {
			Expect(pod.Namespace).To(Equal(preflight.DefaultSubstrateProbeNamespace))
		}
	})
})
