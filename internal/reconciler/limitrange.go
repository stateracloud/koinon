// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/project"
)

// LimitRangeLabel marks the LimitRanges the reconciler creates in sandbox namespaces (#76):
// kubectl delete limitrange -A -l koinon.dev/sandbox-limit-range=true removes them.
var LimitRangeLabel = project.Domain + "/sandbox-limit-range"

const limitRangeCallTimeout = 5 * time.Second

// Default sizes for unsized containers in OpenShell workspace namespaces (#76, ADR 0083 follow-up).
// OpenShell cannot size the supervisor or its init containers; a LimitRange supplies requests and
// limits so they are no longer BestEffort. Kept in sync with the chart's ValidatingAdmissionPolicy
// shape check (templates/sandbox-limitrange.yaml).
var (
	LimitRangeCPURequest    = resource.MustParse("50m")
	LimitRangeCPULimit      = resource.MustParse("250m")
	LimitRangeMemoryRequest = resource.MustParse("64Mi")
	LimitRangeMemoryLimit   = resource.MustParse("256Mi")
)

// SandboxLimitRange applies Koinon's LimitRange in each OpenShell workspace namespace (#76): default
// requests and limits for type Container (which also covers init containers) so the supervisor and
// init containers are Burstable instead of BestEffort.
type SandboxLimitRange struct {
	Kube kubernetes.Interface
	// GatewayID is OpenShell's gateway_id (openshell.gatewayId), which names the workspace namespaces.
	GatewayID string
	// Name is the LimitRange name in every workspace namespace (chart: koinon.sandboxLimitRange).
	Name string
	Log  *slog.Logger

	Now func() time.Time

	mu     sync.Mutex
	denied map[string]*denial
}

func (p *SandboxLimitRange) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *SandboxLimitRange) namespace(workspace string) string {
	return execution.WorkspaceNamespace(p.GatewayID, workspace)
}

// LimitRange is the sized LimitRange for one sandbox namespace.
func (p *SandboxLimitRange) LimitRange(namespace string) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": ManagedByValue,
				LimitRangeLabel:                "true",
			},
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				Default:        limitRangeItem.Default,
				DefaultRequest: limitRangeItem.DefaultRequest,
			}},
		},
	}
}

var limitRangeItem = corev1.LimitRangeItem{
	DefaultRequest: corev1.ResourceList{
		corev1.ResourceCPU:    LimitRangeCPURequest,
		corev1.ResourceMemory: LimitRangeMemoryRequest,
	},
	Default: corev1.ResourceList{
		corev1.ResourceCPU:    LimitRangeCPULimit,
		corev1.ResourceMemory: LimitRangeMemoryLimit,
	},
}

// Ensure converges the LimitRange in the workspace's namespace: it is created when missing and its
// shape is repaired when it drifted. A namespace OpenShell has not created yet, or a denied write,
// is retried after a backoff.
func (p *SandboxLimitRange) Ensure(ctx context.Context, workspace string) error {
	ns := p.namespace(workspace)
	p.mu.Lock()
	d := p.denied[ns]
	p.mu.Unlock()
	if d != nil && p.now().Before(d.until) {
		return d.err
	}
	ctx, cancel := context.WithTimeout(ctx, limitRangeCallTimeout)
	defer cancel()
	want := p.LimitRange(ns)
	lrs := p.Kube.CoreV1().LimitRanges(ns)
	cur, err := lrs.Get(ctx, p.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = lrs.Create(ctx, want, metav1.CreateOptions{FieldManager: ManagedByValue})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return p.fail(ns, d, err)
		}
		if err == nil {
			p.log().Info("applied the sandbox LimitRange", "namespace", ns, "name", p.Name)
		}
	case err != nil:
		return p.fail(ns, d, err)
	case !limitRangeShape(cur):
		cur.Spec = want.Spec
		if cur.Labels == nil {
			cur.Labels = map[string]string{}
		}
		cur.Labels[LimitRangeLabel] = "true"
		cur.Labels["app.kubernetes.io/managed-by"] = ManagedByValue
		if _, err := lrs.Update(ctx, cur, metav1.UpdateOptions{FieldManager: ManagedByValue}); err != nil {
			return p.fail(ns, d, err)
		}
		p.log().Warn("repaired the sandbox LimitRange", "namespace", ns, "name", p.Name)
	}
	if d != nil {
		p.mu.Lock()
		delete(p.denied, ns)
		p.mu.Unlock()
	}
	return nil
}

func limitRangeShape(lr *corev1.LimitRange) bool {
	if len(lr.Spec.Limits) != 1 {
		return false
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		return false
	}
	if !resourceListEqual(item.DefaultRequest, limitRangeItem.DefaultRequest) {
		return false
	}
	if !resourceListEqual(item.Default, limitRangeItem.Default) {
		return false
	}
	if len(item.Min) != 0 || len(item.Max) != 0 || len(item.MaxLimitRequestRatio) != 0 {
		return false
	}
	return true
}

func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av.Cmp(bv) != 0 {
			return false
		}
	}
	return true
}

func (p *SandboxLimitRange) fail(ns string, d *denial, err error) error {
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
	p.log().Warn("sandbox LimitRange could not be applied", "namespace", ns, "err", err)
	return err
}

func (p *SandboxLimitRange) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// ErrLimitRangeConfig is returned for a writer that lacks what it needs.
var ErrLimitRangeConfig = errors.New("reconciler: the sandbox LimitRange writer needs a Kubernetes client and a LimitRange name")

func (p *SandboxLimitRange) validate() error {
	if p.Kube == nil || p.Name == "" {
		return ErrLimitRangeConfig
	}
	return nil
}
