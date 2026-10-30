// SPDX-License-Identifier: Apache-2.0

package semconv_test

import (
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// The pinned names are part of what backends read. Moving to a newer commit of the conventions must
// change this test on purpose, together with the golden file of emitted spans.
func TestPinned(t *testing.T) {
	if semconv.Commit[:7] != "3fe8460" {
		t.Fatalf("pinned commit = %s, want 3fe8460", semconv.Commit)
	}
	want := map[string]string{
		semconv.GenAIUsageCacheReadTokens:  "gen_ai.usage.cache_read.input_tokens",
		semconv.GenAIUsageCacheWriteTokens: "gen_ai.usage.cache_write.input_tokens",
		semconv.GenAIUsageReasoningTokens:  "gen_ai.usage.reasoning.output_tokens",
		semconv.GenAIConversationID:        "gen_ai.conversation.id",
		semconv.KoinonTest:                 "koinon.test",
		semconv.KoinonRequestedByKind:      "koinon.requested_by.kind",
	}
	for got, name := range want {
		if got != name {
			t.Errorf("attribute = %q, want %q", got, name)
		}
		if semconv.IsKoinon(name) != (name[:7] == "koinon.") {
			t.Errorf("IsKoinon(%q) is wrong", name)
		}
	}
	if got := semconv.SpanName(semconv.OperationChat, "gpt-5"); got != "chat gpt-5" {
		t.Errorf("SpanName = %q", got)
	}
	if got := semconv.SpanName(semconv.OperationInvokeAgent, ""); got != "invoke_agent" {
		t.Errorf("SpanName without a subject = %q", got)
	}
}

// The router parents model-call spans on the root's derived ID and derives theirs from the request.
func TestDerivedSpanIDs(t *testing.T) {
	a, b := [16]byte{1}, [16]byte{2}
	if semconv.RootSpanID(a) == semconv.RootSpanID(b) || semconv.RootSpanID(a) != semconv.RootSpanID(a) {
		t.Fatal("RootSpanID is not a function of the trace ID")
	}
	if semconv.ModelCallSpanID(a, "r1") != semconv.ModelCallSpanID(a, "r1") ||
		semconv.ModelCallSpanID(a, "r1") == semconv.ModelCallSpanID(a, "r2") ||
		semconv.ModelCallSpanID(a, "r1") == semconv.ModelCallSpanID(b, "r1") {
		t.Fatal("ModelCallSpanID must depend on the trace and the request, and on nothing else")
	}
	if semconv.ModelCallSpanID(a, "r1") == semconv.RootSpanID(a) {
		t.Fatal("a model call must not get the root's ID")
	}
}
