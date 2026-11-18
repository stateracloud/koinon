// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrNotKind is returned when --platform kind is set on a cluster whose nodes are not kind nodes.
var ErrNotKind = fmt.Errorf("--platform kind is set, but the cluster is not a kind cluster")

// kindProviderPrefix starts the providerID of every kind node (kind pkg/cluster/internal/kubeadm config:
// "provider-id" is kind://<provider>/<cluster>/<node>).
const kindProviderPrefix = "kind://"

// CheckKind refuses --platform kind on a cluster that is not kind. The kind platform uses ate-setup's kind
// overlays unchanged, with host-architecture microVM assets and hostPath volumes, which break another
// cluster: the generic platform is the route for those. It reads nodes only, so it runs before anything
// is created.
func (i *Installer) CheckKind(ctx context.Context) error {
	if i.Profile.Platform != PlatformKind {
		return nil
	}
	nodes, err := i.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("%w: cannot list nodes to check: %w; nothing was installed", ErrNotKind, err)
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("%w: it has no nodes; nothing was installed", ErrNotKind)
	}
	for _, n := range nodes.Items {
		if !strings.HasPrefix(n.Spec.ProviderID, kindProviderPrefix) {
			return fmt.Errorf("%w: node %s has providerID %q (a kind node has kind://...). Use --platform generic for another Kubernetes cluster, or the kubeconfig and context of the kind cluster; nothing was installed", ErrNotKind, n.Name, n.Spec.ProviderID)
		}
	}
	return nil
}
