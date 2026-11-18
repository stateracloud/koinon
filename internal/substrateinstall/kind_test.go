// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stateracloud/koinon/internal/substrateinstall"
)

func kindNode(name, providerID string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{ProviderID: providerID}}
}

func TestKindIsRefusedOnAClusterThatIsNotKindBeforeAnythingIsInstalled(t *testing.T) {
	g := NewWithT(t)
	i, r, client := setup("37", true, []runtime.Object{kindNode("n1", "kind://docker/lab/lab-control-plane"), kindNode("n2", "talos://omni/abc")})
	i.Profile.Kind = true
	err := i.Install(t.Context())
	g.Expect(err).To(MatchError(substrateinstall.ErrNotKind))
	g.Expect(err.Error()).To(And(ContainSubstring("n2"), ContainSubstring("without --kind"), ContainSubstring("nothing was installed")))
	g.Expect(r.calls).To(BeEmpty(), "nothing was exported, built or run")
	_, getErr := client.CoreV1().Namespaces().Get(t.Context(), "ate-system", metav1.GetOptions{})
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "no namespace was created")

	// A cluster without nodes is not proven to be kind either.
	i, _, _ = setup("37", true, nil)
	i.Profile.Kind = true
	g.Expect(i.Install(t.Context())).To(MatchError(substrateinstall.ErrNotKind))
}

func TestKindIsAcceptedOnKindNodesAndIgnoredWithoutTheFlag(t *testing.T) {
	g := NewWithT(t)
	i, _, _ := setup("37", true, []runtime.Object{kindNode("n1", "kind://docker/lab/lab-control-plane")})
	i.Profile.Kind = true
	g.Expect(i.CheckKind(t.Context())).To(Succeed())
	i, _, _ = setup("37", true, []runtime.Object{kindNode("n1", "talos://omni/abc")})
	g.Expect(i.CheckKind(t.Context())).To(Succeed(), "without --kind the nodes are not read")
}
