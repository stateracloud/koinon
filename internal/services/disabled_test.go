// SPDX-License-Identifier: Apache-2.0

package services

import (
	"fmt"
	"testing"

	connect "connectrpc.com/connect/v2"

	"github.com/stateracloud/koinon/internal/outward"
)

// A target an admin switched off answers a Test or an echo check with FailedPrecondition and the
// message, not "internal error" (ADR 0110 decision 5).
func TestDisabledTargetIsFailedPrecondition(t *testing.T) {
	s := &Server{}
	err := fmt.Errorf("wrapped: %w", outward.ErrDisabled)
	for name, got := range map[string]error{
		"dbError":    s.dbError(err, "x"),
		"traceError": traceAPI{s}.traceError(err),
		"echoError":  s.echoError(err),
	} {
		if connect.CodeOf(got) != connect.CodeFailedPrecondition {
			t.Errorf("%s: code = %v (%v)", name, connect.CodeOf(got), got)
		}
	}
}
