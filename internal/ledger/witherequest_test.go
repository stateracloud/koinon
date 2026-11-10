// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/prices"
)

// A price error names its request and stays classifiable, on the lookup path (a stored rate that
// does not parse) and on the pricing path (a cost that does not fit). No database needed.
func TestWithRequestNamesRequestAndKeepsClass(t *testing.T) {
	bad := &prices.BadPriceError{Err: errors.New("money: invalid price")}
	for name, err := range map[string]error{"lookup": bad, "cost": withRequest("x", bad)} {
		got := withRequest("req-212", err)
		if !strings.Contains(got.Error(), "price request req-212") {
			t.Errorf("%s: %q does not name the request", name, got)
		}
		if r, ok := prices.Unpriceable(got); !ok || r != prices.ReasonUnusablePrice {
			t.Errorf("%s: class lost: %v %v", name, r, ok)
		}
	}
	if r, ok := prices.Unpriceable(withRequest("r", prices.ErrUnpriced)); !ok || r != prices.ReasonNoPrice {
		t.Errorf("ErrUnpriced class lost: %v %v", r, ok)
	}
}
