// SPDX-License-Identifier: Apache-2.0

package recovery

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/project"
)

// The PlatformAdminRecovery custom resource (ADR 0110 decision 6).
const (
	Group    = "recovery." + project.Domain
	Version  = "v1alpha1"
	Kind     = "PlatformAdminRecovery"
	Resource = "platformadminrecoveries"
)

// GVR is the resource the controller watches.
var GVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}

// Status phases written to the object.
const (
	PhaseDone    = "Done"
	PhaseRefused = "Refused"
)

// Controller turns PlatformAdminRecovery objects in the release namespace into actions. It polls: the
// object count is tiny and a poll needs no informer, no leader election and no watch permission, and it
// works under the restore hold (the reconciler serves this handler even when held, ADR 0110 decision 6).
// Several replicas may poll; the nonce row makes the action happen once and the status write loses a
// race harmlessly.
type Controller struct {
	Dynamic   dynamic.Interface
	Kube      kubernetes.Interface
	Namespace string
	// Policy and Binding name this release's ValidatingAdmissionPolicy and its binding.
	Policy, Binding string
	Handler         *Handler
	Interval        time.Duration
	Log             *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Run polls until ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.Pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Pass handles every object once.
func (c *Controller) Pass(ctx context.Context) {
	list, err := c.Dynamic.Resource(GVR).Namespace(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if ctx.Err() == nil && !apierrors.IsNotFound(err) {
			c.Log.Warn("recovery requests: list failed", "err", err)
		}
		return
	}
	now := c.now()
	for i := range list.Items {
		obj := &list.Items[i]
		req, perr := ParseRequest(obj)
		// A handled object past its TTL is deleted: the request is one-shot. An unhandled one gets its
		// refusal first ("expired") and is deleted on the next pass.
		if perr == nil && req.Expired(now) && phaseOf(obj) != "" {
			c.remove(ctx, obj)
			continue
		}
		if phaseOf(obj) != "" {
			continue
		}
		if perr != nil {
			c.finish(ctx, obj, Outcome{Reason: perr.Error()})
			continue
		}
		c.finish(ctx, obj, c.process(ctx, req))
	}
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) process(ctx context.Context, req Request) Outcome {
	if err := c.CheckPolicy(ctx, req.Created); err != nil {
		return Outcome{Reason: "admission policy is not in force: " + err.Error()}
	}
	o, err := c.Handler.Handle(ctx, req)
	if err != nil {
		// Transient (the database): leave the status empty and try again on the next pass.
		c.Log.Warn("recovery request failed, will retry", "name", req.Name, "err", err)
		return Outcome{Reason: errRetry}
	}
	return o
}

// errRetry marks an outcome that is not written.
const errRetry = "\x00retry"

func (c *Controller) finish(ctx context.Context, obj *unstructured.Unstructured, o Outcome) {
	if o.Reason == errRetry {
		return
	}
	phase := PhaseRefused
	st := map[string]any{"completedAt": c.now().UTC().Format(time.RFC3339), "observedUID": string(obj.GetUID())}
	if o.Done {
		phase = PhaseDone
		st["sealedResult"] = o.Sealed
	} else {
		st["reason"] = o.Reason
	}
	st["phase"] = phase
	if err := unstructured.SetNestedMap(obj.Object, st, "status"); err != nil {
		c.Log.Warn("recovery request: status not built", "name", obj.GetName(), "err", err)
		return
	}
	// resourceVersion makes the write lose to a replica that finished first.
	if _, err := c.Dynamic.Resource(GVR).Namespace(c.Namespace).UpdateStatus(ctx, obj, metav1.UpdateOptions{}); err != nil {
		if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			c.Log.Warn("recovery request: status write failed, will retry", "name", obj.GetName(), "err", err)
		}
		return
	}
	c.Log.Info("recovery request handled", "name", obj.GetName(), "phase", phase, "reason", o.Reason)
}

func (c *Controller) remove(ctx context.Context, obj *unstructured.Unstructured) {
	uid := obj.GetUID()
	err := c.Dynamic.Resource(GVR).Namespace(c.Namespace).Delete(ctx, obj.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !apierrors.IsNotFound(err) {
		c.Log.Warn("recovery request: delete failed", "name", obj.GetName(), "err", err)
	}
}

func phaseOf(obj *unstructured.Unstructured) string {
	p, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return p
}

// CheckPolicy fails unless this release's admission policy is in force. A ValidatingAdmissionPolicy
// fails open when its binding is missing or changed, and it does not judge objects made before the
// binding, so the identity in spec.requester is trustworthy only while every check holds (ADR 0110
// decision 6). The returned error names the check that failed.
func (c *Controller) CheckPolicy(ctx context.Context, created time.Time) error {
	api := c.Kube.AdmissionregistrationV1()
	pol, err := api.ValidatingAdmissionPolicies().Get(ctx, c.Policy, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("policy %s: %w", c.Policy, err)
	}
	bind, err := api.ValidatingAdmissionPolicyBindings().Get(ctx, c.Binding, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("binding %s: %w", c.Binding, err)
	}
	return checkPolicy(pol, bind, c.Policy, c.Namespace, created)
}

func checkPolicy(pol *admissionv1.ValidatingAdmissionPolicy, bind *admissionv1.ValidatingAdmissionPolicyBinding, policyName, namespace string, created time.Time) error {
	if pol.Spec.FailurePolicy == nil || *pol.Spec.FailurePolicy != admissionv1.Fail {
		return fmt.Errorf("policy failurePolicy is not Fail")
	}
	if bind.Spec.PolicyName != policyName {
		return fmt.Errorf("binding names policy %q, not %q", bind.Spec.PolicyName, policyName)
	}
	if !slices.Equal(bind.Spec.ValidationActions, []admissionv1.ValidationAction{admissionv1.Deny}) {
		return fmt.Errorf("binding validationActions is not exactly [Deny]")
	}
	mr := bind.Spec.MatchResources
	if mr == nil || mr.NamespaceSelector == nil || len(mr.NamespaceSelector.MatchExpressions) != 0 ||
		len(mr.NamespaceSelector.MatchLabels) != 1 || mr.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != namespace {
		return fmt.Errorf("binding does not select exactly the release namespace")
	}
	if mr.ObjectSelector != nil && (len(mr.ObjectSelector.MatchLabels) != 0 || len(mr.ObjectSelector.MatchExpressions) != 0) {
		return fmt.Errorf("binding has an objectSelector")
	}
	if len(mr.ExcludeResourceRules) != 0 {
		return fmt.Errorf("binding has excludeResourceRules")
	}
	if len(mr.ResourceRules) != 0 {
		return fmt.Errorf("binding narrows resourceRules")
	}
	if created.Before(bind.CreationTimestamp.Time) {
		return fmt.Errorf("the request is older than the binding")
	}
	return nil
}

// ParseRequest reads a PlatformAdminRecovery object.
func ParseRequest(obj *unstructured.Unstructured) (Request, error) {
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(obj.Object, append([]string{"spec"}, path...)...)
		return s
	}
	dur := func(field string) (time.Duration, error) {
		s := str(field)
		if s == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("spec.%s %q is not a positive duration", field, s)
		}
		return d, nil
	}
	req := Request{
		Name: obj.GetName(), UID: string(obj.GetUID()), Created: obj.GetCreationTimestamp().Time,
		Action: str("action"), User: str("user"), Org: str("org"), Requester: str("requester"),
		Nonce: str("nonce"), RecipientPublicKey: str("recipientPublicKey"),
	}
	req.Groups, _, _ = unstructured.NestedStringSlice(obj.Object, "spec", "requesterGroups")
	var err error
	if req.TTL, err = dur("ttl"); err != nil {
		return req, err
	}
	if req.TokenTTL, err = dur("tokenTTL"); err != nil {
		return req, err
	}
	return req, nil
}
