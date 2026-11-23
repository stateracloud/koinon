// SPDX-License-Identifier: Apache-2.0

package preflight

// The namespace of the Substrate probe pods (issue #1176). Substrate's admission policies let only workload
// controllers create pods in its namespaces: in the control-plane namespace a pod needs a controller owner, and in
// the worker namespace only the ReplicaSet controller creates pods. Koinon's probe pods are bare pods, so after an
// install they are refused there. They run in a namespace of their own, and the policies keep no exception for them.

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultSubstrateProbeNamespace is the namespace of the Substrate probe pods when none is given.
const DefaultSubstrateProbeNamespace = "koinon-substrate-probe"

// ProbeNamespaceComponent is the app.kubernetes.io/component label of a probe namespace that Koinon created.
const ProbeNamespaceComponent = "substrate-probe"

// CheckProbeNamespace refuses a probe namespace that is one of Substrate's namespaces: the admission
// policies there refuse a bare pod, and a policy exception for the probes would weaken them.
func (s SubstrateConfig) CheckProbeNamespace() error {
	s = s.WithDefaults()
	for _, ns := range []string{s.Namespace, s.WorkerNamespace} {
		if s.ProbeNamespace == ns {
			return fmt.Errorf("the probe namespace %s is a Substrate namespace: Substrate's admission policies refuse a bare probe pod there; use a namespace of its own (default %s)",
				ns, DefaultSubstrateProbeNamespace)
		}
	}
	return nil
}

// ProbeNamespace creates, waits for and removes the namespace of the Substrate probe pods.
type ProbeNamespace struct {
	Client kubernetes.Interface
	Name   string
	// PollInterval and Timeout bound the wait for the namespace's default ServiceAccount and CA (defaults 2s and 2m).
	PollInterval time.Duration
	Timeout      time.Duration
}

// Ensure creates the namespace when it is missing and reports whether this run owns it, so the caller removes it
// at the end. Pod Security is privileged (enforce, warn and audit): the device probe mounts hostPath devices. A
// namespace with Koinon's probe labels is a leftover of an earlier run that could not remove it: this run owns it.
// Another existing namespace is left as it is, so its owner keeps its labels. A namespace that is still terminating
// (the removal of an earlier run) is waited for, then created again: pods are refused in a terminating namespace.
func (n ProbeNamespace) Ensure(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, orDuration(n.Timeout, 2*time.Minute))
	defer cancel()
	for {
		ns, err := n.Client.CoreV1().Namespaces().Get(ctx, n.Name, metav1.GetOptions{})
		switch {
		case err == nil && ns.Status.Phase == corev1.NamespaceTerminating:
			select {
			case <-ctx.Done():
				return false, fmt.Errorf("namespace %s is still terminating: wait until it is gone, then try again", n.Name)
			case <-time.After(orDuration(n.PollInterval, 2*time.Second)):
			}
			continue
		case err == nil:
			return ownedProbeNamespace(ns), nil
		case !apierrors.IsNotFound(err):
			return false, fmt.Errorf("read namespace %s: %w", n.Name, err)
		}
		_, err = n.Client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   n.Name,
			Labels: probeNamespaceLabels(),
		}}, metav1.CreateOptions{})
		switch {
		case apierrors.IsAlreadyExists(err):
			continue // another run created it: read it again
		case err != nil:
			return false, fmt.Errorf("create namespace %s: %w", n.Name, err)
		}
		return true, nil
	}
}

func probeNamespaceLabels() map[string]string {
	return map[string]string{
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/warn":    "privileged",
		"pod-security.kubernetes.io/audit":   "privileged",
		"app.kubernetes.io/part-of":          "koinon",
		"app.kubernetes.io/component":        ProbeNamespaceComponent,
	}
}

// ownedProbeNamespace reports whether a namespace carries the labels Ensure gives the namespace it creates.
func ownedProbeNamespace(ns *corev1.Namespace) bool {
	l := ns.Labels
	return l["app.kubernetes.io/part-of"] == "koinon" && l["app.kubernetes.io/component"] == ProbeNamespaceComponent
}

// WaitReady waits until the controllers have filled the namespace: the probe pods use the default
// ServiceAccount, and the issuer probe mounts its token, whose ca.crt is the kube-root-ca.crt ConfigMap. Both
// appear some time after a namespace is created. Without the wait a probe pod on a fresh namespace is refused or
// stays in FailedMount.
func (n ProbeNamespace) WaitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, orDuration(n.Timeout, 2*time.Minute))
	defer cancel()
	for {
		var missing []string
		if _, err := n.Client.CoreV1().ServiceAccounts(n.Name).Get(ctx, "default", metav1.GetOptions{}); apierrors.IsNotFound(err) {
			missing = append(missing, "ServiceAccount default")
		} else if err != nil {
			return fmt.Errorf("read ServiceAccount %s/default: %w", n.Name, err)
		}
		if _, err := n.Client.CoreV1().ConfigMaps(n.Name).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{}); apierrors.IsNotFound(err) {
			missing = append(missing, "ConfigMap kube-root-ca.crt")
		} else if err != nil {
			return fmt.Errorf("read ConfigMap %s/kube-root-ca.crt: %w", n.Name, err)
		}
		if len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %s not ready in time, missing: %s", n.Name, strings.Join(missing, ", "))
		case <-time.After(orDuration(n.PollInterval, 2*time.Second)):
		}
	}
}

// Remove deletes the namespace. Call it only for a namespace that Ensure reported as owned. It does not wait for the
// deletion to finish.
func (n ProbeNamespace) Remove(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := n.Client.CoreV1().Namespaces().Delete(dctx, n.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the probe namespace %s: %w", n.Name, err)
	}
	return nil
}
