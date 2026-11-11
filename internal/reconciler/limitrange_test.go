// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/reconciler"
)

var _ = Describe("SandboxLimitRange", func() {
	const (
		gateway = "openshell"
		ws      = "kw-1"
		ns      = "openshell-openshell-kw-1"
		name    = "koinon-koinon-sandbox-limitrange"
	)

	It("creates a Container LimitRange once", func(ctx SpecContext) {
		kube := fake.NewClientset()
		p := &reconciler.SandboxLimitRange{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		lr, err := kube.CoreV1().LimitRanges(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(lr.Spec.Limits).To(HaveLen(1))
		Expect(lr.Labels).To(HaveKeyWithValue(reconciler.LimitRangeLabel, "true"))
		Expect(lr.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", reconciler.ManagedByValue))
		item := lr.Spec.Limits[0]
		Expect(item.Type).To(Equal(corev1.LimitTypeContainer))
		cpuReq, memReq := item.DefaultRequest[corev1.ResourceCPU], item.DefaultRequest[corev1.ResourceMemory]
		cpuLim, memLim := item.Default[corev1.ResourceCPU], item.Default[corev1.ResourceMemory]
		Expect((&cpuReq).Cmp(reconciler.LimitRangeCPURequest)).To(Equal(0))
		Expect((&memReq).Cmp(reconciler.LimitRangeMemoryRequest)).To(Equal(0))
		Expect((&cpuLim).Cmp(reconciler.LimitRangeCPULimit)).To(Equal(0))
		Expect((&memLim).Cmp(reconciler.LimitRangeMemoryLimit)).To(Equal(0))
		Expect(item.Min).To(BeEmpty())
		Expect(item.Max).To(BeEmpty())

		By("a second Ensure does not create again")
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		list, err := kube.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(HaveLen(1))
	})

	It("re-creates the LimitRange on the next Ensure after someone deleted it", func(ctx SpecContext) {
		kube := fake.NewClientset()
		p := &reconciler.SandboxLimitRange{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		Expect(kube.CoreV1().LimitRanges(ns).Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())

		Expect(p.Ensure(ctx, ws)).To(Succeed())
		_, err := kube.CoreV1().LimitRanges(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("repairs a LimitRange that drifted after it was created", func(ctx SpecContext) {
		kube := fake.NewClientset()
		p := &reconciler.SandboxLimitRange{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		lr, err := kube.CoreV1().LimitRanges(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		lr.Spec.Limits[0].Default[corev1.ResourceMemory] = resource.MustParse("1Gi")
		_, err = kube.CoreV1().LimitRanges(ns).Update(ctx, lr, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(p.Ensure(ctx, ws)).To(Succeed())
		lr, err = kube.CoreV1().LimitRanges(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		mem := lr.Spec.Limits[0].Default[corev1.ResourceMemory]
		Expect((&mem).Cmp(reconciler.LimitRangeMemoryLimit)).To(Equal(0))
	})

	It("LimitRange names the shape the chart's admission policy expects", func() {
		p := &reconciler.SandboxLimitRange{Name: name}
		lr := p.LimitRange(ns)
		Expect(lr.Name).To(Equal(name))
		Expect(lr.Namespace).To(Equal(ns))
		Expect(lr.Spec.Limits).To(HaveLen(1))
		Expect(lr.Spec.Limits[0].Type).To(Equal(corev1.LimitTypeContainer))
	})
})
