// SPDX-License-Identifier: Apache-2.0

// Command koinon-api serves the Koinon API and web UI, and carries the operator commands
// (migrate, preflight, ensure-secrets, keys, audit). Its command tree lives in
// internal/cmd/koinonapi, where the chart's contract test can build it (ADR 0087).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stateracloud/koinon/internal/cmd/koinonapi"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := koinonapi.Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
