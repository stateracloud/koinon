// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

var _ = Describe("ConfigMapDeliverer", func() {
	It("merge-patches only its data key", func(ctx SpecContext) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "agentgateway-config", Namespace: "koinon",
				Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
				Annotations: map[string]string{"argocd.argoproj.io/tracking-id": "koinon:/ConfigMap:koinon/agentgateway-config"},
			},
			Data: map[string]string{"config.yaml": "old", "other": "kept"},
		}
		client := fake.NewClientset(cm)
		var patches []k8stesting.PatchAction
		client.PrependReactor("patch", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
			patches = append(patches, a.(k8stesting.PatchAction))
			return false, nil, nil
		})

		d := standalone.ConfigMapDeliverer{Client: client, Namespace: "koinon", Name: "agentgateway-config"}
		Expect(d.Deliver(ctx, []byte("new: config\n"), "r1")).To(Succeed())

		Expect(patches).To(HaveLen(1))
		Expect(patches[0].GetPatchType()).To(Equal(types.MergePatchType))
		Expect(string(patches[0].GetPatch())).To(MatchJSON(`{"data":{"config.yaml":"new: config\n"}}`))

		got, err := client.CoreV1().ConfigMaps("koinon").Get(ctx, "agentgateway-config", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Data).To(Equal(map[string]string{"config.yaml": "new: config\n", "other": "kept"}))
		Expect(got.Labels).To(Equal(cm.Labels))
		Expect(got.Annotations).To(Equal(cm.Annotations))
	})

	It("creates the ConfigMap when it does not exist, owned by the gateway Deployment", func(ctx SpecContext) {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "koinon-gateway", Namespace: "koinon", UID: "uid-of-the-deployment"}}
		client := fake.NewClientset(dep)
		labels := map[string]string{"koinon.dev/owner": "reconciler"}
		d := standalone.ConfigMapDeliverer{
			Client: client, Namespace: "koinon", Name: "koinon-gateway-config",
			Labels: labels, OwnerDeployment: "koinon-gateway",
		}
		Expect(d.Deliver(ctx, []byte("new: config\n"), "r1")).To(Succeed())

		got, err := client.CoreV1().ConfigMaps("koinon").Get(ctx, "koinon-gateway-config", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Data).To(Equal(map[string]string{"config.yaml": "new: config\n"}))
		Expect(got.Labels).To(Equal(labels))
		Expect(got.OwnerReferences).To(HaveLen(1))
		ref := got.OwnerReferences[0]
		Expect(ref.Kind).To(Equal("Deployment"))
		Expect(ref.APIVersion).To(Equal("apps/v1"))
		Expect(ref.Name).To(Equal("koinon-gateway"))
		Expect(ref.UID).To(Equal(dep.UID))
		Expect(ref.Controller).To(HaveValue(BeFalse()))
		Expect(ref.BlockOwnerDeletion).To(HaveValue(BeFalse()))

		By("patching, not recreating, on the next delivery")
		Expect(d.Deliver(ctx, []byte("newer: config\n"), "r2")).To(Succeed())
		got, err = client.CoreV1().ConfigMaps("koinon").Get(ctx, "koinon-gateway-config", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Data["config.yaml"]).To(Equal("newer: config\n"))
		Expect(got.OwnerReferences).To(HaveLen(1))
	})

	It("creates the ConfigMap without an owner when none is named", func(ctx SpecContext) {
		client := fake.NewClientset()
		d := standalone.ConfigMapDeliverer{Client: client, Namespace: "koinon", Name: "cm"}
		Expect(d.Deliver(ctx, []byte("x"), "r1")).To(Succeed())
		got, err := client.CoreV1().ConfigMaps("koinon").Get(ctx, "cm", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.OwnerReferences).To(BeEmpty())
	})

	It("fails, creating nothing, when the owner Deployment does not exist", func(ctx SpecContext) {
		client := fake.NewClientset()
		d := standalone.ConfigMapDeliverer{Client: client, Namespace: "koinon", Name: "cm", OwnerDeployment: "missing"}
		Expect(d.Deliver(ctx, []byte("x"), "r1")).To(MatchError(ContainSubstring("owner Deployment missing")))
		_, err := client.CoreV1().ConfigMaps("koinon").Get(ctx, "cm", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("patches again when the ConfigMap appears between the patch and the create", func(ctx SpecContext) {
		client := fake.NewClientset()
		patches := 0
		client.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			patches++
			if patches == 1 {
				return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "cm")
			}
			return true, &corev1.ConfigMap{}, nil
		})
		client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "cm")
		})
		d := standalone.ConfigMapDeliverer{Client: client, Namespace: "koinon", Name: "cm"}
		Expect(d.Deliver(ctx, []byte("x"), "r1")).To(Succeed())
		Expect(patches).To(Equal(2))
	})
})
