// SPDX-License-Identifier: Apache-2.0

package acp

import (
	"context"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

// classify (acp.go) drops every notification except session/update, because the SDK would hand
// no other notification to client. That holds only while client has neither of these handlers.
// If this test fails, a notification the SDK delivers now has a handler: make classify keep it.
func TestClientImplementsNoNotificationHandler(t *testing.T) {
	var c any = client{}
	if _, ok := c.(sdk.ExtensionMethodHandler); ok {
		t.Error("client implements sdk.ExtensionMethodHandler: classify must stop dropping extension methods")
	}
	if _, ok := c.(interface {
		UnstableCompleteElicitation(context.Context, sdk.UnstableCompleteElicitationNotification) error
	}); ok {
		t.Error("client implements UnstableCompleteElicitation: classify must stop dropping elicitation/complete")
	}
}
