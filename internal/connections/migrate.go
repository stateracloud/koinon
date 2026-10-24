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
// Kubernetes Secret into the store (plan item PV1): it reads the Secret, seals its fields into
// connections.secret_envelope, deletes the Secret, and only when that delete succeeded clears
// connections.secret_name. A row with an envelope and a secret_name is a move that did not finish
// (the delete failed, or the process stopped); the next start finishes it, so a Secret that could
// not be deleted is never forgotten. A connection whose Secret is missing and which has no envelope
// is logged and skipped. It returns the number of connections whose fields it moved.
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
		name := c.SecretName.String
		sec, found, err := legacy.Get(ctx, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(c.SecretEnvelope) == 0 {
			if !found {
				log.Warn("a connection's Kubernetes Secret is missing; create the connection again",
					"org", c.OrgID.String(), "connection", c.ID.String(), "secret", name)
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
			moved++
			log.Info("moved a connection's secret fields into the store", "org", c.OrgID.String(), "connection", c.ID.String())
		}
		if found {
			if err := legacy.Delete(ctx, name); err != nil {
				log.Warn("moved a connection's secret fields but could not delete its Kubernetes Secret; the next start tries again, or delete it by hand",
					"connection", c.ID.String(), "secret", name, "err", err)
				continue
			}
		}
		if err := q.ClearConnectionSecretName(ctx, store.ClearConnectionSecretNameParams{OrgID: c.OrgID, ID: c.ID}); err != nil {
			errs = append(errs, fmt.Errorf("connection %s: %w", c.ID.String(), err))
		}
	}
	return moved, errors.Join(errs...)
}
