// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/reconciler"
)

var _ = Describe("SandboxNetworkPolicy", func() {
	const (
		gateway = "openshell"
		ws      = "kw-1"
		ns      = "openshell-openshell-kw-1"
		name    = "koinon-koinon-sandbox-default-deny"
	)

	It("creates a namespace-wide default-deny NetworkPolicy once", func(ctx SpecContext) {
		kube := fake.NewClientset()
		p := &reconciler.SandboxNetworkPolicy{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		np, err := kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(np.Spec.PodSelector.MatchLabels).To(BeEmpty())
		Expect(np.Spec.PodSelector.MatchExpressions).To(BeEmpty())
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		Expect(np.Spec.Ingress).To(BeEmpty())
		Expect(np.Spec.Egress).To(BeEmpty())
		Expect(np.Labels).To(HaveKeyWithValue(reconciler.NetPolLabel, "true"))
		Expect(np.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", reconciler.ManagedByValue))

		By("a second Ensure does not create again")
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		list, err := kube.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(HaveLen(1))
	})

	It("re-creates the policy on the next Ensure (the next pass) after someone deleted it", func(ctx SpecContext) {
		kube := fake.NewClientset()
		p := &reconciler.SandboxNetworkPolicy{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		Expect(kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())

		Expect(p.Ensure(ctx, ws)).To(Succeed())
		np, err := kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		Expect(np.Spec.Egress).To(BeEmpty())
	})

	It("repairs a policy that drifted after it was created", func(ctx SpecContext) {
		kube := fake.NewClientset()
		p := &reconciler.SandboxNetworkPolicy{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		np, err := kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
		_, err = kube.NetworkingV1().NetworkPolicies(ns).Update(ctx, np, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(p.Ensure(ctx, ws)).To(Succeed())
		np, err = kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(np.Spec.Egress).To(BeEmpty())
	})

	It("repairs a drifted policy of the same name", func(ctx SpecContext) {
		kube := fake.NewClientset(&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress:     []networkingv1.NetworkPolicyIngressRule{{}},
			},
		})
		p := &reconciler.SandboxNetworkPolicy{Kube: kube, GatewayID: gateway, Name: name}
		Expect(p.Ensure(ctx, ws)).To(Succeed())
		np, err := kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(np.Spec.PodSelector.MatchLabels).To(BeEmpty())
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		Expect(np.Spec.Ingress).To(BeEmpty())
		Expect(np.Spec.Egress).To(BeEmpty())
	})

	It("Policy names the default-deny shape the chart's admission policy expects", func() {
		p := &reconciler.SandboxNetworkPolicy{Name: name}
		np := p.Policy(ns)
		Expect(np.Name).To(Equal(name))
		Expect(np.Namespace).To(Equal(ns))
		Expect(np.Spec.PodSelector.MatchLabels).To(BeEmpty())
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		Expect(np.Spec.Ingress).To(BeNil())
		Expect(np.Spec.Egress).To(BeNil())
	})

	It("counts a missing namespace without backing off, so the next Ensure can write the policy", func(ctx SpecContext) {
		kube := fake.NewClientset()
		missing := true
		kube.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
			if missing {
				return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
			}
			return false, nil, nil
		})
		errs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "t_errors"}, []string{"reason"})
		p := &reconciler.SandboxNetworkPolicy{Kube: kube, GatewayID: gateway, Name: name, Errors: errs}
		err := p.Ensure(ctx, ws)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(testutil.ToFloat64(errs.WithLabelValues("not_found"))).To(BeZero(), "expected before the first sandbox")
		p.CountError(err)
		Expect(testutil.ToFloat64(errs.WithLabelValues("not_found"))).To(Equal(1.0))
		missing = false
		Expect(p.Ensure(ctx, ws)).To(Succeed(), "no backoff after a missing namespace")
	})

	It("counts a refused Create by reason", func(ctx SpecContext) {
		kube := fake.NewClientset()
		kube.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "networkpolicies"}, name, nil)
		})
		errs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "t_errors"}, []string{"reason"})
		p := &reconciler.SandboxNetworkPolicy{Kube: kube, GatewayID: gateway, Name: name, Errors: errs}
		Expect(p.Ensure(ctx, ws)).To(HaveOccurred())
		Expect(testutil.ToFloat64(errs.WithLabelValues("forbidden"))).To(Equal(1.0))
	})
})
