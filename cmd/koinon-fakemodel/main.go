// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakemodel serves scripted OpenAI- and Anthropic-compatible model responses for
// tests and e2e (ADR 0025). Scripts are YAML files in the scripts directory; see package
// internal/testing/fakemodel for the format.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	scripts := flag.String("scripts", defaultScripts(), "directory of YAML scripts")
	flag.Parse()

	if err := run(*addr, *scripts); err != nil {
		slog.Error("koinon-fakemodel failed", "err", err)
		os.Exit(1)
	}
}

// defaultScripts is the example scripts shipped in the ko image (kodata), or the repository's
// example directory when run from the repository root.
func defaultScripts() string {
	if dir := os.Getenv("KO_DATA_PATH"); dir != "" {
		return filepath.Join(dir, "scripts")
	}
	return "test/fakemodel/scripts"
}

func run(addr, dir string) error {
	scripts, err := fakemodel.LoadDir(dir)
	if err != nil {
		return err
	}
	srv, err := fakemodel.New(scripts...)
	if err != nil {
		return err
	}
	models := make([]string, 0, len(scripts))
	for _, s := range scripts {
		models = append(models, s.Model)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No write timeout: streamed responses and scripted delays may take long.
	hs := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()

	slog.Info("starting koinon-fakemodel", "version", project.Version, "addr", addr, "scripts", dir, "models", models)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
