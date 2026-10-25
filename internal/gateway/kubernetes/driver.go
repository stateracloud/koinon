// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

// NackReason is the reason of the Warning Event agentgateway's controller records on the Gateway
// when the proxy refuses a resource it was sent.
const NackReason = "AgentGatewayNackError"

// Driver is the Kubernetes-mode ModelGateway: Render, Apply, Prune, Wait for status, confirm on
// the data plane.
type Driver struct {
	Options Options
	Store   Store
	// Events reads the controller's NACK events. Optional.
	Events kubernetes.Interface
	// Waiter confirms, with the revision's probe key against the running proxy, that the proxy
	// serves a revision. Required (use standalone.ProbeWaiter).
	Waiter standalone.Waiter
	// StatusTimeout bounds the wait for the controller's status of the platform objects (Gateway,
	// policies, MCP backend and route). Default 60s.
	StatusTimeout time.Duration
	// OrgGrace is how long an org's object may stay without a status before it is reported as
	// rejected instead of holding the Apply back. Default 10s.
	OrgGrace time.Duration
	// StatusInterval is the polling interval. Default 250ms.
	StatusInterval time.Duration
	Log            *slog.Logger
}

var _ gateway.ModelGateway = (*Driver)(nil)

// IssueKey implements gateway.ModelGateway; the controller reads key hashes, so Koinon generates keys.
func (d *Driver) IssueKey(_ context.Context, agent gateway.AgentRef) (gateway.IssuedKey, error) {
	return gateway.NewKey(agent)
}

// LedgerEnforcesBudgets implements gateway.LedgerBudgets: only a limit of zero binds here.
func (d *Driver) LedgerEnforcesBudgets() bool { return true }

// Usage implements gateway.ModelGateway. Usage arrives through the access log, exported over OTLP
// to the spend ledger.
func (d *Driver) Usage() gateway.UsageReader { return gateway.NoUsage{} }

// Apply implements gateway.ModelGateway.
//
// It applies every rendered object (server-side apply), deletes objects of this Gateway that are no
// longer rendered, waits for the controller to report on each object's current generation, reads
// the controller's NACK events, and finally confirms with the revision's probe key that the proxy
// serves the change. An object the controller or the proxy refuses is reported in a
// *gateway.RejectedError together with the revision, and does not hold back the other orgs.
func (d *Driver) Apply(ctx context.Context, s gateway.State) (gateway.Revision, error) {
	o := d.Options.withDefaults()
	r, err := Render(s, o)
	if err != nil {
		return "", err
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	timeout := d.StatusTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	started := time.Now()
	var tApplied, tStatus time.Time

	var (
		rejected  []gateway.Rejection
		applyErrs []error
	)
	for _, obj := range r.Objects {
		if _, err := d.Store.Apply(ctx, obj); err != nil {
			// The API server refuses one org's object (a base URL the CRD's rules reject): report it
			// on the org and go on, so it does not hold back the other orgs. Its previous version,
			// if any, stays. A key ConfigMap is the exception: a refused update would leave the old
			// keys live, so it is an error. A platform object that is refused is an error too.
			if apierrors.IsInvalid(err) && obj.Org != "" && obj.U.GetKind() != KindConfigMap {
				rejected = append(rejected, gateway.Rejection{Org: obj.Org, Kind: obj.U.GetKind(), Name: obj.U.GetName(), Reason: err.Error()})
				continue
			}
			// Go on with the other objects and prune: a revoked key must not wait for an unrelated
			// failure, and the error is returned at the end.
			applyErrs = append(applyErrs, err)
		}
	}
	if err := d.prune(ctx, o, r); err != nil {
		applyErrs = append(applyErrs, err)
	}
	if len(applyErrs) > 0 {
		return "", errors.Join(applyErrs...)
	}
	tApplied = time.Now()
	if len(r.Revoked) > 0 {
		log.InfoContext(ctx, "keys out of budget are revoked from the gateway", "keys", r.Revoked)
	}

	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	refused, err := d.awaitStatus(sctx, o, r)
	if err != nil {
		return "", err
	}
	rejected = append(rejected, refused...)
	tStatus = time.Now()
	rejected = append(rejected, d.nacks(ctx, o, r, started)...)
	if d.Waiter != nil {
		wctx, wcancel := context.WithTimeout(ctx, max(timeout, 30*time.Second))
		defer wcancel()
		if err := d.Waiter.Wait(wctx, r.Revision); err != nil {
			if len(rejected) > 0 {
				err = fmt.Errorf("%w (%w)", err, &gateway.RejectedError{Rejected: rejected})
			}
			return "", fmt.Errorf("kubernetes: revision %s not served: %w", r.Revision, err)
		}
	}
	log.DebugContext(ctx, "kubernetes gateway applied", "revision", string(r.Revision), "objects", len(r.Objects),
		"apply", tApplied.Sub(started).String(), "status", tStatus.Sub(tApplied).String(), "probe", time.Since(tStatus).String())
	if len(rejected) > 0 {
		return r.Revision, &gateway.RejectedError{Rejected: dedupe(rejected)}
	}
	return r.Revision, nil
}

func (d *Driver) selector(o Options) string {
	return "app.kubernetes.io/managed-by=" + o.ManagedBy + "," + LabelGateway + "=" + o.GatewayName
}

// prune deletes this Gateway's objects that Render no longer produces: a revoked key's org after its
// last key, a deleted model, a removed MCP server.
func (d *Driver) prune(ctx context.Context, o Options, r Rendered) error {
	want := map[string]bool{}
	for _, obj := range r.Objects {
		want[obj.Ref()] = true
	}
	for _, kind := range slices.Backward(applyOrder) {
		live, err := d.Store.List(ctx, kind, o.Namespace, d.selector(o))
		if err != nil {
			return fmt.Errorf("kubernetes: list %s: %w", kind, err)
		}
		for _, u := range live {
			if !want[kind+"/"+u.GetName()] {
				if err := d.Store.Delete(ctx, kind, o.Namespace, u.GetName()); err != nil {
					return fmt.Errorf("kubernetes: delete %s/%s: %w", kind, u.GetName(), err)
				}
			}
		}
	}
	return nil
}

// awaitStatus polls the objects' status until none is pending. It returns the refused ones.
func (d *Driver) awaitStatus(ctx context.Context, o Options, r Rendered) ([]gateway.Rejection, error) {
	interval := d.StatusInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	owner := map[string]Object{}
	for _, obj := range r.Objects {
		owner[obj.Ref()] = obj
	}
	grace := d.OrgGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	began := time.Now()
	for {
		var (
			rejected []gateway.Rejection
			pending  []string
		)
		for _, kind := range applyOrder {
			if kind == KindConfigMap {
				continue
			}
			live, err := d.Store.List(ctx, kind, o.Namespace, d.selector(o))
			if err != nil {
				return nil, fmt.Errorf("kubernetes: list %s: %w", kind, err)
			}
			for _, u := range live {
				obj, ok := owner[kind+"/"+u.GetName()]
				if !ok {
					continue
				}
				switch st := Evaluate(u); st.Verdict {
				case Pending:
					if obj.Org != "" && time.Since(began) > grace {
						// No status in time: an org's object the controller does not pick up (a
						// parent or reference that does not resolve) is that org's problem.
						rejected = append(rejected, gateway.Rejection{Org: obj.Org, Kind: kind, Name: u.GetName(), Reason: "the controller did not report on it"})
						continue
					}
					pending = append(pending, obj.Ref())
				case Refused:
					rejected = append(rejected, attribute(obj, st.Reason, r)...)
				}
			}
		}
		if len(pending) == 0 {
			return rejected, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("kubernetes: the controller did not report on %s within the status timeout", strings.Join(pending, ", "))
		case <-time.After(interval):
		}
	}
}

// attribute turns a refused object into rejections. A policy that refuses key entries names the
// ConfigMap in its message, and the ConfigMap belongs to one org.
func attribute(obj Object, reason string, r Rendered) []gateway.Rejection {
	if obj.U.GetKind() == KindPolicy {
		var out []gateway.Rejection
		for _, other := range r.Objects {
			if other.U.GetKind() == KindConfigMap && other.Org != "" && strings.Contains(reason, "configMap "+other.U.GetName()+" ") {
				out = append(out, gateway.Rejection{Org: other.Org, Kind: KindConfigMap, Name: other.U.GetName(), Reason: reason})
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []gateway.Rejection{{Org: obj.Org, Kind: obj.U.GetKind(), Name: obj.U.GetName(), Reason: reason}}
}

// nacks reads the controller's NACK events newer than since. A message that names one of the
// rendered objects is attributed to it (and so to its org); any other becomes a platform rejection of
// the Gateway.
func (d *Driver) nacks(ctx context.Context, o Options, r Rendered, since time.Time) []gateway.Rejection {
	if d.Events == nil {
		return nil
	}
	list, err := d.Events.CoreV1().Events(o.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=" + NackReason})
	if err != nil {
		return nil
	}
	var out []gateway.Rejection
	for _, ev := range list.Items {
		at := ev.LastTimestamp.Time
		if at.IsZero() {
			at = ev.EventTime.Time
		}
		if at.IsZero() {
			at = ev.CreationTimestamp.Time
		}
		if at.Before(since.Add(-2 * time.Second)) {
			continue
		}
		matched := false
		if obj, ok := nackTarget(ev.Message, r.Objects); ok {
			out = append(out, gateway.Rejection{Org: obj.Org, Kind: obj.U.GetKind(), Name: obj.U.GetName(), Reason: "proxy NACK: " + ev.Message})
			matched = true
		}
		if !matched {
			out = append(out, gateway.Rejection{Kind: KindGateway, Name: o.GatewayName, Reason: "proxy NACK: " + ev.Message})
		}
	}
	return out
}

func dedupe(in []gateway.Rejection) []gateway.Rejection {
	var out []gateway.Rejection
	for _, r := range in {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// nackTarget finds the rendered object a NACK message names: the object whose name occurs in the
// message as a whole name (not as part of a longer one), the longest such name winning, because
// koinon-gateway is a prefix of the name of every object. ConfigMaps count.
func nackTarget(msg string, objs []Object) (Object, bool) {
	var best Object
	found := false
	for _, o := range objs {
		name := o.U.GetName()
		if !hasName(msg, name) {
			continue
		}
		// The Gateway and its Parameters share a name; the NACK events are about the Gateway.
		if !found || len(name) > len(best.U.GetName()) || (len(name) == len(best.U.GetName()) && o.U.GetKind() == KindGateway) {
			best, found = o, true
		}
	}
	return best, found
}

// hasName reports whether name occurs in msg delimited by characters a Kubernetes name cannot hold.
func hasName(msg, name string) bool {
	for i := 0; ; {
		j := strings.Index(msg[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		before := start == 0 || !isNameChar(msg[start-1])
		after := end == len(msg) || !isNameChar(msg[end])
		if before && after {
			return true
		}
		i = start + 1
	}
}

func isNameChar(c byte) bool {
	return c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
