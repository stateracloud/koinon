// SPDX-License-Identifier: Apache-2.0

// Command koinon-reconciler renders agents into OpenShell sandboxes, provider records and
// agentgateway keys (plan item R2). Its command tree lives in internal/cmd/koinonreconciler, where
// the chart's contract test can build it (ADR 0088).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stateracloud/koinon/internal/cmd/koinonreconciler"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := koinonreconciler.Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
