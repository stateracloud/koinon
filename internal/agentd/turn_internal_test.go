// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/agentd/loopguard"
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

// A loop stop says whether the agent was asked to change its approach: when the harness ignored the
// cancel, the nudge never went out and the reason must not claim it did.
func TestLoopStopReasonSaysWhetherTheNudgeWentOut(t *testing.T) {
	for _, undelivered := range []bool{false, true} {
		r, class, ok := (&turnResult{out: turnpolicy.Outcome{
			Kind: turnpolicy.OutLoop, Pattern: string(loopguard.SameAction), NudgeUndelivered: undelivered,
		}}).fails()
		if !ok || class != classLoop {
			t.Fatalf("undelivered=%v: fails() = %q, %q, %v", undelivered, r, class, ok)
		}
		asked := strings.Contains(r, "after it was asked")
		notAsked := strings.Contains(r, "could not be asked")
		if asked == undelivered || notAsked != undelivered {
			t.Errorf("undelivered=%v: reason %q", undelivered, r)
		}
	}
}

// A turn cancelled for a loop records the fixed reason "loop", on the nudge turn and on the final
// turn before the loop failure alike (both end with turnpolicy.EndedLoop), even with no prompt error.
func TestLoopCancelledTurnRecordsLoopReason(t *testing.T) {
	d := &turnDriver{}
	for _, name := range []string{"nudge turn", "final turn"} {
		if got := d.reason(turnpolicy.EndedLoop); got != "loop" {
			t.Errorf("%s: reason = %q, want loop", name, got)
		}
	}
}
