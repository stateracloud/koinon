// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"testing"

	"github.com/stateracloud/koinon/internal/agentd/turnpolicy"
	"github.com/stateracloud/koinon/internal/db/store"
)

// turnpolicy names the failure classes it retries by their stored values. A rename of either side
// must not silently stop the retries.
func TestTurnPolicyClassesAreTheStoredClasses(t *testing.T) {
	if string(store.RunFailureClassTransient) != string(turnpolicy.ClassTransient) {
		t.Errorf("transient: store %q, turnpolicy %q", store.RunFailureClassTransient, turnpolicy.ClassTransient)
	}
	if string(store.RunFailureClassThrottled) != string(turnpolicy.ClassThrottled) {
		t.Errorf("throttled: store %q, turnpolicy %q", store.RunFailureClassThrottled, turnpolicy.ClassThrottled)
	}
	for _, c := range []store.RunFailureClass{classContextOverflow, classBudget, classPolicy, classFatal, classSandboxUnavailable, ""} {
		if turnpolicy.Class(c).Retryable() {
			t.Errorf("class %q must not be retried", c)
		}
	}
}
