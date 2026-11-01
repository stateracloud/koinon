// SPDX-License-Identifier: Apache-2.0

// Command koinon-agentd drives runs: it claims them from Postgres and speaks ACP to the harness
// in each agent's sandbox. Its command lives in internal/cmd/koinonagentd, where the chart's
// contract test can build it (ADR 0088).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stateracloud/koinon/internal/cmd/koinonagentd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := koinonagentd.Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
