// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
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
		Scope:          templatev1alpha1.TemplateScope_TEMPLATE_SCOPE_PLATFORM,
	}
	if row.OrgID.Valid {
		out.Scope, out.OrgId = templatev1alpha1.TemplateScope_TEMPLATE_SCOPE_ORG, idString(row.OrgID)
	}
	for _, f := range t.Spec.Harness.Allowed {
		out.AllowedHarnesses = append(out.AllowedHarnesses, string(f))
		if p := flavorProto(string(f)); p != templatev1alpha1.Flavor_FLAVOR_UNSPECIFIED {
			out.AllowedFlavors = append(out.AllowedFlavors, p) //nolint:staticcheck // deprecated field, still set for older clients
		}
	}
	return out
}

// ListTemplates implements TemplateService. Every signed-in user may list the platform templates. A
// request that names an org adds the org's own templates and needs the viewer role there: no other
// org's template is ever listed.
func (s *Server) ListTemplates(ctx context.Context, req *templatev1alpha1.ListTemplatesRequest) (*templatev1alpha1.ListTemplatesResponse, error) {
	if _, ok := auth.PrincipalFrom(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	rows, err := s.q.ListCurrentTemplates(ctx)
	if err != nil {
		return nil, s.dbError(err, "templates")
	}
	if req.GetOrgId() != "" {
		c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
		if err != nil {
			return nil, err
		}
		own, err := s.q.ListOrgTemplates(ctx, c.org)
		if err != nil {
			return nil, s.dbError(err, "templates")
		}
		rows = append(rows, own...)
		// Sorted by slug; the platform template first when an org template has the same slug.
		slices.SortStableFunc(rows, func(a, b store.Template) int {
			if c := strings.Compare(a.Slug, b.Slug); c != 0 {
				return c
			}
			switch {
			case a.OrgID.Valid == b.OrgID.Valid:
				return 0
			case !a.OrgID.Valid:
				return -1
			}
			return 1
		})
	}
	return &templatev1alpha1.ListTemplatesResponse{Templates: s.templateProtos(rows)}, nil
}

// templateProtos converts templates rows in order. It logs and skips a row whose spec does not decode.
func (s *Server) templateProtos(rows []store.Template) []*templatev1alpha1.Template {
	out := make([]*templatev1alpha1.Template, 0, len(rows))
	for _, row := range rows {
		t, err := decodeTemplate(row)
		if err != nil {
			s.log.Error("skipping undecodable template", "err", err)
			continue
		}
		out = append(out, templateProto(row, t))
	}
	return out
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugOf derives a template slug from a display name: lowercase letters, digits and single hyphens,
// at most 64 characters, as the templates table requires.
func slugOf(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}
	return s
}

var slugRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// CreateTemplateFromAgent implements TemplateService: "save as template" (ADR 0126 section 5). It needs
// the role that creates agents. The template holds the definition of the agent's latest version and
// leaves out what ties the agent to the org's resources: bindings (connections, repositories), the
// budget and the history.
func (s *Server) CreateTemplateFromAgent(ctx context.Context, req *templatev1alpha1.CreateTemplateFromAgentRequest) (*templatev1alpha1.CreateTemplateFromAgentResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	agentID, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	name, err := checkDisplayName(req.GetDisplayName())
	if err != nil {
		return nil, err
	}
	description := strings.TrimSpace(req.GetDescription())
	slug := strings.TrimSpace(req.GetSlug())
	if slug == "" {
		if slug = slugOf(name); slug == "" {
			return nil, invalidArg("slug is required: display_name has no letter or digit to derive it from")
		}
	}
	if !slugRE.MatchString(slug) {
		return nil, invalidArg("slug must be lowercase letters, digits and hyphens, 1 to 64 characters, not starting or ending with a hyphen")
	}

	a, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: agentID})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	_, cfg, err := latestConfig(ctx, s.q, a)
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	platform, err := s.q.ListCurrentTemplates(ctx)
	if err != nil {
		return nil, s.dbError(err, "templates")
	}
	// An org template never takes the slug of a platform template: clients read a slug as one template.
	if slices.ContainsFunc(platform, func(p store.Template) bool { return p.Slug == slug }) {
		return nil, connect.NewError(connect.CodeAlreadyExists, "a platform template has the slug "+slug)
	}
	t := templateFromConfig(cfg, a, slug, name, description)
	spec, err := json.Marshal(t)
	if err != nil {
		return nil, s.dbError(err, "template")
	}
	sum := sha256.Sum256(spec)
	hash := hex.EncodeToString(sum[:])

	var row store.Template
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
		var err error
		row, err = q.CreateOrgTemplate(ctx, store.CreateOrgTemplateParams{
			OrgID: c.org, Slug: slug, ContentHash: hash, DisplayName: name, Description: description, Spec: spec,
			SourceAgentID: a.ID, CreatedBy: c.actor(),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return connect.NewError(connect.CodeAlreadyExists, "the org has a template with the slug "+slug)
		}
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "template.create", TargetKind: "template", TargetID: row.ID.String(),
			After: map[string]any{"slug": slug, "displayName": name, "sourceAgentId": a.ID.String()},
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "template")
	}
	t.Hash = "sha256:" + hash
	return &templatev1alpha1.CreateTemplateFromAgentResponse{Template: templateProto(row, t)}, nil
}

// templateFromConfig is the template an agent version makes. The grants are proposals again: the
// bindings stay with the agent, so an agent made from the template picks its own connections and
// repositories.
//
// The agent's own settings (model, thinking, sandbox resources) are columns of the agent that win over
// its version, so the template takes them. A trigger's notify target that names a person or a channel
// is dropped (the org's default applies): a template cannot name a person.
func templateFromConfig(cfg agentconfig.Config, a store.Agent, slug, name, description string) *templates.Template {
	h := templates.HarnessName(a.Harness)
	model, thinking, res := cfg.Model, cfg.Thinking, cfg.Resources
	if a.Model.Valid && a.Model.String != "" {
		model = a.Model.String
	}
	if a.Thinking.Valid && a.Thinking.String != "" {
		thinking = a.Thinking.String
	}
	for _, o := range []struct {
		dst *string
		src pgtype.Text
	}{
		{&res.CPU.Request, a.SandboxCpuRequest},
		{&res.CPU.Limit, a.SandboxCpuLimit},
		{&res.Memory.Request, a.SandboxMemoryRequest},
		{&res.Memory.Limit, a.SandboxMemoryLimit},
	} {
		if o.src.Valid && o.src.String != "" {
			*o.dst = o.src.String
		}
	}
	triggers := slices.Clone(cfg.Triggers)
	for i := range triggers {
		if n := triggers[i].Notify; n != nil && n.Kind != templates.NotifyRole {
			triggers[i].Notify = nil
		}
	}
	return &templates.Template{
		APIVersion: "templates.koinon.dev/v1alpha1",
		Kind:       "Template",
		Metadata:   templates.Metadata{ID: slug, Version: "1", DisplayName: name, Description: description},
		Spec: templates.Spec{
			Harness:      templates.Harness{Allowed: []templates.HarnessName{h}, Default: h, HarnessSettings: cfg.Harness},
			Ephemeral:    cfg.Ephemeral,
			Model:        templates.Model{Name: model, Thinking: thinking},
			Instructions: cfg.Instructions,
			Grants:       cfg.Grants,
			Limits:       cfg.Limits,
			Triggers:     triggers,
			Questions:    cfg.Questions,
			Resources:    res,
		},
	}
}

// runLimitsProto is the limits a run of an agent version gets.
func runLimitsProto(l agentconfig.RunLimits) *templatev1alpha1.RunLimits {
	return &templatev1alpha1.RunLimits{
		MaxDurationSeconds: int64(l.WallClock.Seconds()), MaxModelRequests: l.MaxModelRequests, MaxSpendUsd: l.MaxSpend.String(),
	}
}
