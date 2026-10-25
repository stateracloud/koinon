// SPDX-License-Identifier: Apache-2.0

package kubernetes_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/gateway"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
)

// memStore is an in-memory Store whose "controller" answers every object with a status.
type memStore struct {
	mu      sync.Mutex
	objs    map[string]*unstructured.Unstructured
	status  func(*unstructured.Unstructured) map[string]any
	applyFn func(gwk8s.Object) error
}

func newMem() *memStore { return &memStore{objs: map[string]*unstructured.Unstructured{}} }

func (m *memStore) Apply(_ context.Context, o gwk8s.Object) (*unstructured.Unstructured, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.applyFn != nil {
		if err := m.applyFn(o); err != nil {
			return nil, err
		}
	}
	u := o.U.DeepCopy()
	u.SetGeneration(1)
	if m.status != nil {
		if st := m.status(u); st != nil {
			u.Object["status"] = st
		}
	}
	m.objs[o.Ref()] = u
	return u, nil
}

func (m *memStore) Get(_ context.Context, kind, _, name string) (*unstructured.Unstructured, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.objs[kind+"/"+name]; ok {
		return u.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: kind}, name)
}

func (m *memStore) List(_ context.Context, kind, _, _ string) ([]*unstructured.Unstructured, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*unstructured.Unstructured
	for k, u := range m.objs {
		if u.GetKind() == kind {
			_ = k
			out = append(out, u.DeepCopy())
		}
	}
	return out, nil
}

func (m *memStore) Delete(_ context.Context, kind, _, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, kind+"/"+name)
	return nil
}

func (m *memStore) names(kind string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, u := range m.objs {
		if u.GetKind() == kind {
			out = append(out, u.GetName())
		}
	}
	sort.Strings(out)
	return out
}

func cond(typ, status, reason, msg string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": msg, "observedGeneration": int64(1)}
}

// acceptAll answers like a healthy controller.
func acceptAll(u *unstructured.Unstructured) map[string]any {
	ok := func(types ...string) []any {
		var out []any
		for _, t := range types {
			out = append(out, cond(t, "True", "Valid", ""))
		}
		return out
	}
	switch u.GetKind() {
	case gwk8s.KindGateway:
		return map[string]any{"conditions": ok("Accepted", "Programmed")}
	case gwk8s.KindBackend:
		return map[string]any{"conditions": ok("Accepted")}
	case gwk8s.KindPolicy:
		return map[string]any{"ancestors": []any{map[string]any{"controllerName": gwk8s.ControllerName, "conditions": ok("Accepted", "Attached")}}}
	case gwk8s.KindModel, gwk8s.KindHTTPRoute:
		return map[string]any{"parents": []any{map[string]any{"controllerName": gwk8s.ControllerName, "conditions": ok("Accepted", "ResolvedRefs")}}}
	}
	return nil
}

type instantWaiter struct{ revs []gateway.Revision }

func (w *instantWaiter) Wait(_ context.Context, rev gateway.Revision) error {
	w.revs = append(w.revs, rev)
	return nil
}

var _ = Describe("Driver without a cluster", func() {
	var (
		mem    *memStore
		waiter *instantWaiter
		d      *gwk8s.Driver
	)
	BeforeEach(func() {
		mem = newMem()
		mem.status = acceptAll
		waiter = &instantWaiter{}
		d = &gwk8s.Driver{Options: opts(), Store: mem, Waiter: waiter, StatusInterval: time.Millisecond, StatusTimeout: 2 * time.Second, OrgGrace: 30 * time.Millisecond}
	})

	It("applies, waits for the proxy with the revision's probe key, and prunes what is no longer rendered", func(ctx SpecContext) {
		rev, err := d.Apply(ctx, fullState())
		Expect(err).NotTo(HaveOccurred())
		Expect(waiter.revs).To(Equal([]gateway.Revision{rev}))
		Expect(mem.names("ConfigMap")).To(ContainElements("koinon-gateway-keys-acme", "koinon-gateway-keys-globex", "koinon-gateway-keys-probe", "koinon-gateway-catalog"))
		Expect(mem.names("AgentgatewayModel")).To(HaveLen(2))

		// globex's last key goes (and its model with it): its ConfigMap and model are pruned.
		s := fullState()
		s.RevokeKey("k-gggggggggggg")
		s.Providers = s.Providers[:1]
		_, err = d.Apply(ctx, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(mem.names("ConfigMap")).NotTo(ContainElement("koinon-gateway-keys-globex"))
		Expect(mem.names("AgentgatewayModel")).To(HaveLen(1))
		Expect(mem.names("Gateway")).To(Equal([]string{"koinon-gateway"}))
	})

	It("returns the revision with a RejectedError for the refused objects only", func(ctx SpecContext) {
		mem.status = func(u *unstructured.Unstructured) map[string]any {
			if u.GetKind() == gwk8s.KindModel && u.GetLabels()[gwk8s.LabelOrg] == "acme" {
				return map[string]any{"parents": []any{map[string]any{"controllerName": gwk8s.ControllerName, "conditions": []any{
					cond("Accepted", "True", "Accepted", ""), cond("ResolvedRefs", "False", "Invalid", "authorization matchExpression is not a valid CEL expression"),
				}}}}
			}
			return acceptAll(u)
		}
		rev, err := d.Apply(ctx, fullState())
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue())
		Expect(rev).NotTo(BeEmpty())
		Expect(rej.Orgs()).To(Equal([]string{"acme"}))
		Expect(rej.Rejected[0].Kind).To(Equal("AgentgatewayModel"))
		Expect(rej.Error()).To(ContainSubstring("not a valid CEL expression"))
		Expect(waiter.revs).To(HaveLen(1), "the proxy was still probed: the other orgs are live")
	})

	It("attributes a policy's PartiallyValid key entries to the org whose ConfigMap it names", func(ctx SpecContext) {
		mem.status = func(u *unstructured.Unstructured) map[string]any {
			if u.GetName() == "koinon-gateway-edge" {
				return map[string]any{"ancestors": []any{map[string]any{"controllerName": gwk8s.ControllerName, "conditions": []any{
					cond("Accepted", "True", "PartiallyValid", "configMap koinon-gateway-keys-globex contains invalid key k-gggggggggggg: keyHash must contain a valid sha256 hex digest"),
					cond("Attached", "True", "Attached", ""),
				}}}}
			}
			return acceptAll(u)
		}
		_, err := d.Apply(ctx, fullState())
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue())
		Expect(rej.Rejected).To(ConsistOf(HaveField("Org", "globex")))
		Expect(rej.Rejected[0].Name).To(Equal("koinon-gateway-keys-globex"))
	})

	It("reports an org's object that gets no status after the grace period, and fails on a platform object that does not", func(ctx SpecContext) {
		mem.status = func(u *unstructured.Unstructured) map[string]any {
			if u.GetKind() == gwk8s.KindModel {
				return nil
			}
			return acceptAll(u)
		}
		_, err := d.Apply(ctx, fullState())
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue())
		Expect(rej.Orgs()).To(Equal([]string{"acme", "globex"}))

		mem.status = func(u *unstructured.Unstructured) map[string]any {
			if u.GetKind() == gwk8s.KindGateway {
				return nil
			}
			return acceptAll(u)
		}
		d.StatusTimeout = 100 * time.Millisecond
		_, err = d.Apply(ctx, fullState())
		Expect(err).To(MatchError(ContainSubstring("did not report on Gateway/koinon-gateway")))
		Expect(errors.As(err, &rej)).To(BeFalse())
	})

	It("reports an object the API server finds invalid on its org, and fails on an invalid platform object", func(ctx SpecContext) {
		invalid := func(kind string) func(gwk8s.Object) error {
			return func(o gwk8s.Object) error {
				if o.U.GetKind() == kind {
					return apierrors.NewInvalid(schema.GroupKind{Group: "agentgateway.dev", Kind: kind}, o.U.GetName(), nil)
				}
				return nil
			}
		}
		mem.applyFn = invalid(gwk8s.KindModel)
		_, err := d.Apply(ctx, fullState())
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue())

		mem.applyFn = invalid(gwk8s.KindGateway)
		_, err = d.Apply(ctx, fullState())
		Expect(err).To(HaveOccurred())
		Expect(errors.As(err, &rej)).To(BeFalse())
	})

	It("does not wait for a proxy that never serves the revision", func(ctx SpecContext) {
		d.Waiter = failingWaiter{}
		_, err := d.Apply(ctx, fullState())
		Expect(err).To(MatchError(ContainSubstring("not served")))
	})

	It("maps the controller's NACK events to the objects they name", func(ctx SpecContext) {
		model := ""
		r, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		for _, o := range r.Objects {
			if o.U.GetKind() == gwk8s.KindModel && o.Org == "globex" {
				model = o.U.GetName()
			}
		}
		d.Events = fake.NewSimpleClientset(
			&corev1.Event{
				ObjectMeta: metav1.ObjectMeta{Name: "nack-1", Namespace: "koinon"}, Reason: gwk8s.NackReason, Type: "Warning",
				Message: "rejected resource backend/koinon/" + model + ": bad", LastTimestamp: metav1.NewTime(time.Now().Add(time.Second)),
			},
			&corev1.Event{
				ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "koinon"}, Reason: gwk8s.NackReason, Type: "Warning",
				Message: "rejected resource from last week", LastTimestamp: metav1.NewTime(time.Now().Add(-7 * 24 * time.Hour)),
			},
		)
		_, err = d.Apply(ctx, fullState())
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue(), "got %v", err)
		Expect(rej.Rejected).To(ConsistOf(SatisfyAll(HaveField("Org", "globex"), HaveField("Name", model))))
	})
})

type failingWaiter struct{}

func (failingWaiter) Wait(context.Context, gateway.Revision) error { return errors.New("401") }

var _ = Describe("Evaluate", func() {
	obj := func(kind string, gen int64, status map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"kind": kind, "metadata": map[string]any{"name": "x", "generation": gen}}}
		if status != nil {
			u.Object["status"] = status
		}
		return u
	}
	It("waits for a status of the current generation", func() {
		Expect(gwk8s.Evaluate(obj(gwk8s.KindModel, 2, nil)).Verdict).To(Equal(gwk8s.Pending))
		stale := map[string]any{"parents": []any{map[string]any{"controllerName": gwk8s.ControllerName, "conditions": []any{
			cond("Accepted", "True", "Accepted", ""), cond("ResolvedRefs", "True", "ResolvedRefs", ""),
		}}}}
		Expect(gwk8s.Evaluate(obj(gwk8s.KindModel, 2, stale)).Verdict).To(Equal(gwk8s.Pending), "generation 1 is not generation 2")
		Expect(gwk8s.Evaluate(obj(gwk8s.KindModel, 1, stale)).Verdict).To(Equal(gwk8s.Accepted))
	})
	It("ignores other controllers' entries", func() {
		other := map[string]any{"parents": []any{map[string]any{"controllerName": "example.com/other", "conditions": []any{cond("Accepted", "False", "No", "other")}}}}
		Expect(gwk8s.Evaluate(obj(gwk8s.KindModel, 1, other)).Verdict).To(Equal(gwk8s.Pending))
	})
	It("refuses a False condition with the controller's message", func() {
		st := acceptAll(&unstructured.Unstructured{Object: map[string]any{"kind": gwk8s.KindGateway}})
		st["conditions"] = []any{cond("Accepted", "True", "Accepted", ""), cond("Programmed", "False", "Invalid", "no addresses")}
		r := gwk8s.Evaluate(obj(gwk8s.KindGateway, 1, st))
		Expect(r.Verdict).To(Equal(gwk8s.Refused))
		Expect(r.Reason).To(ContainSubstring("no addresses"))
	})
	It("takes a ConfigMap as accepted: its policy reports it", func() {
		Expect(gwk8s.Evaluate(obj(gwk8s.KindConfigMap, 1, nil)).Verdict).To(Equal(gwk8s.Accepted))
	})
})
