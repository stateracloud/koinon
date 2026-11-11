// SPDX-License-Identifier: Apache-2.0

package services

import (
	"encoding/json"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/flavor"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/templates"
)

func idString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return id.String()
}

func tsProto(t pgtype.Timestamptz) *timestamppb.Timestamp {
	if !t.Valid {
		return nil
	}
	return timestamppb.New(t.Time)
}

// payloadProto decodes a jsonb object. Payloads are always objects; anything
// else is dropped rather than failing the whole response.
func payloadProto(raw []byte) *structpb.Struct {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func flavorProto(f string) templatev1alpha1.Flavor {
	d, _ := flavor.Lookup(store.AgentFlavor(f))
	return d.Proto
}

func flavorFromProto(f templatev1alpha1.Flavor) (store.AgentFlavor, bool) {
	d, ok := flavor.FromProto(f)
	return d.Name, ok
}

var presetsToProto = map[templates.Preset]templatev1alpha1.Preset{
	templates.PresetReadOnly: templatev1alpha1.Preset_PRESET_READ_ONLY,
	templates.PresetStandard: templatev1alpha1.Preset_PRESET_STANDARD,
	templates.PresetTrusted:  templatev1alpha1.Preset_PRESET_TRUSTED,
}

func grantsProto(gs []templates.Grant) []*templatev1alpha1.Grant {
	out := make([]*templatev1alpha1.Grant, 0, len(gs))
	for _, g := range gs {
		pg := &templatev1alpha1.Grant{
			Connector: g.Connector,
			Scopes:    g.Scopes,
			Preset:    presetsToProto[g.Preset],
			Purpose:   g.Purpose,
		}
		if g.Targets != nil {
			pg.TargetsMin, pg.TargetsMax = int32(g.Targets.Min), int32(g.Targets.Max) //nolint:gosec // schema-bounded
		}
		out = append(out, pg)
	}
	return out
}

// agentGrantsProto renders an agent version's grants with their bindings.
func agentGrantsProto(cfg agentconfig.Config) []*templatev1alpha1.Grant {
	out := grantsProto(cfg.Grants)
	for _, g := range out {
		if b, ok := cfg.Binding(g.GetConnector()); ok {
			g.ConnectionId, g.Repositories = b.ConnectionID, slices.Clone(b.Targets)
		}
	}
	return out
}

var taskStateToProto = map[store.TaskState]taskv1alpha1.TaskState{
	store.TaskStateSubmitted:     taskv1alpha1.TaskState_TASK_STATE_SUBMITTED,
	store.TaskStateWorking:       taskv1alpha1.TaskState_TASK_STATE_WORKING,
	store.TaskStateInputRequired: taskv1alpha1.TaskState_TASK_STATE_INPUT_REQUIRED,
	store.TaskStateCompleted:     taskv1alpha1.TaskState_TASK_STATE_COMPLETED,
	store.TaskStateCanceled:      taskv1alpha1.TaskState_TASK_STATE_CANCELED,
	store.TaskStateFailed:        taskv1alpha1.TaskState_TASK_STATE_FAILED,
}

func taskStateFromProto(s taskv1alpha1.TaskState) (store.TaskState, bool) {
	for k, v := range taskStateToProto {
		if v == s {
			return k, true
		}
	}
	return "", false
}

var actorKindToProto = map[store.TaskActorKind]taskv1alpha1.ActorKind{
	store.TaskActorKindUser:   taskv1alpha1.ActorKind_ACTOR_KIND_USER,
	store.TaskActorKindAgent:  taskv1alpha1.ActorKind_ACTOR_KIND_AGENT,
	store.TaskActorKindSystem: taskv1alpha1.ActorKind_ACTOR_KIND_SYSTEM,
}

var creatorKindToProto = map[store.TaskCreatorKind]taskv1alpha1.CreatorKind{
	store.TaskCreatorKindUser:    taskv1alpha1.CreatorKind_CREATOR_KIND_USER,
	store.TaskCreatorKindTrigger: taskv1alpha1.CreatorKind_CREATOR_KIND_TRIGGER,
	store.TaskCreatorKindAgent:   taskv1alpha1.CreatorKind_CREATOR_KIND_AGENT,
}

func taskProto(t store.Task) *taskv1alpha1.Task {
	return &taskv1alpha1.Task{
		Id:      idString(t.ID),
		OrgId:   idString(t.OrgID),
		Title:   t.Title,
		State:   taskStateToProto[t.State],
		AgentId: idString(t.AgentID),
		CreatedBy: &taskv1alpha1.Creator{
			Kind:      creatorKindToProto[t.CreatedByKind],
			UserId:    idString(t.CreatedByUserID),
			AgentId:   idString(t.CreatedByAgentID),
			TriggerId: idString(t.CreatedByTriggerID),
		},
		LastEventSeq: t.LastEventSeq,
		CreatedAt:    tsProto(t.CreatedAt),
		UpdatedAt:    tsProto(t.UpdatedAt),
		FinishedAt:   tsProto(t.FinishedAt),
		// Triggers and agents create tasks too; a direct message's thread is the one the agent's
		// direct-message creator ID marks (TR3).
		DirectMessageThread: t.CreatedByKind == store.TaskCreatorKindTrigger && t.CreatedByTriggerID == DirectMessageTriggerID(t.AgentID),
	}
}

func taskEventProto(e store.TaskEvent) *taskv1alpha1.TaskEvent {
	return &taskv1alpha1.TaskEvent{
		Seq:          e.Seq,
		Kind:         e.Kind,
		ActorKind:    actorKindToProto[e.ActorKind],
		ActorUserId:  idString(e.ActorUserID),
		ActorAgentId: idString(e.ActorAgentID),
		RunId:        idString(e.RunID),
		Payload:      payloadProto(e.Payload),
		CreatedAt:    tsProto(e.CreatedAt),
	}
}

var runStateToProto = map[store.RunState]runv1alpha1.RunState{
	store.RunStateQueued:    runv1alpha1.RunState_RUN_STATE_QUEUED,
	store.RunStateStarting:  runv1alpha1.RunState_RUN_STATE_STARTING,
	store.RunStateRunning:   runv1alpha1.RunState_RUN_STATE_RUNNING,
	store.RunStateParked:    runv1alpha1.RunState_RUN_STATE_PARKED,
	store.RunStateSucceeded: runv1alpha1.RunState_RUN_STATE_SUCCEEDED,
	store.RunStateFailed:    runv1alpha1.RunState_RUN_STATE_FAILED,
	store.RunStateCanceled:  runv1alpha1.RunState_RUN_STATE_CANCELED,
}

var failureClassToProto = map[store.RunFailureClass]runv1alpha1.FailureClass{
	store.RunFailureClassTransient:          runv1alpha1.FailureClass_FAILURE_CLASS_TRANSIENT,
	store.RunFailureClassThrottled:          runv1alpha1.FailureClass_FAILURE_CLASS_THROTTLED,
	store.RunFailureClassContextOverflow:    runv1alpha1.FailureClass_FAILURE_CLASS_CONTEXT_OVERFLOW,
	store.RunFailureClassBudget:             runv1alpha1.FailureClass_FAILURE_CLASS_BUDGET,
	store.RunFailureClassPolicy:             runv1alpha1.FailureClass_FAILURE_CLASS_POLICY,
	store.RunFailureClassFatal:              runv1alpha1.FailureClass_FAILURE_CLASS_FATAL,
	store.RunFailureClassSandboxUnavailable: runv1alpha1.FailureClass_FAILURE_CLASS_SANDBOX_UNAVAILABLE,
	store.RunFailureClassLoop:               runv1alpha1.FailureClass_FAILURE_CLASS_LOOP,
}

var parkReasonToProto = map[store.RunParkReason]runv1alpha1.ParkReason{
	store.RunParkReasonApproval:      runv1alpha1.ParkReason_PARK_REASON_APPROVAL,
	store.RunParkReasonBudget:        runv1alpha1.ParkReason_PARK_REASON_BUDGET,
	store.RunParkReasonA2a:           runv1alpha1.ParkReason_PARK_REASON_A2A,
	store.RunParkReasonInputRequired: runv1alpha1.ParkReason_PARK_REASON_INPUT_REQUIRED,
}

func runProto(r store.Run) *runv1alpha1.Run {
	out := &runv1alpha1.Run{
		Id:                  idString(r.ID),
		OrgId:               idString(r.OrgID),
		TicketId:            idString(r.TaskID),
		AgentId:             idString(r.AgentID),
		AgentVersionId:      idString(r.AgentVersionID),
		State:               runStateToProto[r.State],
		OutcomeReason:       r.OutcomeReason.String,
		MaxWallClockSeconds: int64(r.MaxWallClockSeconds),
		MaxModelRequests:    r.MaxModelRequests.Int64,
		TokensUsed:          r.TokensUsed,
		LastEventSeq:        r.LastEventSeq,
		CreatedAt:           tsProto(r.CreatedAt),
		StartedAt:           tsProto(r.StartedAt),
		FinishedAt:          tsProto(r.FinishedAt),
	}
	if spend, ok, err := budget.RunSpendLimit(r); err == nil && ok {
		out.MaxSpendUsd = spend.String()
	}
	if r.ParkReason.Valid {
		out.ParkReason = parkReasonToProto[r.ParkReason.RunParkReason]
	}
	if r.FailureClass.Valid {
		out.FailureClass = failureClassToProto[r.FailureClass.RunFailureClass]
	}
	return out
}

func runsProto(rs []store.Run) []*runv1alpha1.Run {
	out := make([]*runv1alpha1.Run, 0, len(rs))
	for _, r := range rs {
		out = append(out, runProto(r))
	}
	return out
}

func runEventProto(e store.RunEvent) *runv1alpha1.RunEvent {
	return &runv1alpha1.RunEvent{
		RunId:     idString(e.RunID),
		Seq:       e.Seq,
		Kind:      e.Kind,
		Payload:   payloadProto(e.Payload),
		CreatedAt: tsProto(e.CreatedAt),

		ContentPurged: e.ContentPurgedAt.Valid,
	}
}

var triggerTypeToProto = map[templates.TriggerType]templatev1alpha1.TriggerType{
	templates.TriggerSchedule:      templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE,
	templates.TriggerDirectMessage: templatev1alpha1.TriggerType_TRIGGER_TYPE_DIRECT_MESSAGE,
	templates.TriggerCodeHostEvent: templatev1alpha1.TriggerType_TRIGGER_TYPE_CODE_HOST_EVENT,
	templates.TriggerChannelEvent:  templatev1alpha1.TriggerType_TRIGGER_TYPE_CHANNEL_EVENT,
}

func triggersProto(ts []templates.Trigger) []*templatev1alpha1.Trigger {
	out := make([]*templatev1alpha1.Trigger, 0, len(ts))
	for _, t := range ts {
		out = append(out, &templatev1alpha1.Trigger{
			Id: t.ID, Type: triggerTypeToProto[t.Type], Cron: t.Filter.Cron, Timezone: t.Filter.Timezone,
			Instruction: t.Instruction, Notify: notifyToProto(t.Notify),
		})
	}
	return out
}

var notifyKindToProto = map[templates.NotifyKind]templatev1alpha1.NotifyTargetKind{
	templates.NotifyUser:    templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_USER,
	templates.NotifyRole:    templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_ROLE,
	templates.NotifyChannel: templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_CHANNEL,
}

// notifyToProto is the wire form of a trigger's notify target; a nil target is an unset message.
func notifyToProto(n *templates.NotifyTarget) *templatev1alpha1.NotifyTarget {
	if n == nil {
		return nil
	}
	return &templatev1alpha1.NotifyTarget{
		Kind: notifyKindToProto[n.Kind], UserId: n.User, Role: auth.RoleToProto(n.Role), ChannelId: n.Channel,
	}
}

// notifyFromProto is the notify target of a request's trigger; an unknown kind stays empty so that
// validation refuses it.
func notifyFromProto(p *templatev1alpha1.NotifyTarget) *templates.NotifyTarget {
	if p == nil {
		return nil
	}
	n := &templates.NotifyTarget{User: p.GetUserId(), Channel: p.GetChannelId()}
	// An unspecified or unknown role stays empty, so that validation refuses it.
	if role, ok := auth.RoleFromProto(p.GetRole()); ok {
		n.Role = role
	}
	for k, v := range notifyKindToProto {
		if v == p.GetKind() {
			n.Kind = k
		}
	}
	return n
}

// triggersFromProto converts and validates a request's triggers.
func triggersFromProto(ps []*templatev1alpha1.Trigger) ([]templates.Trigger, error) {
	out := make([]templates.Trigger, 0, len(ps))
	for _, p := range ps {
		var typ templates.TriggerType
		for k, v := range triggerTypeToProto {
			if v == p.GetType() {
				typ = k
			}
		}
		out = append(out, templates.Trigger{
			ID: p.GetId(), Type: typ,
			Filter:      templates.TriggerFilter{Cron: p.GetCron(), Timezone: p.GetTimezone()},
			Instruction: p.GetInstruction(), Notify: notifyFromProto(p.GetNotify()),
		})
	}
	if err := templates.ValidateTriggers(out); err != nil {
		return nil, err
	}
	return out, nil
}
