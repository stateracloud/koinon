// SPDX-License-Identifier: Apache-2.0

package providertype_test

import (
	"testing"

	"github.com/stateracloud/koinon/internal/providertype"
)

func TestRejectedRequestsBilling(t *testing.T) {
	for _, typ := range []string{providertype.Anthropic, providertype.OpenRouter, providertype.OpenAI} {
		if providertype.For(typ).BillsRejectedRequests {
			t.Errorf("%s is documented not to bill rejected requests", typ)
		}
		if !providertype.Known(typ) {
			t.Errorf("%s should have a row", typ)
		}
	}
	// Custom, local and unknown providers keep the conservative charge.
	for _, typ := range []string{"", "ollama", "fake", "vllm", "my-company-gateway"} {
		if !providertype.For(typ).BillsRejectedRequests {
			t.Errorf("%q has no documented behaviour and must be assumed to bill", typ)
		}
	}
}
