// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrNotKind is returned when --kind is set on a cluster whose nodes are not kind nodes.
var ErrNotKind = fmt.Errorf("--kind is set, but the cluster is not a kind cluster")

// CheckKind refuses --kind on a cluster that is not kind. kind sets every node's providerID to
// kind://<provider>/<cluster>/<node> (kind pkg/cluster/internal/kubeadm config: "provider-id"); --kind makes
// ate-setup use kind overlays, host-architecture images and hostPath volumes, which break a real cluster.
// It reads nodes only, so it runs before anything is created.
func (i *Installer) CheckKind(ctx context.Context) error {
	if !i.Profile.Kind {
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
		if !strings.HasPrefix(n.Spec.ProviderID, "kind://") {
			return fmt.Errorf("%w: node %s has providerID %q (a kind node has kind://...). Run again without --kind, or with the kubeconfig and context of the kind cluster; nothing was installed", ErrNotKind, n.Name, n.Spec.ProviderID)
		}
	}
	return nil
}
