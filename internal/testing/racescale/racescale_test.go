// SPDX-License-Identifier: Apache-2.0

package racescale_test

import (
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/testing/racescale"
)

func TestD(t *testing.T) {
	got := racescale.D(time.Second)
	if got < time.Second || got%time.Second != 0 {
		t.Fatalf("D(1s) = %v, want a whole multiple of 1s", got)
	}
}
