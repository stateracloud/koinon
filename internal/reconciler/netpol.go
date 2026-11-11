// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/project"
)

// NetPolLabel marks the default-deny NetworkPolicies the reconciler creates in sandbox
// namespaces (spike 8.15): kubectl delete networkpolicy -A -l koinon.dev/sandbox-network-policy=true
// removes them.
var NetPolLabel = project.Domain + "/sandbox-network-policy"

const netpolCallTimeout = 5 * time.Second

// SandboxNetworkPolicy applies Koinon's default-deny NetworkPolicy in each OpenShell workspace
// namespace (spike 8.15, ADR 0045). Standard networking.k8s.io/v1 only: empty podSelector,
// policyTypes Ingress and Egress, no allow rules. OpenShell's own workload and supervisor
// policies are additive with it.
type SandboxNetworkPolicy struct {
	Kube kubernetes.Interface
	// GatewayID is OpenShell's gateway_id (openshell.gatewayId), which names the workspace namespaces.
	GatewayID string
	// Name is the NetworkPolicy name in every workspace namespace (chart: koinon.sandboxDefaultDeny).
	Name string
	Log  *slog.Logger

	Now func() time.Time

	mu     sync.Mutex
	denied map[string]*denial
}

func (p *SandboxNetworkPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *SandboxNetworkPolicy) namespace(workspace string) string {
	return execution.WorkspaceNamespace(p.GatewayID, workspace)
}

// Policy is the default-deny NetworkPolicy for one sandbox namespace.
func (p *SandboxNetworkPolicy) Policy(namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": ManagedByValue,
				NetPolLabel:                    "true",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

// Ensure converges the default-deny NetworkPolicy in the workspace's namespace: it is created when
// missing (also after someone deleted it) and its shape is repaired when it drifted. The
// reconciler calls it once per pass for each active workspace, so it reads the object every time
// (one GET when nothing changed) instead of caching that it was done. A namespace OpenShell has not
// created yet, or a denied write, is retried after a backoff.
func (p *SandboxNetworkPolicy) Ensure(ctx context.Context, workspace string) error {
	ns := p.namespace(workspace)
	p.mu.Lock()
	d := p.denied[ns]
	p.mu.Unlock()
	if d != nil && p.now().Before(d.until) {
		return d.err
	}
	ctx, cancel := context.WithTimeout(ctx, netpolCallTimeout)
	defer cancel()
	want := p.Policy(ns)
	nps := p.Kube.NetworkingV1().NetworkPolicies(ns)
	cur, err := nps.Get(ctx, p.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = nps.Create(ctx, want, metav1.CreateOptions{FieldManager: ManagedByValue})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return p.fail(ns, d, err)
		}
		if err == nil {
			p.log().Info("applied the sandbox default-deny NetworkPolicy", "namespace", ns, "name", p.Name)
		}
	case err != nil:
		return p.fail(ns, d, err)
	case !defaultDenyShape(cur):
		cur.Spec = want.Spec
		if cur.Labels == nil {
			cur.Labels = map[string]string{}
		}
		cur.Labels[NetPolLabel] = "true"
		cur.Labels["app.kubernetes.io/managed-by"] = ManagedByValue
		if _, err := nps.Update(ctx, cur, metav1.UpdateOptions{FieldManager: ManagedByValue}); err != nil {
			return p.fail(ns, d, err)
		}
		p.log().Warn("repaired the sandbox default-deny NetworkPolicy", "namespace", ns, "name", p.Name)
	}
	if d != nil {
		p.mu.Lock()
		delete(p.denied, ns)
		p.mu.Unlock()
	}
	return nil
}

func defaultDenyShape(np *networkingv1.NetworkPolicy) bool {
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		return false
	}
	if len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 {
		return false
	}
	have := map[networkingv1.PolicyType]bool{}
	for _, t := range np.Spec.PolicyTypes {
		have[t] = true
	}
	return have[networkingv1.PolicyTypeIngress] && have[networkingv1.PolicyTypeEgress] && len(np.Spec.PolicyTypes) == 2
}

func (p *SandboxNetworkPolicy) fail(ns string, d *denial, err error) error {
	p.mu.Lock()
	if p.denied == nil {
		p.denied = map[string]*denial{}
	}
	failures := 1
	if d != nil {
		failures = d.failures + 1
	}
	p.denied[ns] = &denial{failures: failures, until: p.now().Add(observerBackoff(failures)), err: err}
	p.mu.Unlock()
	p.log().Warn("sandbox default-deny NetworkPolicy could not be applied", "namespace", ns, "err", err)
	return err
}

func (p *SandboxNetworkPolicy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// ErrNetworkPolicyConfig is returned for a writer that lacks what it needs.
var ErrNetworkPolicyConfig = errors.New("reconciler: the sandbox NetworkPolicy writer needs a Kubernetes client and a policy name")

func (p *SandboxNetworkPolicy) validate() error {
	if p.Kube == nil || p.Name == "" {
		return ErrNetworkPolicyConfig
	}
	return nil
}
