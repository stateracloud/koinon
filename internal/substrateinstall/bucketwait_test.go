// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

// On kind the bucket of the object store comes from a Job that ate-setup does not wait for (issue #1091): the
// gVisor staging retries while the bucket is missing and then goes on; a bucket that never comes fails it.

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/substrateinstall"
)

// lateBucketStore reports a missing bucket for the first missing calls of Digest.
type lateBucketStore struct {
	*fakeStore
	missing int
	calls   int
}

func (s *lateBucketStore) Digest(ctx context.Context, key string) (string, bool, error) {
	s.calls++
	if s.calls <= s.missing {
		return "", false, fmt.Errorf("the bucket %q does not exist: %w", "snapshots", substrateinstall.ErrNoBucket)
	}
	return s.fakeStore.Digest(ctx, key)
}

func TestGVisorStagingWaitsForTheBucketOfTheObjectStore(t *testing.T) {
	for name, tc := range map[string]struct {
		missing int
		ok      bool
	}{
		"the bucket comes after a few tries": {3, true},
		"the bucket never comes":             {1 << 20, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			base, fetch := stageEnv(t)
			store := &lateBucketStore{fakeStore: base, missing: tc.missing}
			i, _, _ := setup("37", true, nil)
			i.Profile.Platform = substrateinstall.PlatformKind
			i.PollInterval, i.WaitTimeout = time.Millisecond, 100*time.Millisecond
			i.Out = io.Discard
			i.GVisorFetcher = fetch
			i.GVisorStore = func(context.Context) (substrateinstall.AssetStore, func(), error) { return store, func() {}, nil }
			dyn, _ := sandboxConfigClient()
			i.Dynamic = dyn
			err := i.EnsureGVisorAssets(t.Context())
			if tc.ok {
				g.Expect(err).To(Succeed())
				return
			}
			g.Expect(err).To(MatchError(substrateinstall.ErrNoBucket))
		})
	}
}
