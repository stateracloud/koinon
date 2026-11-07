// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stateracloud/koinon/internal/auth"
)

// SQLSTATEs that mean the schema is not there yet: the migration Job has not run.
const (
	sqlUndefinedTable   = "42P01"
	sqlInvalidSchema    = "3F000"
	sqlCannotConnectNow = "57P03" // the server is starting up or shutting down
)

// retryableBootstrap reports whether a bootstrap error can go away by itself on a fresh
// install: the database is not reachable yet, or reachable but not migrated yet. Everything
// else (an unreadable password file, bad credentials, a constraint violation) is permanent.
func retryableBootstrap(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Checked before ConnectError: a refused login is a PgError inside a ConnectError.
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case sqlUndefinedTable, sqlInvalidSchema, sqlCannotConnectNow:
			return true
		}
		return strings.HasPrefix(pgErr.Code, "08") // connection exception
	}
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return true
	}
	// Not net.Error: syscall.Errno implements it too, so a missing password file would match.
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return true // the database Service does not resolve yet
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || pgconn.SafeToRetry(err) || pgconn.Timeout(err)
}

// retryBootstrap calls bootstrap until it succeeds, fails permanently, ctx ends or wait has
// passed, sleeping interval between attempts.
func retryBootstrap[T any](ctx context.Context, wait, interval time.Duration, bootstrap func() (T, error)) (T, error) {
	retryUntil := time.Now().Add(wait)
	for {
		res, err := bootstrap()
		if err == nil || !retryableBootstrap(err) || ctx.Err() != nil || time.Now().After(retryUntil) {
			return res, err
		}
		slog.Warn("bootstrap failed; retrying (is the database up and migrated?)", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// reportBootstrap logs what a finished bootstrap did. The direct start and the background retry
// after a deferred install write both call it, so the same lines appear either way.
func reportBootstrap(res auth.BootstrapResult, loginEnabled bool) {
	slog.Info("bootstrap done", "created_admin", res.CreatedAdmin, "created_org", res.CreatedOrg,
		"created_credential", res.CreatedCredential, "revoked_bootstrap_sessions", res.RevokedSessions,
		"bootstrap_login", loginEnabled)
	if res.MissingPasswordFile {
		slog.Warn("bootstrap login is enabled but no password is set; set --bootstrap-password-file")
	}
}

// bootstrapRetryWrite is the write the background retry runs after the install audit writer role
// refused bootstrap. It runs bootstrap with the retry goroutine's own ctx (not the serve ctx, which
// outlives the goroutine's stop) and reports the result like the direct path.
func bootstrapRetryWrite(run func(context.Context) (auth.BootstrapResult, error), loginEnabled bool) func(context.Context) error {
	return func(ctx context.Context) error {
		res, err := run(ctx)
		if err != nil {
			return err
		}
		reportBootstrap(res, loginEnabled)
		return nil
	}
}
