// SPDX-License-Identifier: Apache-2.0

package restorehold_test

import (
	"errors"
	"testing"

	"github.com/stateracloud/koinon/internal/restorehold"
)

func TestGuard(t *testing.T) {
	var nilGuard *restorehold.Guard
	if nilGuard.Held() || nilGuard.Err() != nil {
		t.Fatal("the nil guard is not held")
	}
	if g := restorehold.NewGuard(false); g.Held() || g.Err() != nil {
		t.Fatal("a guard made not held does not refuse")
	}
	g := restorehold.NewGuard(true)
	if !g.Held() || !errors.Is(g.Err(), restorehold.ErrHeld) {
		t.Fatal("a held guard returns ErrHeld")
	}
	if g.Err().Error() != restorehold.Message {
		t.Fatalf("message = %q", g.Err().Error())
	}
}
