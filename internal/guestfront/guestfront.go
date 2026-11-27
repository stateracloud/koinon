// SPDX-License-Identifier: Apache-2.0

// Package guestfront is the Koinon guest front (issue #1309): the process that owns a Substrate
// sandbox's guest API listener. It embeds the upstream guest server
// (github.com/agent-substrate/env/guest, unchanged) in its own process, so the upstream server has
// no socket of its own, and it lets a request reach that server only with a valid guest API token
// (internal/guestauth). Agent code in the sandbox can connect to the listener, but it has no token,
// so it cannot start a process outside the launcher.
//
// Only cmd/koinon-guest-front imports this package. The upstream module registers the same proto
// file names as internal/gen/substrate/ateenv, so one binary must not import both.
package guestfront

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/guest/process"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/stateracloud/koinon/internal/guestauth"
)

// Config is the configuration of a front.
type Config struct {
	// Listen is the TCP address of the guest API. Default ":80".
	Listen string
	// Workspace is the upstream guest server's.
	Workspace string
	// LogDir is where the upstream guest server writes the job logs. Default DefaultLogDir. The
	// front makes the directory and checks it before any payload exists (PrepareLogDir).
	LogDir string
	// Keys verify the tokens. Required.
	Keys   []ed25519.PublicKey
	Logger *slog.Logger
	// NoReap turns the reaper off. Only a test that needs an orphan to stay sets it.
	NoReap bool
}

// KeysFromEnv reads the verify keys from guestauth.VerifyKeysEnv and removes the variable from the
// process environment, so no process the guest server starts inherits it. The keys are public; the
// payload does not need them.
func KeysFromEnv() ([]ed25519.PublicKey, error) {
	v := os.Getenv(guestauth.VerifyKeysEnv)
	if err := os.Unsetenv(guestauth.VerifyKeysEnv); err != nil {
		return nil, fmt.Errorf("guestfront: unset %s: %w", guestauth.VerifyKeysEnv, err)
	}
	keys, err := guestauth.DecodeKeys(v)
	if err != nil {
		return nil, fmt.Errorf("guestfront: %s: %w", guestauth.VerifyKeysEnv, err)
	}
	return keys, nil
}

// Front is a guest API listener with the upstream guest server behind it.
type Front struct {
	cfg      Config
	verifier *guestauth.Verifier
	grpc     *grpc.Server
	own      *ownedProcesses
	cleanup  func()
	srv      *http.Server
}

// New builds the upstream guest server and the front. It fails closed without a key.
func New(cfg Config) (*Front, error) {
	if cfg.Listen == "" {
		cfg.Listen = ":80"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.LogDir == "" {
		cfg.LogDir = DefaultLogDir
	}
	v, err := guestauth.NewVerifier(cfg.Keys)
	if err != nil {
		return nil, fmt.Errorf("guestfront: %w", err)
	}
	if err := PrepareLogDir(cfg.LogDir); err != nil {
		return nil, fmt.Errorf("guestfront: %w", err)
	}
	// The upstream server builds the gRPC server and the other services. The front builds the process
	// service itself, from the same upstream package and the same settings as guest.NewServer, so it
	// can record the processes that it starts (ownedProcesses, for the reaper).
	gs, cleanup, err := guest.NewServer(guest.Config{
		ListenAddr:       cfg.Listen,
		LogDir:           cfg.LogDir,
		Workspace:        cfg.Workspace,
		EnableProcess:    false,
		EnableFileSystem: false, // Upload streams a tar on process stdin (#1478): the root front never writes a file
	})
	if err != nil {
		return nil, fmt.Errorf("guestfront: upstream guest server: %w", err)
	}
	trackerCfg := process.DefaultConfig(cfg.LogDir)
	if cfg.Workspace != "" {
		trackerCfg.Workspace = cfg.Workspace
	}
	tracker, err := process.NewTracker(trackerCfg)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("guestfront: process tracker: %w", err)
	}
	own := newOwnedProcesses(process.NewService(tracker))
	ateenvv1alpha.RegisterProcessServiceServer(gs, &ownedService{Service: own.svc, own: own})
	gs.RegisterService(&frontService, struct{}{})
	f := &Front{cfg: cfg, verifier: v, grpc: gs, own: own, cleanup: func() { cleanup(); tracker.Close() }}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	f.srv = &http.Server{
		Handler:           f.Handler(),
		Protocols:         &protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return f, nil
}

// Handler returns the front's HTTP handler: /readyz without a token, everything else through the
// token check to the upstream gRPC server.
func (f *Front) Handler() http.Handler {
	return Gate(f.verifier, f.grpc, f.cfg.Logger)
}

// Serve serves on lis until ctx ends, then stops gracefully.
func (f *Front) Serve(ctx context.Context, lis net.Listener) error {
	defer f.cleanup()
	if !f.cfg.NoReap {
		rctx, stopReaper := context.WithCancel(ctx)
		reaped := make(chan struct{})
		go func() {
			defer close(reaped)
			newReaper(f.own, f.cfg.Logger).run(rctx)
		}()
		defer func() { stopReaper(); <-reaped }()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		f.grpc.GracefulStop()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = f.srv.Shutdown(sctx)
	}()
	f.cfg.Logger.Info("koinon-guest-front listening", "addr", lis.Addr().String(),
		"services", guest.FormatEnabledServices(guest.Config{EnableProcess: true}),
		"workspace", f.cfg.Workspace)
	err := f.srv.Serve(lis)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}

// ListenAndServe listens on the configured address and serves until ctx ends.
func (f *Front) ListenAndServe(ctx context.Context) error {
	lis, err := new(net.ListenConfig).Listen(ctx, "tcp", f.cfg.Listen)
	if err != nil {
		return fmt.Errorf("guestfront: listen on %s: %w", f.cfg.Listen, err)
	}
	return f.Serve(ctx, lis)
}

// Gate puts the token check in front of next. GET and HEAD of /readyz pass without a token: it is
// the wakeup probe and returns no data. Every other request needs a token that v accepts. The
// header is removed before next sees the request. A refused gRPC request gets the status
// Unauthenticated with no reason; the reason goes to logger only.
func Gate(v *guestauth.Verifier, next http.Handler, logger *slog.Logger) http.Handler {
	lim := &refusalLog{logger: logger, every: refusalLogInterval}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		tokens := r.Header.Values(guestauth.Header)
		var err error
		if len(tokens) != 1 {
			err = fmt.Errorf("%w: want one %s header, got %d", guestauth.ErrDenied, guestauth.Header, len(tokens))
		} else {
			err = v.Verify(tokens[0])
		}
		if err != nil {
			lim.refused(r.URL.Path, err)
			refuse(w, r)
			return
		}
		r.Header.Del(guestauth.Header)
		next.ServeHTTP(w, r)
	})
}

// refusalLogInterval is the shortest time between two log lines about refused calls.
const refusalLogInterval = 10 * time.Second

// refusalLog writes at most one line per interval about refused calls, with the number it did not
// write, so agent code that floods the front with calls cannot flood its log.
type refusalLog struct {
	logger *slog.Logger
	every  time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func (l *refusalLog) refused(path string, err error) {
	l.mu.Lock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	n := l.suppressed
	l.last, l.suppressed = now, 0
	l.mu.Unlock()
	l.logger.Warn("guest API call refused", "path", path, "reason", err.Error(), "suppressed", n)
}

// refuse writes a gRPC trailers-only Unauthenticated response, or 401 to a request that is not gRPC.
func refuse(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "16") // codes.Unauthenticated
		w.Header().Set("Grpc-Message", "unauthenticated")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}

// frontService is the front's own gRPC service. It has one method, Bind.
var frontService = grpc.ServiceDesc{
	ServiceName: "koinon.guestfront.v1.Front",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Bind",
		Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(new(emptypb.Empty)); err != nil {
				return nil, err
			}
			return new(emptypb.Empty), nil
		},
	}},
	Metadata: "koinon/guestfront/v1/front",
}
