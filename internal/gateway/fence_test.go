// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stateracloud/koinon/internal/gateway"
)

func TestCheckFence(t *testing.T) {
	ctx := context.Background()
	if err := gateway.CheckFence(ctx); err != nil {
		t.Fatalf("no fence: %v", err)
	}
	lost := errors.New("lease lost")
	held := true
	fctx := gateway.WithFence(ctx, func() error {
		if held {
			return nil
		}
		return lost
	})
	if err := gateway.CheckFence(fctx); err != nil {
		t.Fatalf("holding fence: %v", err)
	}
	held = false
	if err := gateway.CheckFence(fctx); !errors.Is(err, lost) {
		t.Fatalf("lost fence: got %v", err)
	}
	cctx, cancel := context.WithCancel(gateway.WithFence(ctx, func() error { return nil }))
	cancel()
	if err := gateway.CheckFence(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: got %v", err)
	}
}
