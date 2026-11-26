// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/stateracloud/koinon/internal/schemagate"
)

// schemaHolder is what holdWork needs of the schema gate.
type schemaHolder interface {
	Hold(ctx context.Context, work func(context.Context) error) error
}

// holdWork runs the converging work under the schema gate (issue 680): no reconcile pass, spend
// settlement or cost reconciliation starts until the schema is supported. Once a check reads an
// unsupported schema the work stops, as on SIGTERM (a pass in progress is cancelled; the leader lock
// goes with its connection), and the reconciler exits with an error. The restarted container waits
// for the gate again.
func holdWork(ctx context.Context, gate schemaHolder, work func(context.Context) error, log *slog.Logger) error {
	err := gate.Hold(ctx, work)
	if errors.Is(err, schemagate.ErrUnsupported) {
		log.Error("stopped reconciling: the database schema is not supported by this build; exiting so that the restarted container waits for the schema gate", "err", err)
		return err
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil // stopped while waiting for the gate: a normal shutdown
	}
	return err
}
