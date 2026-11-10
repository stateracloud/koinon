// SPDX-License-Identifier: Apache-2.0

package scopesync_test

import (
	"context"
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/codehost/scopesync"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// A held install reads no code-host scope: Request returns a finished channel at once and starts no
// reading (the nil registry would panic if one started) (issue #462, ADR 0107).
func TestRequestRefusesWhileHeld(t *testing.T) {
	s := scopesync.New(nil, nil, nil, nil).WithRestoreHold(restorehold.NewGuard(true))
	select {
	case <-s.Request(context.Background(), store.Connection{}, audit.Actor{}):
	case <-time.After(time.Second):
		t.Fatal("Request did not return a closed channel")
	}
}
