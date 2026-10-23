// SPDX-License-Identifier: Apache-2.0

// Command koinon-agentd drives runs: it speaks ACP to the harness in each
// sandbox over OpenShell's exec stream (ADR 0049). For now it only waits for a
// termination signal.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/procoduck/koinon/internal/project"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting koinon-agentd", "version", project.Version)
	<-ctx.Done()
	slog.Info("stopping koinon-agentd")
}
