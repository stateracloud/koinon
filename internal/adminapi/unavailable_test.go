// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"net/http"
	"testing"

	connect "connectrpc.com/connect/v2"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/pricing"
)

// connect.CodeUnavailable (a busy org queue on Run now) must surface as HTTP 503 with code
// unavailable and the original message, not as 500 internal (#529).
func TestUnavailableIs503(t *testing.T) {
	s := adminapi.New(nil, &catalog.Service{}, &tokens.Service{}, nil, pricing.Embedded())
	const msg = "the organisation's queue is busy; run the trigger again in a moment"
	status, e := adminapi.Problem(s, connect.NewError(connect.CodeUnavailable, msg))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want %d", status, http.StatusServiceUnavailable)
	}
	if e.Error.Code != adminapi.Unavailable {
		t.Fatalf("code %q, want %q", e.Error.Code, adminapi.Unavailable)
	}
	if e.Error.Message != msg {
		t.Fatalf("message %q, want %q", e.Error.Message, msg)
	}
}
