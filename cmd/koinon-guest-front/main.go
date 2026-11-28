// SPDX-License-Identifier: Apache-2.0

// Command koinon-guest-front is the entrypoint of a Substrate sandbox (issue #1309). It serves the
// guest API on one TCP listener and runs the upstream guest server
// (github.com/agent-substrate/env/guest, unchanged) in its own process behind a token check
// (internal/guestfront). It replaces only upstream cmd/ate-env-guest/main.go; Koinon uses no
// sidecars.
//
//	koinon-guest-front [-listen :80] [-workspace DIR] [-log-dir DIR]
//
// The public keys that verify the driver's tokens come from KOINON_GUEST_VERIFY_KEYS, which the
// actor template sets. The front removes the variable from its environment before it starts, and
// it does not start without a key. Exit 64 is a usage or configuration error.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stateracloud/koinon/internal/guestfront"
)

func main() {
	listen := flag.String("listen", ":80", "address to serve the guest API on")
	logDir := flag.String("log-dir", "", "directory for process logs, made and checked by the front (default: "+guestfront.DefaultLogDir+")")
	workspace := flag.String("workspace", "/", "workspace root directory")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Read and remove the keys first, before anything can start a process.
	keys, err := guestfront.KeysFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "koinon-guest-front:", err)
		os.Exit(64)
	}
	f, err := guestfront.New(guestfront.Config{
		Listen: *listen, Workspace: *workspace, LogDir: *logDir, Keys: keys, Logger: log,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "koinon-guest-front:", err)
		os.Exit(64)
	}
	if err := serve(f); err != nil {
		log.Error("koinon-guest-front stopped", "err", err)
		os.Exit(1)
	}
}

func serve(f *guestfront.Front) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return f.ListenAndServe(ctx)
}
