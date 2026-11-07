// SPDX-License-Identifier: Apache-2.0

// Command pgserver starts the Postgres server every process of a hack/go-test.sh run shares
// (pgtest.ServerVar, old issue 224) and keeps it until it is told to stop.
//
// It writes the server's superuser DSN to -dsn-file once Postgres accepts connections, then waits
// for SIGTERM or SIGINT and removes the container. If it dies without that, the testcontainers
// reaper removes the container.
//
// pgserver holds the run's reaper connection on purpose (#171): the Postgres container it keeps
// until SIGTERM keeps one client connected to the Ryuk of the TESTCONTAINERS_SESSION_ID that
// hack/go-test.sh exported, so Ryuk does not idle out between packages (testcontainers-go#3867).
// Do not start Postgres lazily or release the container early. Without Docker (testenv's check, so KOINON_TESTENV_UNAVAILABLE
// applies) it exits 3 and writes nothing: the suites then start their own containers or skip.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func main() { os.Exit(run()) }

func run() int {
	dsnFile := flag.String("dsn-file", "", "file to write the server's DSN to (required)")
	flag.Parse()
	if *dsnFile == "" {
		fmt.Fprintln(os.Stderr, "pgserver: -dsn-file is required")
		return 2
	}
	if err := testenv.Docker.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "pgserver: %v\n", err)
		return 3
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	startCtx, startCancel := context.WithTimeout(ctx, 3*time.Minute)
	dsn, stop, err := pgtest.StartServer(startCtx)
	startCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgserver: start Postgres: %v\n", err)
		return 1
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Minute)
		defer stopCancel()
		if err := stop(stopCtx); err != nil {
			fmt.Fprintf(os.Stderr, "pgserver: remove Postgres: %v\n", err)
		}
	}()
	// Written beside and renamed, so a reader never sees half a DSN.
	tmp := *dsnFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(dsn+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "pgserver: %v\n", err)
		return 1
	}
	if err := os.Rename(tmp, *dsnFile); err != nil {
		fmt.Fprintf(os.Stderr, "pgserver: %v\n", err)
		return 1
	}
	<-ctx.Done()
	return 0
}
