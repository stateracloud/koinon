// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	connect "connectrpc.com/connect/v2"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/templates"
)

// SyncTemplates records the given templates (normally templates.All(), the
// ones embedded in the binary) in the templates table (plan items T1, D1).
// Each distinct content is its own row, keyed by slug and content hash, so
// agents keep pointing at the exact content they were made from; syncing
// known content only bumps last_synced_at, which makes it the current
// version of its slug. Safe to run concurrently from several replicas.
func SyncTemplates(ctx context.Context, db store.DBTX, ts []*templates.Template) ([]store.Template, error) {
	q := store.New(db)
	out := make([]store.Template, 0, len(ts))
	for _, t := range ts {
		spec, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", t.Metadata.ID, err)
		}
		row, err := q.UpsertTemplate(ctx, store.UpsertTemplateParams{
			Slug:        t.Metadata.ID,
			ContentHash: strings.TrimPrefix(t.Hash, "sha256:"),
			DisplayName: t.Metadata.DisplayName,
			Description: t.Metadata.Description,
			Spec:        spec,
		})
		if err != nil {
			return nil, fmt.Errorf("sync template %s: %w", t.Metadata.ID, err)
		}
		out = append(out, row)
	}
	return out, nil
}

// decodeTemplate parses a templates row's spec.
func decodeTemplate(row store.Template) (*templates.Template, error) {
	var t templates.Template
	if err := json.Unmarshal(row.Spec, &t); err != nil {
		return nil, fmt.Errorf("decode template %s: %w", row.ID.String(), err)
	}
	t.Hash = "sha256:" + row.ContentHash
	return &t, nil
}

func templateProto(row store.Template, t *templates.Template) *templatev1alpha1.Template {
	out := &templatev1alpha1.Template{
		Id:           idString(row.ID),
		Slug:         row.Slug,
		ContentHash:  row.ContentHash,
		Version:      t.Metadata.Version,
		DisplayName:  row.DisplayName,
		Description:  row.Description,
		Model:        t.Spec.Model.Name,
		Thinking:     t.Spec.Model.Thinking,
		Instructions: t.Spec.Instructions,
		Grants:       grantsProto(t.Spec.Grants),
		Triggers:     triggersProto(t.Spec.Triggers),
		// The limits a run would get, the platform defaults filling what the template leaves out.
		RunLimits:      runLimitsProto(agentconfig.FromTemplate(t, row.ContentHash).RunLimits()),
		SpendLimitUsd:  t.Spec.Limits.SpendUSD.String(),
		DefaultFlavor:  flavorProto(string(t.Spec.Harness.Default)), //nolint:staticcheck // deprecated field, still set for older clients
		DefaultHarness: string(t.Spec.Harness.Default),
		Ephemeral:      t.Spec.Ephemeral,
	}
	for _, f := range t.Spec.Harness.Allowed {
		out.AllowedHarnesses = append(out.AllowedHarnesses, string(f))
		if p := flavorProto(string(f)); p != templatev1alpha1.Flavor_FLAVOR_UNSPECIFIED {
			out.AllowedFlavors = append(out.AllowedFlavors, p) //nolint:staticcheck // deprecated field, still set for older clients
		}
	}
	return out
}

// ListTemplates implements TemplateService. Templates are platform data, so
// any signed-in user may list them.
func (s *Server) ListTemplates(ctx context.Context, _ *templatev1alpha1.ListTemplatesRequest) (*templatev1alpha1.ListTemplatesResponse, error) {
	if _, ok := auth.PrincipalFrom(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	rows, err := s.q.ListCurrentTemplates(ctx)
	if err != nil {
		return nil, s.dbError(err, "templates")
	}
	res := &templatev1alpha1.ListTemplatesResponse{}
	for _, row := range rows {
		t, err := decodeTemplate(row)
		if err != nil {
			s.log.Error("skipping undecodable template", "err", err)
			continue
		}
		res.Templates = append(res.Templates, templateProto(row, t))
	}
	return res, nil
}

// runLimitsProto is the limits a run of an agent version gets.
func runLimitsProto(l agentconfig.RunLimits) *templatev1alpha1.RunLimits {
	return &templatev1alpha1.RunLimits{
		MaxDurationSeconds: int64(l.WallClock.Seconds()), MaxModelRequests: l.MaxModelRequests, MaxSpendUsd: l.MaxSpend.String(),
	}
}
