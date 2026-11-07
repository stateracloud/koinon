// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// FormatActor is the audit actor of the startup re-derivation of model formats.
var FormatActor = audit.System("koinon-api:model-formats")

// RederiveFormats applies internal/modelformat to every model whose format no admin set, in every
// org, and audits each change with the actor "system koinon-api:model-formats" (ADR 0069, decision 2). This is
// how a change to the table, such as the OpenRouter flip, reaches existing models; it is the
// counterpart of prices.CompleteMigrated's startup fill. It is idempotent and safe from every
// replica, and returns how many models it changed. A model an admin overrides is never touched.
func (s *Service) RederiveFormats(ctx context.Context) (int, error) {
	rows, err := store.New(s.pool).ListModelsToRederiveFormat(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		want := string(modelformat.Default(r.ProviderType, r.Name))
		if want == r.Format {
			continue
		}
		changed := false
		err := s.tx(ctx, func(q *store.Queries) error {
			done, err := q.SetDerivedModelFormat(ctx, store.SetDerivedModelFormatParams{OrgID: r.OrgID, ID: r.ID, Format: want})
			if err != nil || done == 0 {
				return err // a save that raced us set the format: leave it
			}
			changed = true
			return audit.RecordErr(ctx, q, audit.Event{
				Org: r.OrgID, Actor: FormatActor, Action: "model.format_rederived", TargetKind: "model", TargetID: r.ID.String(),
				Before: map[string]any{"name": r.Name, "format": r.Format},
				After:  map[string]any{"name": r.Name, "format": want},
			})
		})
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	if n > 0 {
		s.log.Info("re-derived model formats from the format table", "models", n)
	}
	return n, nil
}

// ModelFormats returns the formats a model may use on a provider, preferred first, and the one it
// gets by default, so a form offers what Create and Update will accept.
func (s *Service) ModelFormats(ctx context.Context, org pgtype.UUID, providerID, name string) (string, error) {
	prov, err := s.GetProvider(ctx, org, providerID)
	if err != nil {
		return "", err
	}
	return string(modelformat.Default(prov.Type, name)), nil
}

// TypeInfo says what a provider type offers, so a form can show it before the provider exists: the
// model formats, and whether the type takes an Anthropic workspace ID.
type TypeInfo struct {
	Formats     []string
	WorkspaceID bool
}

// ProviderTypeInfo returns what a provider type offers. The rules are the ones validation applies.
func ProviderTypeInfo(typ string) TypeInfo {
	var f []string
	for _, x := range modelformat.Offered(typ) {
		f = append(f, string(x))
	}
	return TypeInfo{Formats: f, WorkspaceID: typ == WorkspaceProviderType}
}
