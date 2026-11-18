// SPDX-License-Identifier: Apache-2.0

// Command koinon-gateway is Koinon's own gateway, MCP path (ADR 0097 step 2, issue 278). Its command
// lives in internal/cmd/koinongateway, where the chart's contract test can build it (ADR 0088).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stateracloud/koinon/internal/cmd/koinongateway"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := koinongateway.Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
