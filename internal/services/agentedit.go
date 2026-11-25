// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/templates"
)

// The agent editor of ADR 0126 section 5: CreateAgent (the "Blank agent" form) and UpdateAgent. Both
// run a person's definition through the checks a template and CreateAgentFromTemplate apply (the
// template schema's bounds, CheckCatalog, bindGrants, ValidateTriggers, agentmodel's served-model
// check) and only ever write a version that is proposed: grants, triggers and limits take effect
// when an admin or owner activates it (ADR 0020).

// BlankAgentDailySpend is the daily spend limit an agent without a template starts with: the lowest
// limit of the shipped templates. A blank form does not set it; admins change it with
// BudgetService, the same as a template agent's.
const blankDailySpendCents = 100

// BlankAgentDailySpend is blankDailySpendCents as money.
var BlankAgentDailySpend = money.FromCents(blankDailySpendCents)

// The template schema's bounds: spec.instructions in characters, and the run limits.
const (
	maxInstructions  = 32768
	maxModelRequests = 100000
	maxRunSpendCents = 99999999
)

// The AgentDefinition fields UpdateAgent changes, by update_mask path.
const (
	fieldDisplayName  = "display_name"
	fieldInstructions = "instructions"
	fieldModel        = "model"
	fieldHarness      = "harness"
	fieldGrants       = "grants"
	fieldTriggers     = "triggers"
	fieldRunLimits    = "run_limits"
)

// versionFields change the agent's definition: a change adds a version.
var versionFields = []string{fieldInstructions, fieldModel, fieldGrants, fieldTriggers, fieldRunLimits}

var definitionFields = append([]string{fieldDisplayName, fieldHarness}, versionFields...)

func invalidArg(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(format, args...))
}

func checkDisplayName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", invalidArg("display_name is required")
	case utf8.RuneCountInString(name) > 200:
		return "", invalidArg("display_name is longer than 200 characters")
	}
	return name, nil
}

func checkInstructions(s string) (string, error) {
	switch {
	case strings.TrimSpace(s) == "":
		return "", invalidArg("instructions are required")
	case utf8.RuneCountInString(s) > maxInstructions:
		return "", invalidArg("instructions are longer than %d characters", maxInstructions)
	}
	return s, nil
}

// pickHarness returns the named harness, or the platform's default harness for an empty name.
func pickHarness(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return harness.All()[0].Name, nil
	}
	if _, ok := harness.Lookup(name); !ok {
		return "", invalidArg("harness %q is not one of %s", name, strings.Join(harness.Names(), ", "))
	}
	return name, nil
}

// runLimitsFromProto returns the run limits of p, with the values the form does not show
// (continuations, the deprecated token cap) carried over from prev. The bounds are the template
// schema's.
func runLimitsFromProto(p *templatev1alpha1.RunLimits, prev templates.RunLimits) (templates.RunLimits, error) {
	out := prev
	d := time.Duration(p.GetMaxDurationSeconds()) * time.Second
	switch {
	case p.GetMaxDurationSeconds() < 0:
		return out, invalidArg("run_limits.max_duration_seconds is negative")
	case d > templates.MaxRunDuration:
		return out, invalidArg("run_limits.max_duration_seconds exceeds %s", templates.MaxRunDuration)
	case p.GetMaxModelRequests() < 0:
		return out, invalidArg("run_limits.max_model_requests is negative")
	case p.GetMaxModelRequests() > maxModelRequests:
		return out, invalidArg("run_limits.max_model_requests exceeds %d", maxModelRequests)
	case len(p.GetMaxSpendUsd()) > len("999999.99"):
		return out, invalidArg("run_limits.max_spend_usd exceeds 999999.99")
	}
	out.MaxDuration = templates.Duration{Duration: d}
	out.MaxModelRequests = p.GetMaxModelRequests()
	out.MaxSpendUSD = 0
	if s := strings.TrimSpace(p.GetMaxSpendUsd()); s != "" {
		raw, err := json.Marshal(s)
		if err != nil {
			return out, invalidArg("run_limits.max_spend_usd: %v", err)
		}
		if err := out.MaxSpendUSD.UnmarshalJSON(raw); err != nil {
			return out, invalidArg("run_limits.max_spend_usd: %v", err)
		}
		if out.MaxSpendUSD > maxRunSpendCents {
			return out, invalidArg("run_limits.max_spend_usd exceeds 999999.99")
		}
	}
	return out, nil
}

// grantsFromProto checks the grants of a definition as a template's grants are checked (one per
// connector type, a known preset, at least one scope, CheckCatalog) and binds them as
// CreateAgentFromTemplate does (bindGrants). A grant keeps the targets bounds of prev's grant on the
// same connector, so a template agent stays within its template's bounds; a new grant has none.
// The binding talks to the code host: call it outside a transaction.
func (s *Server) grantsFromProto(ctx context.Context, org pgtype.UUID, in []*templatev1alpha1.Grant, prev []templates.Grant) ([]templates.Grant, []agentconfig.Binding, error) {
	types, err := connectors.All()
	if err != nil {
		return nil, nil, s.dbError(err, "connector types")
	}
	cat := templates.Catalog{Scopes: map[string][]string{}}
	for _, t := range types {
		for _, sc := range t.Spec.Scopes {
			cat.Scopes[t.Metadata.ID] = append(cat.Scopes[t.Metadata.ID], sc.ID)
		}
	}
	grants := make([]templates.Grant, 0, len(in))
	var bindings []*agentv1alpha1.GrantBinding
	for i, p := range in {
		field := fmt.Sprintf("grants[%d]", i)
		connector := strings.TrimSpace(p.GetConnector())
		if slices.ContainsFunc(grants, func(g templates.Grant) bool { return g.Connector == connector }) {
			return nil, nil, invalidArg("%s: more than one grant for connector %q", field, connector)
		}
		var preset templates.Preset
		for k, v := range presetsToProto {
			if v == p.GetPreset() {
				preset = k
			}
		}
		if preset == "" {
			return nil, nil, invalidArg("%s: the preset is read-only, standard or trusted", field)
		}
		if len(p.GetScopes()) == 0 {
			return nil, nil, invalidArg("%s: a grant lists at least one scope", field)
		}
		if len(p.GetPurpose()) > 500 {
			return nil, nil, invalidArg("%s: the purpose is longer than 500 characters", field)
		}
		g := templates.Grant{Connector: connector, Scopes: slices.Clone(p.GetScopes()), Preset: preset, Purpose: p.GetPurpose()}
		if j := slices.IndexFunc(prev, func(o templates.Grant) bool { return o.Connector == connector }); j >= 0 && prev[j].Targets != nil {
			t := *prev[j].Targets
			g.Targets = &t
		}
		grants = append(grants, g)
		if p.GetConnectionId() != "" || len(p.GetRepositories()) > 0 {
			bindings = append(bindings, &agentv1alpha1.GrantBinding{
				Connector: connector, ConnectionId: p.GetConnectionId(), Repositories: p.GetRepositories(),
			})
		}
	}
	t := &templates.Template{Spec: templates.Spec{Grants: grants}}
	if err := t.CheckCatalog(cat); err != nil {
		return nil, nil, invalidArg("%v", err)
	}
	bound, err := s.bindGrants(ctx, org, t, bindings)
	if err != nil {
		return nil, nil, err
	}
	return grants, bound, nil
}

// checkNotifyMembers checks that every person a trigger asks is a member of the org who can answer
// (member or higher).
func checkNotifyMembers(ctx context.Context, q *store.Queries, org pgtype.UUID, triggers []templates.Trigger) error {
	for i, t := range triggers {
		if t.Notify == nil || t.Notify.Kind != templates.NotifyUser {
			continue
		}
		m, err := q.FindOrgMemberByIdent(ctx, store.FindOrgMemberByIdentParams{OrgID: org, Ident: t.Notify.User})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.Role == store.OrgRoleViewer) {
			return invalidArg("triggers[%d]: notify: %s is not a member who can answer questions", i, t.Notify.User)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// grantAudit renders grants and their bindings for an audit event.
func grantAudit(cfg agentconfig.Config) []map[string]any {
	out := make([]map[string]any, 0, len(cfg.Grants))
	for _, g := range cfg.Grants {
		m := map[string]any{"connector": g.Connector, "scopes": g.Scopes, "preset": string(g.Preset)}
		if b, ok := cfg.Binding(g.Connector); ok {
			m["connectionId"], m["targets"] = b.ConnectionID, b.Targets
		}
		out = append(out, m)
	}
	return out
}

// instructionsAudit names instructions by length and digest: the text is in the version.
func instructionsAudit(s string) map[string]any {
	sum := sha256.Sum256([]byte(s))
	return map[string]any{"chars": utf8.RuneCountInString(s), "sha256": hex.EncodeToString(sum[:])}
}

func runLimitsAudit(l templates.RunLimits) map[string]any {
	return map[string]any{
		"maxDuration": l.MaxDuration.String(), "maxModelRequests": l.MaxModelRequests, "maxSpendUSD": l.MaxSpendUSD.String(),
	}
}

// versionAudit renders the version fields of cfg that fields names.
func versionAudit(cfg agentconfig.Config, fields []string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		switch f {
		case fieldInstructions:
			out["instructions"] = instructionsAudit(cfg.Instructions)
		case fieldModel:
			out["model"] = settingLabel(cfg.Model)
		case fieldGrants:
			out["grants"] = grantAudit(cfg)
		case fieldTriggers:
			out["triggers"] = triggerAudit(cfg.Triggers, true)
		case fieldRunLimits:
			out["runLimits"] = runLimitsAudit(cfg.Limits.Run)
		}
	}
	return out
}

func settingLabel(m string) string {
	if m == "" {
		return "default"
	}
	return m
}

// CreateAgent implements AgentService: a draft agent without a template (ADR 0126 section 5).
func (s *Server) CreateAgent(ctx context.Context, req *agentv1alpha1.CreateAgentRequest) (*agentv1alpha1.CreateAgentResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	d := req.GetDefinition()
	if d == nil {
		return nil, invalidArg("definition is required")
	}
	name, err := checkDisplayName(d.GetDisplayName())
	if err != nil {
		return nil, err
	}
	chosen, err := pickHarness(d.GetHarness())
	if err != nil {
		return nil, err
	}
	instructions, err := checkInstructions(d.GetInstructions())
	if err != nil {
		return nil, err
	}
	triggers, err := triggersFromProto(d.GetTriggers())
	if err != nil {
		return nil, invalidArg("%v", err)
	}
	run, err := runLimitsFromProto(d.GetRunLimits(), templates.RunLimits{})
	if err != nil {
		return nil, err
	}
	model, err := s.agentModels.CheckChoice(requestCtx(ctx), s.q, c.org, d.GetModel(), time.Now())
	if err != nil {
		return nil, s.agentModelError(err)
	}
	grants, bindings, err := s.grantsFromProto(ctx, c.org, d.GetGrants(), nil)
	if err != nil {
		return nil, err
	}
	cfg := agentconfig.Config{
		Instructions: instructions, Model: model, Grants: grants, Bindings: bindings, Triggers: triggers,
		Limits: templates.Limits{Run: run, SpendUSD: templates.USD(blankDailySpendCents)},
	}
	config, err := cfg.Marshal()
	if err != nil {
		return nil, s.dbError(err, "agent config")
	}

	var out *agentv1alpha1.Agent
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
		if err := checkNotifyMembers(ctx, q, c.org, triggers); err != nil {
			return err
		}
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: c.org, DisplayName: name, Harness: chosen, CreatedBy: c.actor()})
		if err != nil {
			return err
		}
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: c.org, AgentID: a.ID, Config: config, CreatedBy: c.actor()})
		if err != nil {
			return err
		}
		if err := q.InsertAgentBudgetDefault(ctx, store.InsertAgentBudgetDefaultParams{
			OrgID: c.org, AgentID: a.ID, DailyLimit: BlankAgentDailySpend.Numeric(), UpdatedBy: "default",
		}); err != nil {
			return err
		}
		if a, err = q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: a.ID}); err != nil {
			return err
		}
		if out, err = s.agentView(ctx, q, a, nil, nil); err != nil {
			return err
		}
		after := versionAudit(cfg, versionFields)
		after["displayName"], after["harness"], after["version"], after["blank"] = name, chosen, v.Version, true
		after["dailyLimitUSD"] = BlankAgentDailySpend.String()
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "agent.create", TargetKind: "agent", TargetID: a.ID.String(), After: after,
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.CreateAgentResponse{Agent: out}, nil
}

// canEdit reports whether c may change agent a: an admin or owner may change any agent of the org; a
// member only a draft that they created and that was never activated (ADR 0126 section 5). An
// activation stays an admin's or owner's step either way.
func canEdit(c caller, a store.Agent) error {
	if c.p.HasRole(c.org, store.OrgRoleAdmin) {
		return nil
	}
	if a.Status == store.AgentStatusDraft && !a.ActiveVersionID.Valid && a.CreatedBy.Valid && a.CreatedBy == c.actor() {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, "this needs the admin role or higher; a member changes only a draft they created")
}

// latestConfig returns the config of a's latest version.
func latestConfig(ctx context.Context, q *store.Queries, a store.Agent) (store.AgentVersion, agentconfig.Config, error) {
	versions, err := q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: a.OrgID, AgentID: a.ID})
	if err != nil {
		return store.AgentVersion{}, agentconfig.Config{}, err
	}
	i := slices.IndexFunc(versions, func(v store.AgentVersion) bool { return v.Version == a.LatestVersion })
	if i < 0 {
		return store.AgentVersion{}, agentconfig.Config{}, connect.NewError(connect.CodeFailedPrecondition, "agent has no version to base the change on")
	}
	cfg, err := agentconfig.Parse(versions[i].Config)
	return versions[i], cfg, err
}

// checkBase refuses a change to an archived agent or one based on a version that is not the latest.
func checkBase(a store.Agent, base int32) error {
	if a.Status == store.AgentStatusArchived {
		return connect.NewError(connect.CodeFailedPrecondition, "agent is archived")
	}
	if base != a.LatestVersion {
		return connect.NewError(connect.CodeFailedPrecondition, "base_version does not match the agent's latest version; review it again")
	}
	return nil
}

// UpdateAgent implements AgentService.
func (s *Server) UpdateAgent(ctx context.Context, req *agentv1alpha1.UpdateAgentRequest) (*agentv1alpha1.UpdateAgentResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	mask := map[string]bool{}
	for _, p := range req.GetUpdateMask().GetPaths() {
		if !slices.Contains(definitionFields, p) {
			return nil, invalidArg("update_mask: %q is not one of %s", p, strings.Join(definitionFields, ", "))
		}
		mask[p] = true
	}
	if len(mask) == 0 {
		return nil, invalidArg("update_mask names no field")
	}
	d := req.GetDefinition()

	// The checks that need no lock. The agent is read first so a caller who may not change it gets
	// PermissionDenied before Koinon asks the code host about a binding.
	a, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	if err := canEdit(c, a); err != nil {
		return nil, err
	}
	if err := checkBase(a, req.GetBaseVersion()); err != nil {
		return nil, err
	}
	_, base, err := latestConfig(ctx, s.q, a)
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	next := base
	if mask[fieldInstructions] {
		if next.Instructions, err = checkInstructions(d.GetInstructions()); err != nil {
			return nil, err
		}
	}
	if mask[fieldModel] {
		if next.Model, err = s.agentModels.CheckChoice(requestCtx(ctx), s.q, c.org, d.GetModel(), time.Now()); err != nil {
			return nil, s.agentModelError(err)
		}
	}
	if mask[fieldTriggers] {
		if next.Triggers, err = triggersFromProto(d.GetTriggers()); err != nil {
			return nil, invalidArg("%v", err)
		}
	}
	if mask[fieldRunLimits] {
		if next.Limits.Run, err = runLimitsFromProto(d.GetRunLimits(), base.Limits.Run); err != nil {
			return nil, err
		}
	}
	if mask[fieldGrants] {
		if next.Grants, next.Bindings, err = s.grantsFromProto(ctx, c.org, d.GetGrants(), base.Grants); err != nil {
			return nil, err
		}
	}
	name, chosen := a.DisplayName, a.Harness
	if mask[fieldDisplayName] {
		if name, err = checkDisplayName(d.GetDisplayName()); err != nil {
			return nil, err
		}
	}
	if mask[fieldHarness] {
		if chosen, err = pickHarness(d.GetHarness()); err != nil {
			return nil, err
		}
		if a.TemplateID.Valid {
			row, err := s.q.GetTemplate(ctx, a.TemplateID)
			if err != nil {
				return nil, s.dbError(err, "template")
			}
			t, err := decodeTemplate(row)
			if err != nil {
				return nil, s.dbError(err, "template")
			}
			if !slices.Contains(t.Spec.Harness.Allowed, templates.HarnessName(chosen)) {
				return nil, invalidArg("harness is not allowed by the template")
			}
		}
		if next.Harness.PluginInstalls && !harness.SupportsPluginInstalls(chosen) {
			return nil, invalidArg("harness %q cannot install plugins, which the agent's version turns on", chosen)
		}
	}
	var changed []string
	for _, f := range versionFields {
		if mask[f] {
			changed = append(changed, f)
		}
	}

	var out *agentv1alpha1.Agent
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
		// The row lock makes the base_version check and the change one step: a concurrent change
		// waits, then fails the check against the version this one created.
		cur, err := q.GetAgentForUpdate(ctx, store.GetAgentForUpdateParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if err := canEdit(c, cur); err != nil {
			return err
		}
		if err := checkBase(cur, req.GetBaseVersion()); err != nil {
			return err
		}
		if chosen != cur.Harness && cur.ActiveVersionID.Valid {
			return connect.NewError(connect.CodeFailedPrecondition, "the harness is fixed once the agent was activated")
		}
		if mask[fieldTriggers] {
			if err := checkNotifyMembers(ctx, q, c.org, next.Triggers); err != nil {
				return err
			}
		}
		before := map[string]any{"version": cur.LatestVersion}
		after := map[string]any{}
		// A harness change also adds a version, though the config is the same: an admin activates
		// a version number, so a change after their review must not pass under the number they
		// reviewed (ADR 0020).
		if len(changed) > 0 || chosen != cur.Harness {
			raw, err := next.Marshal()
			if err != nil {
				return err
			}
			v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: c.org, AgentID: cur.ID, Config: raw, CreatedBy: c.actor()})
			if err != nil {
				return err
			}
			maps(before, versionAudit(base, changed))
			maps(after, versionAudit(next, changed))
			after["version"] = v.Version
		}
		if name != cur.DisplayName || chosen != cur.Harness {
			if _, err := q.UpdateAgentProfile(ctx, store.UpdateAgentProfileParams{
				OrgID: c.org, ID: cur.ID, DisplayName: name, Harness: chosen,
			}); errors.Is(err, pgx.ErrNoRows) {
				return connect.NewError(connect.CodeAborted, "agent changed concurrently; retry")
			} else if err != nil {
				return err
			}
			if name != cur.DisplayName {
				before["displayName"], after["displayName"] = cur.DisplayName, name
			}
			if chosen != cur.Harness {
				before["harness"], after["harness"] = cur.Harness, chosen
			}
		}
		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: cur.ID})
		if err != nil {
			return err
		}
		if out, err = s.agentView(ctx, q, a, nil, nil); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "agent.update", TargetKind: "agent", TargetID: cur.ID.String(),
			Before: before, After: after,
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.UpdateAgentResponse{Agent: out}, nil
}

// maps copies src into dst.
func maps(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}
