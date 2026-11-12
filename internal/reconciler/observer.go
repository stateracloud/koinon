// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/project"
)

// Observer error reasons of koinon_reconciler_sandbox_observer_errors_total: a fixed set (ADR 0023).
const (
	observerForbidden = "forbidden"
	observerNotFound  = "not_found"
	observerOther     = "other"
)

// ObserverLabel is the label of the RoleBindings the observer creates: kubectl delete rolebinding -A -l
// koinon.dev/sandbox-observer=true removes them.
var ObserverLabel = project.Domain + "/sandbox-observer"

var observerReasons = []string{observerForbidden, observerNotFound, observerOther}

// sandboxGVR is the Agent Sandbox object, the same kind the chart's policy fences (v1beta1 since
// Agent Sandbox v1.0).
var sandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// observerCallTimeout bounds each Kubernetes call, so a slow API server cannot hold a pass.
const observerCallTimeout = 5 * time.Second

// SandboxObserver explains why a sandbox never became ready (ADR 0078). OpenShell hides the pod's
// pull and crash errors behind ConfigurationPending, so the reconciler reads the pod, or the Agent
// Sandbox object when the pod is missing, with get only. Its access is one RoleBinding per workspace
// namespace to the chart's observer ClusterRole, which a native policy fences.
type SandboxObserver struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// GatewayID is OpenShell's gateway_id (openshell.gatewayId), which names the workspace namespaces.
	GatewayID string
	// ClusterRole is the chart's observer ClusterRole. The RoleBinding carries the same name.
	ClusterRole string
	// Subject is the reconciler's ServiceAccount: the RoleBinding's one subject.
	ServiceAccount, ServiceAccountNamespace string
	Log                                     *slog.Logger
	// Errors counts failures by reason; nil keeps them uncounted.
	Errors *prometheus.CounterVec

	// Now defaults to time.Now; tests move it.
	Now func() time.Time

	mu    sync.Mutex
	bound map[string]bool
	// denied is the negative cache: a namespace whose RoleBinding could not be created is not tried
	// again until its backoff has passed.
	denied map[string]*denial
}

// denial is one namespace's failed attempts.
type denial struct {
	failures int
	until    time.Time
	err      error
}

// Backoff after a failed RoleBinding: observerBackoffBase doubled for each failure in a row, capped at
// observerBackoffMax. A namespace OpenShell has not created yet is retried soon; a policy that refuses
// the binding (a wrong gateway ID) is not asked every 30 seconds for ever.
const (
	observerBackoffBase = 10 * time.Second
	observerBackoffMax  = 5 * time.Minute
)

func (o *SandboxObserver) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func observerBackoff(failures int) time.Duration {
	d := observerBackoffBase
	for i := 1; i < failures && d < observerBackoffMax; i++ {
		d *= 2
	}
	return min(d, observerBackoffMax)
}

func (o *SandboxObserver) namespace(workspace string) string {
	return execution.WorkspaceNamespace(o.GatewayID, workspace)
}

// EnsureAccess makes sure the observer ClusterRole is bound to the reconciler in the workspace's
// namespace. It creates the RoleBinding once per namespace and process; AlreadyExists counts as done.
// A namespace OpenShell has not created yet, or has not labelled yet, is retried by the next call.
func (o *SandboxObserver) EnsureAccess(ctx context.Context, workspace string) error {
	ns := o.namespace(workspace)
	o.mu.Lock()
	done := o.bound[ns]
	d := o.denied[ns]
	o.mu.Unlock()
	if done {
		return nil
	}
	if d != nil && o.now().Before(d.until) {
		return d.err
	}
	ctx, cancel := context.WithTimeout(ctx, observerCallTimeout)
	defer cancel()
	_, err := o.Kube.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: o.ClusterRole, Namespace: ns, Labels: map[string]string{
			"app.kubernetes.io/managed-by": ManagedByValue,
			// For the one-liner that removes them after the observer is turned off (docs).
			ObserverLabel: "true",
		}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: o.ClusterRole},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: o.ServiceAccount, Namespace: o.ServiceAccountNamespace}},
	}, metav1.CreateOptions{FieldManager: ManagedByValue})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		o.count(err)
		o.mu.Lock()
		if o.denied == nil {
			o.denied = map[string]*denial{}
		}
		failures := 1
		if d != nil {
			failures = d.failures + 1
		}
		o.denied[ns] = &denial{failures: failures, until: o.now().Add(observerBackoff(failures)), err: err}
		o.mu.Unlock()
		return err
	}
	o.mu.Lock()
	if o.bound == nil {
		o.bound = map[string]bool{}
	}
	o.bound[ns] = true
	delete(o.denied, ns)
	o.mu.Unlock()
	if err == nil {
		o.log().Info("bound the sandbox observer role", "namespace", ns)
	}
	return nil
}

// Explain reads the sandbox's workload pod and returns what explains the wait. When that pod says
// nothing it reads the supervisor pod, which OpenShell 0.1.5 creates next to it and names after the
// sandbox's ID (a supervisor image that cannot be pulled leaves the workload pod terminated with
// nothing waiting, and OpenShell reporting only ConfigurationPending). When the workload pod does
// not exist it reads the Agent Sandbox object. Any failure returns the zero detail: the observer only
// adds to what OpenShell reports, so the caller keeps OpenShell's reason. Reads are get by name.
func (o *SandboxObserver) Explain(ctx context.Context, workspace, sandbox, sandboxID string) execution.WaitDetail {
	if err := o.EnsureAccess(ctx, workspace); err != nil {
		return execution.WaitDetail{}
	}
	ns := o.namespace(workspace)
	cctx, cancel := context.WithTimeout(ctx, observerCallTimeout)
	defer cancel()
	// In managed mode the pod and the Sandbox object carry the OpenShell sandbox's name.
	pod, err := o.Kube.CoreV1().Pods(ns).Get(cctx, sandbox, metav1.GetOptions{})
	podMissing := apierrors.IsNotFound(err)
	switch {
	case err == nil:
		if d := execution.PodWaitDetail(pod); !d.IsZero() {
			return d
		}
	case !podMissing:
		o.failed(ns, err)
		return execution.WaitDetail{}
	}
	if sandboxID != "" {
		sup, err := o.Kube.CoreV1().Pods(ns).Get(cctx, execution.SupervisorPodName(sandboxID), metav1.GetOptions{})
		switch {
		case err == nil:
			if d := execution.PodWaitDetail(sup); !d.IsZero() {
				d.Message = truncateSupervisor(d.Message)
				return d
			}
		case !apierrors.IsNotFound(err):
			o.failed(ns, err)
			return execution.WaitDetail{}
		}
	}
	if !podMissing || o.Dynamic == nil {
		return execution.WaitDetail{}
	}
	cr, err := o.Dynamic.Resource(sandboxGVR).Namespace(ns).Get(cctx, sandbox, metav1.GetOptions{})
	switch {
	case err == nil:
		return execution.SandboxCRDetail(cr)
	case apierrors.IsNotFound(err):
		// Neither exists yet (just created) or any more: nothing to say, and nothing to count.
		return execution.WaitDetail{}
	}
	o.failed(ns, err)
	return execution.WaitDetail{}
}

// truncateSupervisor marks a message as the supervisor pod's, so that an operator does not look for the
// harness image.
func truncateSupervisor(msg string) string {
	return execution.TruncateWaitDetail("supervisor pod: " + msg)
}

// failed counts a failed read. A forbidden read means the RoleBinding is gone (deleted by hand):
// forget it, so that the next call creates it again.
func (o *SandboxObserver) failed(ns string, err error) {
	if apierrors.IsForbidden(err) {
		o.mu.Lock()
		delete(o.bound, ns)
		o.mu.Unlock()
	}
	o.count(err)
	o.log().Warn("sandbox observer could not read", "namespace", ns, "err", err)
}

func (o *SandboxObserver) count(err error) {
	if o.Errors == nil {
		return
	}
	reason := observerOther
	switch {
	case apierrors.IsForbidden(err):
		reason = observerForbidden
	case apierrors.IsNotFound(err):
		reason = observerNotFound
	}
	o.Errors.WithLabelValues(reason).Inc()
}

func (o *SandboxObserver) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// ErrObserverConfig is returned for an observer that lacks what it needs.
var ErrObserverConfig = errors.New("reconciler: the sandbox observer needs a Kubernetes client, a ClusterRole and a ServiceAccount")

func (o *SandboxObserver) validate() error {
	if o.Kube == nil || o.ClusterRole == "" || o.ServiceAccount == "" || o.ServiceAccountNamespace == "" {
		return ErrObserverConfig
	}
	return nil
}
