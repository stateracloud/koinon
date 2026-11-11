// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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

var _ = Describe("SandboxLimitRange.Ensure backoff", func() {
	var (
		creates atomic.Int32
		refuse  atomic.Bool
		now     time.Time
		p       *reconciler.SandboxLimitRange
	)
	BeforeEach(func() {
		creates.Store(0)
		refuse.Store(true)
		now = time.Unix(1_000_000, 0)
		kube := fake.NewClientset()
		kube.PrependReactor("create", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
			creates.Add(1)
			if refuse.Load() {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "limitranges"}, "lr", errors.New("policy"))
			}
			return false, nil, nil
		})
		p = &reconciler.SandboxLimitRange{
			Kube: kube, GatewayID: "openshell", Name: "koinon-koinon-sandbox-limitrange",
			Now: func() time.Time { return now },
		}
	})

	It("does not ask the API server again until the backoff has passed", func() {
		Expect(p.Ensure(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(BeEquivalentTo(1))

		By("within the first backoff (10s) the cached refusal is returned")
		for range 5 {
			now = now.Add(time.Second)
			err := p.Ensure(context.Background(), "kw-1")
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
		}
		Expect(creates.Load()).To(BeEquivalentTo(1))

		By("after it, one more attempt")
		now = now.Add(5 * time.Second)
		Expect(p.Ensure(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(BeEquivalentTo(2))
	})

	It("keeps a separate backoff per namespace, and forgets it once the LimitRange is made", func() {
		Expect(p.Ensure(context.Background(), "kw-1")).To(HaveOccurred())
		refuse.Store(false)
		Expect(p.Ensure(context.Background(), "kw-2")).To(Succeed(), "another namespace is not held back")
		now = now.Add(11 * time.Second)
		Expect(p.Ensure(context.Background(), "kw-1")).To(Succeed())
		creates.Store(0)
		Expect(p.Ensure(context.Background(), "kw-1")).To(Succeed())
		Expect(creates.Load()).To(BeZero(), "applied namespaces are not asked again")
	})

	It("does not back off when the workspace namespace is missing", func() {
		kube := fake.NewClientset()
		kube.PrependReactor("create", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
			creates.Add(1)
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "namespaces"}, "openshell-openshell-kw-1")
		})
		p = &reconciler.SandboxLimitRange{
			Kube: kube, GatewayID: "openshell", Name: "koinon-koinon-sandbox-limitrange",
			Now: func() time.Time { return now },
		}
		Expect(p.Ensure(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(BeEquivalentTo(1))
		Expect(p.Ensure(context.Background(), "kw-1")).To(HaveOccurred(), "missing namespace is retried immediately")
		Expect(creates.Load()).To(BeEquivalentTo(2))
		Expect(p.Ready(context.Background(), "kw-1")).To(BeFalse())
	})
})

var _ = Describe("SandboxLimitRange.Ready", func() {
	const (
		gateway = "openshell"
		ws      = "kw-1"
		ns      = "openshell-openshell-kw-1"
		name    = "koinon-koinon-sandbox-limitrange"
	)

	It("is false when the LimitRange is missing", func(ctx SpecContext) {
		p := &reconciler.SandboxLimitRange{Kube: fake.NewClientset(), GatewayID: gateway, Name: name}
		Expect(p.Ready(ctx, ws)).To(BeFalse())
	})

	It("is true only when the LimitRange has the sized shape", func(ctx SpecContext) {
		kube := fake.NewClientset()
		p := &reconciler.SandboxLimitRange{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		Expect(p.Ready(ctx, ws)).To(BeTrue())
		lr, err := kube.CoreV1().LimitRanges(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		lr.Spec.Limits[0].Default[corev1.ResourceMemory] = resource.MustParse("1Gi")
		_, err = kube.CoreV1().LimitRanges(ns).Update(ctx, lr, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Ready(ctx, ws)).To(BeFalse(), "a drifted LimitRange is not ready")
	})
})
