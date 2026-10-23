// SPDX-License-Identifier: Apache-2.0

// Command koinon-reconciler renders orgs, grants and agents into Kubernetes
// and OpenShell objects. For now it only waits for a termination signal.
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

	slog.Info("starting koinon-reconciler", "version", project.Version)
	<-ctx.Done()
	slog.Info("stopping koinon-reconciler")
}
