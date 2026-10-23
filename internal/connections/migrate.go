// SPDX-License-Identifier: Apache-2.0

package connections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/stateracloud/koinon/internal/db/store"
)

// MigrateLegacy moves the secret fields of connections that still live in their own
// Kubernetes Secret into the store (plan item PV1): it reads the Secret, seals its fields
// into connections.secret_envelope, and deletes the Secret only after the row is updated. A
// connection whose Secret is missing is logged and skipped. It is idempotent, so a crash
// between the update and the delete is repaired by the next start. It returns the number of
// connections moved.
func MigrateLegacy(ctx context.Context, q *store.Queries, legacy SecretStore, sealer Sealer, log *slog.Logger) (int, error) {
	rows, err := q.ListConnectionsToMigrate(ctx)
	if err != nil {
		return 0, fmt.Errorf("connections: list connections to migrate: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if legacy == nil {
		return 0, errors.New("connections: connections still hold Kubernetes Secrets but koinon-api has no Kubernetes access to move them")
	}
	var errs []error
	moved := 0
	for _, c := range rows {
		sec, found, err := legacy.Get(ctx, c.SecretName.String)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !found {
			log.Warn("a connection's Kubernetes Secret is missing; create the connection again",
				"org", c.OrgID.String(), "connection", c.ID.String(), "secret", c.SecretName.String)
			continue
		}
		env, err := SealFields(sealer, c.OrgID, c.ID, sec.Data)
		if err != nil {
			errs = append(errs, fmt.Errorf("connection %s: %w", c.ID.String(), err))
			continue
		}
		if _, err := q.SetConnectionEnvelope(ctx, store.SetConnectionEnvelopeParams{OrgID: c.OrgID, ID: c.ID, SecretEnvelope: env}); err != nil {
			errs = append(errs, fmt.Errorf("connection %s: %w", c.ID.String(), err))
			continue
		}
		if err := legacy.Delete(ctx, c.SecretName.String); err != nil {
			// The row is moved; the next start finds no work for it, so the stray Secret
			// stays. Say so.
			log.Warn("moved a connection's secret fields but could not delete its Kubernetes Secret; delete it by hand",
				"connection", c.ID.String(), "secret", c.SecretName.String, "err", err)
		}
		moved++
		log.Info("moved a connection's secret fields into the store", "org", c.OrgID.String(), "connection", c.ID.String())
	}
	return moved, errors.Join(errs...)
}
