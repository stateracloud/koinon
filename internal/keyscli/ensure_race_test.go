// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Two koinon-api replicas that start together both find no KEK Secret and both decide to create it
// (issue #604, ADR 0056 amendment). Create is the compare-and-set: one wins, the other reads the
// winner's Secret, and no replica ends with a key the Secret does not hold.
var _ = Describe("EnsureWith on a first start", func() {
	It("lets one of two racing callers create the KEK Secret and both load that key", func(ctx SpecContext) {
		kube := fake.NewClientset()
		store := keyscli.KubeStore{Client: kube, Namespace: "koinon", Name: "koinon-kek"}

		// hasValues runs after the Get that found no Secret and before the Create. It holds each
		// caller there until both have arrived, so both are certain to try Create.
		var arrived sync.WaitGroup
		arrived.Add(2)
		barrier := func(context.Context) (bool, error) {
			arrived.Done()
			arrived.Wait()
			return false, nil
		}

		var created atomic.Int32
		var wg sync.WaitGroup
		errs := make([]error, 2)
		loaded := make([]keyscli.KeySet, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				did, err := store.EnsureWith(ctx, time.Now(), barrier, nil)
				errs[i] = err
				if did {
					created.Add(1)
				}
				// Each caller loads the key set right after its own EnsureWith: both must see the winner's.
				if err == nil {
					loaded[i], err = store.Load(ctx)
					errs[i] = err
				}
			}()
		}
		wg.Wait()

		Expect(errs).To(Equal([]error{nil, nil}))
		Expect(created.Load()).To(BeEquivalentTo(1), "exactly one caller creates the Secret")

		Expect(loaded[0].Keys).To(HaveLen(1))
		Expect(loaded[0].Active).NotTo(BeEmpty())
		Expect(loaded[1].Active).To(Equal(loaded[0].Active))
		Expect(loaded[1].Keys).To(Equal(loaded[0].Keys))
	})
})

var _ = Describe("DrawID", func() {
	It("redraws while the probe refuses an ID, then gives up", func(ctx SpecContext) {
		calls := 0
		id, err := keyscli.DrawID(ctx, time.Now(), nil, func(context.Context, string) (bool, error) {
			calls++
			if calls < 3 {
				return true, keyscli.ErrIDRetired
			}
			return false, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(Equal(3))
		Expect(secretstore.ValidKEKID(id)).To(BeTrue())

		_, err = keyscli.DrawID(ctx, time.Now(), nil, func(context.Context, string) (bool, error) { return true, nil })
		Expect(err).To(MatchError(ContainSubstring("could not draw an unused KEK ID")))
	})
})
