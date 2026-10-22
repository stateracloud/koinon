// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/procoduck/koinon/internal/gateway/standalone"
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

	It("fails when the ConfigMap does not exist", func(ctx SpecContext) {
		d := standalone.ConfigMapDeliverer{Client: fake.NewClientset(), Namespace: "koinon", Name: "missing"}
		Expect(d.Deliver(ctx, []byte("x"), "r1")).To(MatchError(ContainSubstring("koinon/missing")))
	})
})
