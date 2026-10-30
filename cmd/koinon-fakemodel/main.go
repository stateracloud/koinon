// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakemodel serves scripted OpenAI- and Anthropic-compatible model responses for
// tests and e2e (ADR 0025). Scripts are YAML files, built in from test/fakemodel/scripts or read from -scripts; see package
// internal/testing/fakemodel for the format.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	fakemodelscripts "github.com/stateracloud/koinon/test/fakemodel"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	scripts := flag.String("scripts", "", "directory of YAML scripts (default: the example scripts built into the binary)")
	strict := flag.Bool("strict-messages", false, "refuse Messages requests the way Anthropic does: unknown fields, and beta-only fields without their anthropic-beta")
	standin := flag.Bool("standin", false, "serve no model: answer every request with an empty 418 and X-Koinon-Standin: 1 (a stand-in for agentgateway in the escape suite, ADR 0080)")
	flag.Parse()

	if *standin {
		if err := serve(*addr, standinHandler(), "standin"); err != nil {
			slog.Error("koinon-fakemodel failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*addr, *scripts, *strict); err != nil {
		slog.Error("koinon-fakemodel failed", "err", err)
		os.Exit(1)
	}
}

func run(addr, dir string, strict bool) error {
	scripts, err := loadScripts(dir)
	if err != nil {
		return err
	}
	srv, err := fakemodel.New(scripts...)
	if err != nil {
		return err
	}
	srv.SetStrictMessages(strict)
	models := make([]string, 0, len(scripts))
	for _, s := range scripts {
		models = append(models, s.Model)
	}

	slog.Info("starting koinon-fakemodel", "version", project.Version, "addr", addr, "scripts", cmp.Or(dir, "built-in"), "strictMessages", strict, "models", models)
	return serve(addr, srv, "")
}

// serve runs handler on addr until SIGINT or SIGTERM. mode names a non-default mode in the log.
func serve(addr string, handler http.Handler, mode string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No write timeout: streamed responses and scripted delays may take long.
	hs := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()

	if mode != "" {
		slog.Info("starting koinon-fakemodel", "version", project.Version, "addr", addr, "mode", mode)
	}
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadScripts reads dir, or the example scripts embedded from test/fakemodel/scripts when dir is
// empty.
func loadScripts(dir string) ([]fakemodel.Script, error) {
	if dir == "" {
		return fakemodel.LoadFS(fakemodelscripts.FS, "scripts")
	}
	return fakemodel.LoadDir(dir)
}
