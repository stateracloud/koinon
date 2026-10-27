// SPDX-License-Identifier: Apache-2.0

// Package safego starts goroutines that cannot take the process down by panicking: a panic is
// recovered and logged with its stack (practice B9). Use it for long-running loops and workers;
// short request-scoped goroutines do not need it, net/http recovers handler panics itself.
//
// A recovered panic ends that goroutine. A loop that should go on wraps one iteration in Do.
package safego

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// Go runs fn(ctx) in a new goroutine. A panic is logged at error level, with name and the stack,
// and ends the goroutine.
func Go(ctx context.Context, log *slog.Logger, name string, fn func(ctx context.Context)) {
	go Do(log, name, func() { fn(ctx) })
}

// Do runs fn on the calling goroutine and recovers a panic the same way. It reports whether fn
// panicked.
func Do(log *slog.Logger, name string, fn func()) (panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			panicked = true
			if log == nil {
				log = slog.Default()
			}
			log.Error("goroutine panicked", "goroutine", name, "panic", v, "stack", string(debug.Stack()))
		}
	}()
	fn()
	return false
}
