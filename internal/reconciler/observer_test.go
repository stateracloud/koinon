// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/reconciler"
)

var _ = Describe("SandboxObserver.EnsureAccess backoff", func() {
	var (
		creates atomic.Int32
		refuse  atomic.Bool
		now     time.Time
		o       *reconciler.SandboxObserver
	)
	BeforeEach(func() {
		creates.Store(0)
		refuse.Store(true)
		now = time.Unix(1_000_000, 0)
		kube := fake.NewClientset()
		kube.PrependReactor("create", "rolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
			creates.Add(1)
			if refuse.Load() {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "rolebindings"}, "b", errors.New("policy"))
			}
			return false, nil, nil
		})
		o = &reconciler.SandboxObserver{
			Kube: kube, GatewayID: "openshell", ClusterRole: "r", ServiceAccount: "sa", ServiceAccountNamespace: "koinon",
			Now: func() time.Time { return now },
		}
	})

	It("does not ask the API server again until the backoff has passed, and doubles it up to a cap", func() {
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(BeEquivalentTo(1))

		By("within the first backoff (10s) the cached refusal is returned")
		for range 5 {
			now = now.Add(time.Second)
			err := o.EnsureAccess(context.Background(), "kw-1")
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
		}
		Expect(creates.Load()).To(BeEquivalentTo(1))

		By("after it, one more attempt, then twice as long")
		now = now.Add(5 * time.Second)
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(BeEquivalentTo(2))
		now = now.Add(19 * time.Second)
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(BeEquivalentTo(2), "the second backoff is 20s")
		now = now.Add(2 * time.Second)
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(BeEquivalentTo(3))

		By("capped at five minutes")
		for range 10 {
			now = now.Add(time.Hour)
			Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		}
		before := creates.Load()
		now = now.Add(4*time.Minute + 59*time.Second)
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(Equal(before), "still inside the cap")
		now = now.Add(2 * time.Second)
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		Expect(creates.Load()).To(Equal(before + 1))
	})

	It("keeps a separate backoff per namespace, and forgets it once the binding is made", func() {
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(HaveOccurred())
		refuse.Store(false)
		Expect(o.EnsureAccess(context.Background(), "kw-2")).To(Succeed(), "another namespace is not held back")
		now = now.Add(11 * time.Second)
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(Succeed())
		creates.Store(0)
		Expect(o.EnsureAccess(context.Background(), "kw-1")).To(Succeed())
		Expect(creates.Load()).To(BeZero(), "bound namespaces are not asked again")
	})
})
