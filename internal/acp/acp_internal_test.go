// SPDX-License-Identifier: Apache-2.0

package acp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

// classify (acp.go) drops every message without an id except session/update. For notifications
// that is right only while client has neither of the two handlers below: the SDK would hand
// extension methods and elicitation/complete to them. If this test fails, a notification now has
// a handler: make classify keep it.
//
// The other Unstable* interfaces (UnstableCreateElicitation, UnstableConnectMcp,
// UnstableDisconnectMcp) need no guard: they are requests. Without an id they are dropped by the
// rule above, and with an id they do not pass through classify. client does not implement them.
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

func TestTruncateMethod(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"ab\u00e9", 3, "ab"},
		{"a\u20ac", 3, "a"},
		{"a\U0001F600", 4, "a"},
		{"\u00e9\u00e9", 2, "\u00e9"},
		{"\u00e9", 1, ""},
	} {
		if got := truncateMethod(c.in, c.n); got != c.want {
			t.Errorf("truncateMethod(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	long := strings.Repeat("\u00e9", 80)
	if got := truncateMethod(long, maxDropMethodBytes); len(got) > maxDropMethodBytes || !strings.HasPrefix(long, got) {
		t.Errorf("long method cut to %d bytes: %q", len(got), got)
	}
}
