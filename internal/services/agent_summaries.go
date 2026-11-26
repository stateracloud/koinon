// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/templates"
)

// Page sizes of the agent reads (ADR 0126 section 4: the v0.2 scale track is 1k to 10k agents).
const (
	// ListAgents renders each agent with its versions, so its pages stay at most this size. The
	// default is the maximum, so a client that does not page still gets every agent of an org
	// below it.
	agentsMaxPageSize = 500
	// ListAgentSummaries reads a page in one query.
	agentSummariesDefaultPageSize = 50
	agentSummariesMaxPageSize     = 200
	// maxTeamLength is the longest team label, in characters (the agents.team check).
	maxTeamLength = 64
)

// agentCursor is the position after the last agent of a page. The page token is its JSON in
// base64url: opaque to clients, and only a position (the org comes from the request).
type agentCursor struct {
	// Team is set only for ListAgentSummaries, which orders by team.
	Team      *string   `json:"t,omitempty"`
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

func (c agentCursor) token() string {
	raw, _ := json.Marshal(c) //nolint:errchkjson // a struct of strings and a time always marshals
	return base64.RawURLEncoding.EncodeToString(raw)
}

// parseAgentCursor decodes a page token; withTeam says whether the read orders by team.
func parseAgentCursor(tok string, withTeam bool) (agentCursor, pgtype.UUID, error) {
	var c agentCursor
	var id pgtype.UUID
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil || c.CreatedAt.IsZero() || id.Scan(c.ID) != nil || !id.Valid || (c.Team != nil) != withTeam {
		return agentCursor{}, id, connect.NewError(connect.CodeInvalidArgument, "page_token is not valid")
	}
	return c, id, nil
}

func tsArg(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// ListAgents implements AgentService: one page of the org's agents, oldest first.
func (s *Server) ListAgents(ctx context.Context, req *agentv1alpha1.ListAgentsRequest) (*agentv1alpha1.ListAgentsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	size := pageSize(req.GetPageSize(), agentsMaxPageSize, agentsMaxPageSize)
	arg := store.ListAgentsPageParams{OrgID: c.org, PageSize: int32(size + 1)} //nolint:gosec // at most agentsMaxPageSize+1
	if t := req.GetPageToken(); t != "" {
		cur, id, err := parseAgentCursor(t, false)
		if err != nil {
			return nil, err
		}
		arg.AfterCreatedAt, arg.AfterID = tsArg(cur.CreatedAt), id
	}
	agents, err := s.q.ListAgentsPage(ctx, arg)
	if err != nil {
		return nil, s.dbError(err, "agents")
	}
	res := &agentv1alpha1.ListAgentsResponse{}
	if len(agents) > size {
		agents = agents[:size]
		last := agents[size-1]
		res.NextPageToken = agentCursor{CreatedAt: last.CreatedAt.Time, ID: last.ID.String()}.token()
	}
	slugs := map[pgtype.UUID]string{}
	rows, err := s.q.ListArchivedAgentSandboxes(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "agents")
	}
	archived := map[pgtype.UUID]pgtype.Timestamptz{}
	for _, r := range rows {
		archived[r.AgentID] = r.ArchivedAt
	}
	for _, a := range agents {
		v, err := s.agentView(ctx, s.q, a, slugs, archived)
		if err != nil {
			return nil, s.dbError(err, "agent")
		}
		res.Agents = append(res.Agents, v)
	}
	return res, nil
}

// ListAgentSummaries implements AgentService: one page of agent cards from one query.
func (s *Server) ListAgentSummaries(ctx context.Context, req *agentv1alpha1.ListAgentSummariesRequest) (*agentv1alpha1.ListAgentSummariesResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	size := pageSize(req.GetPageSize(), agentSummariesDefaultPageSize, agentSummariesMaxPageSize)
	now := time.Now()
	day := budget.DayStart(now)
	arg := store.ListAgentSummariesParams{
		OrgID: c.org, UserID: c.p.UserID, DayStart: tsArg(day), PageSize: int32(size + 1), //nolint:gosec // at most agentSummariesMaxPageSize+1
		IsApprover: c.p.HasRole(c.org, store.OrgRoleAdmin), CanAnswer: c.p.HasRole(c.org, store.OrgRoleMember),
	}
	if t := req.GetPageToken(); t != "" {
		cur, id, err := parseAgentCursor(t, true)
		if err != nil {
			return nil, err
		}
		arg.AfterNoTeam = pgtype.Bool{Bool: *cur.Team == "", Valid: true}
		arg.AfterTeam = pgtype.Text{String: *cur.Team, Valid: true}
		arg.AfterCreatedAt, arg.AfterID = tsArg(cur.CreatedAt), id
	}
	rows, err := s.q.ListAgentSummaries(ctx, arg)
	if err != nil {
		return nil, s.dbError(err, "agents")
	}
	res := &agentv1alpha1.ListAgentSummariesResponse{DayStart: timestamppb.New(day)}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[size-1]
		team := last.Team
		res.NextPageToken = agentCursor{Team: &team, CreatedAt: last.CreatedAt.Time, ID: last.ID.String()}.token()
	}
	for _, r := range rows {
		v, err := agentSummaryProto(r, now)
		if err != nil {
			return nil, s.dbError(err, "agent")
		}
		res.Agents = append(res.Agents, v)
	}
	return res, nil
}

// agentSummaryProto renders one summary row. It reads no more rows: the versions' configs come
// with the row.
func agentSummaryProto(r store.ListAgentSummariesRow, now time.Time) (*agentv1alpha1.AgentSummary, error) {
	out := &agentv1alpha1.AgentSummary{
		AgentId:              idString(r.ID),
		DisplayName:          r.DisplayName,
		Status:               agentStatusToProto[r.Status],
		Harness:              r.Harness,
		Model:                r.ModelSetting.String,
		Team:                 r.Team,
		CreatedAt:            tsProto(r.CreatedAt),
		UnhealthyAt:          tsProto(r.UnhealthyAt),
		LastRunAt:            tsProto(r.LastRunAt),
		RunningRuns:          r.RunningRuns,
		InputTokensToday:     r.InputTokensToday,
		CacheReadTokensToday: r.CacheReadTokensToday,
		WaitingOnYou:         r.WaitingOnYou,
	}
	if r.LastRunState != "" {
		out.LastRunState = runStateToProto[store.RunState(r.LastRunState)]
	}
	switch {
	case r.ReportsToAgentID.Valid:
		out.ReportsTo = &agentv1alpha1.AgentReportsTo{
			Target: &agentv1alpha1.AgentReportsTo_AgentId{AgentId: idString(r.ReportsToAgentID)}, DisplayName: r.ReportsToAgentName.String,
		}
	case r.ReportsToUserID.Valid:
		out.ReportsTo = &agentv1alpha1.AgentReportsTo{
			Target: &agentv1alpha1.AgentReportsTo_UserId{UserId: idString(r.ReportsToUserID)}, DisplayName: r.ReportsToUserName.String,
		}
	}
	spent, err := money.ParseNumeric(r.SpentToday)
	if err != nil {
		return nil, err
	}
	out.SpentTodayUsd = spent.String()
	if out.Model == "" && len(r.LatestConfig) > 0 {
		cfg, err := agentconfig.Parse(r.LatestConfig)
		if err != nil {
			return nil, err
		}
		out.Model = cfg.Model
	}
	// The daily limit: the agent's own, else the spendUSD of its active version (budget.AgentDailyLimit).
	var active agentconfig.Config
	if len(r.ActiveConfig) > 0 {
		if active, err = agentconfig.Parse(r.ActiveConfig); err != nil {
			return nil, err
		}
	}
	if r.OwnDailyLimit != "" {
		limit, err := money.ParseNumeric(r.OwnDailyLimit)
		if err != nil {
			return nil, err
		}
		out.DailyLimitUsd = limit.String()
	} else if l := money.FromCents(active.Limits.SpendUSD.Cents()); l > 0 {
		out.DailyLimitUsd = l.String()
	}
	// Only an active agent's active version fires its schedules (internal/triggers).
	if r.Status == store.AgentStatusActive {
		out.NextScheduledAt = nextScheduled(active.Triggers, now)
	}
	return out, nil
}

// nextScheduled is the earliest due time after now of the schedule triggers, as the scheduler
// computes due times (internal/triggers/schedule); nil when there is none.
func nextScheduled(trs []templates.Trigger, now time.Time) *timestamppb.Timestamp {
	var next time.Time
	for _, tr := range trs {
		if tr.Type != templates.TriggerSchedule {
			continue
		}
		sched, loc, err := templates.ParseSchedule(tr.Filter.Cron, tr.Filter.Timezone)
		if err != nil {
			// The scheduler skips a schedule that does not parse; so does the card.
			continue
		}
		if d := sched.Next(now.In(loc)); !d.IsZero() && (next.IsZero() || d.Before(next)) {
			next = d
		}
	}
	if next.IsZero() {
		return nil
	}
	return timestamppb.New(next)
}

// normalizeTeam trims the team label and checks it.
func normalizeTeam(team string) (string, error) {
	team = strings.TrimSpace(team)
	if utf8.RuneCountInString(team) > maxTeamLength {
		return "", connect.NewError(connect.CodeInvalidArgument, "team is longer than 64 characters")
	}
	if !utf8.ValidString(team) || strings.ContainsFunc(team, isHiddenRune) {
		return "", connect.NewError(connect.CodeInvalidArgument, "team has a control or format character, or is not UTF-8")
	}
	return team, nil
}

// isHiddenRune is a control character or a format character (a bidirectional override, a
// zero-width character): the label would show other text than it holds.
func isHiddenRune(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

// SetAgentPlacement implements AgentService: the agent's team and whom it reports to.
func (s *Server) SetAgentPlacement(ctx context.Context, req *agentv1alpha1.SetAgentPlacementRequest) (*agentv1alpha1.SetAgentPlacementResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	team, err := normalizeTeam(req.GetTeam())
	if err != nil {
		return nil, err
	}
	var parentAgent, parentUser pgtype.UUID
	switch t := req.GetReportsTo().GetTarget().(type) {
	case nil:
	case *agentv1alpha1.AgentReportsTo_AgentId:
		if parentAgent, err = parseID("reports_to.agent_id", t.AgentId); err != nil {
			return nil, err
		}
		if parentAgent == id {
			return nil, connect.NewError(connect.CodeInvalidArgument, "an agent cannot report to itself")
		}
	case *agentv1alpha1.AgentReportsTo_UserId:
		if parentUser, err = parseID("reports_to.user_id", t.UserId); err != nil {
			return nil, err
		}
	}
	var out *agentv1alpha1.Agent
	err = s.inAuditTx(requestCtx(ctx), func(q *store.Queries, b audit.Batch) error {
		// The org lock first, then the row: every placement change takes them in this order.
		if err := q.LockAgentReportsTo(ctx, c.org); err != nil {
			return err
		}
		cur, err := q.GetAgentForUpdate(ctx, store.GetAgentForUpdateParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if cur.Status == store.AgentStatusArchived {
			return connect.NewError(connect.CodeFailedPrecondition, "the agent is archived")
		}
		// A parent that does not change is not checked again: a team change keeps a parent that was
		// archived or left the org later.
		if parentAgent.Valid && parentAgent != cur.ReportsToAgentID {
			// The read is scoped to the org: an agent of another org is not found.
			p, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: parentAgent})
			if err != nil {
				return s.dbError(err, "reports_to agent")
			}
			if p.Status == store.AgentStatusArchived {
				return connect.NewError(connect.CodeFailedPrecondition, "the reports_to agent is archived")
			}
			cycle, err := q.AgentReportsToReaches(ctx, store.AgentReportsToReachesParams{OrgID: c.org, StartID: parentAgent, TargetID: id})
			if err != nil {
				return err
			}
			if cycle {
				return connect.NewError(connect.CodeFailedPrecondition, "reports_to would make a cycle: that agent reports to this one")
			}
		}
		if parentUser.Valid && parentUser != cur.ReportsToUserID {
			member, err := q.HasOrgMembership(ctx, store.HasOrgMembershipParams{OrgID: c.org, UserID: parentUser})
			if err != nil {
				return err
			}
			if !member {
				return connect.NewError(connect.CodeNotFound, "reports_to user is not a member of this org")
			}
		}
		a := cur
		if cur.Team != team || cur.ReportsToAgentID != parentAgent || cur.ReportsToUserID != parentUser {
			a, err = q.SetAgentPlacement(ctx, store.SetAgentPlacementParams{
				OrgID: c.org, ID: id, Team: team, ReportsToAgentID: parentAgent, ReportsToUserID: parentUser,
			})
			if err != nil {
				return err
			}
			b.Org(audit.Event{
				Org: c.org, Actor: c.auditActor(), Action: "agent.placement.set", TargetKind: "agent", TargetID: id.String(),
				Before: placementAudit(cur), After: placementAudit(a),
			})
		}
		out, err = s.agentView(ctx, q, a, nil, nil)
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.SetAgentPlacementResponse{Agent: out}, nil
}

func placementAudit(a store.Agent) map[string]any {
	return map[string]any{"team": a.Team, "reportsToAgentId": idString(a.ReportsToAgentID), "reportsToUserId": idString(a.ReportsToUserID)}
}

// reportsToProto is the stored reports-to of an agent, without a name.
func reportsToProto(a store.Agent) *agentv1alpha1.AgentReportsTo {
	switch {
	case a.ReportsToAgentID.Valid:
		return &agentv1alpha1.AgentReportsTo{Target: &agentv1alpha1.AgentReportsTo_AgentId{AgentId: idString(a.ReportsToAgentID)}}
	case a.ReportsToUserID.Valid:
		return &agentv1alpha1.AgentReportsTo{Target: &agentv1alpha1.AgentReportsTo_UserId{UserId: idString(a.ReportsToUserID)}}
	}
	return nil
}
