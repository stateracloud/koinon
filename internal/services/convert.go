// SPDX-License-Identifier: Apache-2.0

package services

import (
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/procoduck/koinon/internal/db/store"
	runv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/run/v1alpha1"
	templatev1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/template/v1alpha1"
	ticketv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/procoduck/koinon/templates"
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

var flavorsToProto = map[store.AgentFlavor]templatev1alpha1.Flavor{
	store.AgentFlavorOpencode: templatev1alpha1.Flavor_FLAVOR_OPENCODE,
	store.AgentFlavorHermes:   templatev1alpha1.Flavor_FLAVOR_HERMES,
	store.AgentFlavorPi:       templatev1alpha1.Flavor_FLAVOR_PI,
}

func flavorProto(f string) templatev1alpha1.Flavor { return flavorsToProto[store.AgentFlavor(f)] }

func flavorFromProto(f templatev1alpha1.Flavor) (store.AgentFlavor, bool) {
	for k, v := range flavorsToProto {
		if v == f {
			return k, true
		}
	}
	return "", false
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

var ticketStatusToProto = map[store.TicketStatus]ticketv1alpha1.TicketStatus{
	store.TicketStatusTodo:         ticketv1alpha1.TicketStatus_TICKET_STATUS_TODO,
	store.TicketStatusInProgress:   ticketv1alpha1.TicketStatus_TICKET_STATUS_IN_PROGRESS,
	store.TicketStatusWaitingOnYou: ticketv1alpha1.TicketStatus_TICKET_STATUS_WAITING_ON_YOU,
	store.TicketStatusDone:         ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE,
}

func ticketStatusFromProto(s ticketv1alpha1.TicketStatus) (store.TicketStatus, bool) {
	for k, v := range ticketStatusToProto {
		if v == s {
			return k, true
		}
	}
	return "", false
}

var actorKindToProto = map[store.TicketActorKind]ticketv1alpha1.ActorKind{
	store.TicketActorKindUser:   ticketv1alpha1.ActorKind_ACTOR_KIND_USER,
	store.TicketActorKindAgent:  ticketv1alpha1.ActorKind_ACTOR_KIND_AGENT,
	store.TicketActorKindSystem: ticketv1alpha1.ActorKind_ACTOR_KIND_SYSTEM,
}

func ticketProto(t store.Ticket) *ticketv1alpha1.Ticket {
	return &ticketv1alpha1.Ticket{
		Id:           idString(t.ID),
		OrgId:        idString(t.OrgID),
		Title:        t.Title,
		Status:       ticketStatusToProto[t.Status],
		AgentId:      idString(t.AgentID),
		CreatedBy:    idString(t.CreatedBy),
		LastEventSeq: t.LastEventSeq,
		CreatedAt:    tsProto(t.CreatedAt),
		UpdatedAt:    tsProto(t.UpdatedAt),
		DoneAt:       tsProto(t.DoneAt),
	}
}

func ticketEventProto(e store.TicketEvent) *ticketv1alpha1.TicketEvent {
	return &ticketv1alpha1.TicketEvent{
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
		TicketId:            idString(r.TicketID),
		AgentId:             idString(r.AgentID),
		AgentVersionId:      idString(r.AgentVersionID),
		State:               runStateToProto[r.State],
		OutcomeReason:       r.OutcomeReason.String,
		MaxWallClockSeconds: int64(r.MaxWallClockSeconds),
		MaxTokens:           r.MaxTokens,
		TokensUsed:          r.TokensUsed,
		LastEventSeq:        r.LastEventSeq,
		CreatedAt:           tsProto(r.CreatedAt),
		StartedAt:           tsProto(r.StartedAt),
		FinishedAt:          tsProto(r.FinishedAt),
	}
	if r.ParkReason.Valid {
		out.ParkReason = parkReasonToProto[r.ParkReason.RunParkReason]
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
	}
}
