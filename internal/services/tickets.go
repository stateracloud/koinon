// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/stateracloud/koinon/internal/db/store"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
)

// TicketService is the deprecated compatibility layer over tasks (ADR 0065, plan item TK1): the
// same rows, seen as tickets with a board column. The web UI uses it until the inbox (TK2) moves it
// to TaskService. The column is derived from the task state:
//
//	submitted -> To do, working -> In progress, input_required -> Waiting on you,
//	completed, canceled, failed -> Done
//
// and moving a ticket to a column sets the task state (Done sets completed). The timeline keeps
// the ticket's vocabulary: "task.created" is "ticket.created" and "status.changed" names columns.

// ticketColumn maps a task state to its board column.
var ticketColumn = map[store.TaskState]ticketv1alpha1.TicketStatus{
	store.TaskStateSubmitted:     ticketv1alpha1.TicketStatus_TICKET_STATUS_TODO,
	store.TaskStateWorking:       ticketv1alpha1.TicketStatus_TICKET_STATUS_IN_PROGRESS,
	store.TaskStateInputRequired: ticketv1alpha1.TicketStatus_TICKET_STATUS_WAITING_ON_YOU,
	store.TaskStateCompleted:     ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE,
	store.TaskStateCanceled:      ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE,
	store.TaskStateFailed:        ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE,
}

// ticketColumnName is the column as the timeline's status.changed payload names it.
var ticketColumnName = map[store.TaskState]string{
	store.TaskStateSubmitted:     "todo",
	store.TaskStateWorking:       "in_progress",
	store.TaskStateInputRequired: "waiting_on_you",
	store.TaskStateCompleted:     "done",
	store.TaskStateCanceled:      "done",
	store.TaskStateFailed:        "done",
}

// columnStates are the task states shown in a column.
func columnStates(col ticketv1alpha1.TicketStatus) []store.TaskState {
	var out []store.TaskState
	for _, st := range []store.TaskState{
		store.TaskStateSubmitted, store.TaskStateWorking, store.TaskStateInputRequired,
		store.TaskStateCompleted, store.TaskStateCanceled, store.TaskStateFailed,
	} {
		if ticketColumn[st] == col {
			out = append(out, st)
		}
	}
	return out
}

// columnState is the state a move to a column sets.
func columnState(col ticketv1alpha1.TicketStatus) (store.TaskState, bool) {
	states := columnStates(col)
	if len(states) == 0 {
		return "", false
	}
	return states[0], true // Done: completed, listed before canceled and failed
}

func ticketProto(t store.Task) *ticketv1alpha1.Ticket {
	return &ticketv1alpha1.Ticket{
		Id:           idString(t.ID),
		OrgId:        idString(t.OrgID),
		Title:        t.Title,
		Status:       ticketColumn[t.State],
		AgentId:      idString(t.AgentID),
		CreatedBy:    idString(t.CreatedByUserID),
		LastEventSeq: t.LastEventSeq,
		CreatedAt:    tsProto(t.CreatedAt),
		UpdatedAt:    tsProto(t.UpdatedAt),
		DoneAt:       tsProto(t.FinishedAt),
	}
}

var ticketActorKind = map[store.TaskActorKind]ticketv1alpha1.ActorKind{
	store.TaskActorKindUser:   ticketv1alpha1.ActorKind_ACTOR_KIND_USER,
	store.TaskActorKindAgent:  ticketv1alpha1.ActorKind_ACTOR_KIND_AGENT,
	store.TaskActorKindSystem: ticketv1alpha1.ActorKind_ACTOR_KIND_SYSTEM,
}

func ticketEventProto(e store.TaskEvent) *ticketv1alpha1.TicketEvent {
	kind, payload := e.Kind, payloadProto(e.Payload)
	switch kind {
	case KindTaskCreated:
		kind = "ticket.created"
	case KindStatusChanged:
		payload = columnPayload(payload)
	}
	return &ticketv1alpha1.TicketEvent{
		Seq:          e.Seq,
		Kind:         kind,
		ActorKind:    ticketActorKind[e.ActorKind],
		ActorUserId:  idString(e.ActorUserID),
		ActorAgentId: idString(e.ActorAgentID),
		RunId:        idString(e.RunID),
		Payload:      payload,
		CreatedAt:    tsProto(e.CreatedAt),
	}
}

// columnPayload renames the states of a status.changed payload to board columns.
func columnPayload(p *structpb.Struct) *structpb.Struct {
	if p == nil {
		return p
	}
	out := proto.Clone(p).(*structpb.Struct)
	for _, k := range []string{"from", "to"} {
		if v, ok := out.GetFields()[k]; ok {
			if col, ok := ticketColumnName[store.TaskState(v.GetStringValue())]; ok {
				out.Fields[k] = structpb.NewStringValue(col)
			}
		}
	}
	return out
}

func setTicketCost(t *ticketv1alpha1.Ticket, costs map[string]*runv1alpha1.Cost) {
	t.Cost = costOrNone(costs, t.GetId())
}

// CreateTicket implements TicketService.
// Compatibility layer: see the file comment.
func (s *Server) CreateTicket(ctx context.Context, req *ticketv1alpha1.CreateTicketRequest) (*ticketv1alpha1.CreateTicketResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	agentID, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.GetTitle())
	if title == "" || utf8.RuneCountInString(title) > 500 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "title must be 1 to 500 characters")
	}
	msg, err := checkMessage(req.GetMessage(), false)
	if err != nil {
		return nil, err
	}
	t, run, err := s.createTask(ctx, c, agentID, title, msg)
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &ticketv1alpha1.CreateTicketResponse{Ticket: ticketProto(t), Run: run}, nil
}

// ListTickets implements TicketService.
// Compatibility layer: see the file comment.
func (s *Server) ListTickets(ctx context.Context, req *ticketv1alpha1.ListTicketsRequest) (*ticketv1alpha1.ListTicketsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	var states []store.TaskState
	if req.GetStatus() != ticketv1alpha1.TicketStatus_TICKET_STATUS_UNSPECIFIED {
		if states = columnStates(req.GetStatus()); len(states) == 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, "unknown status")
		}
	}
	tasks, costs, err := s.listTasks(ctx, c, states)
	if err != nil {
		return nil, s.dbError(err, "tickets")
	}
	res := &ticketv1alpha1.ListTicketsResponse{}
	for _, t := range tasks {
		p := ticketProto(t)
		setTicketCost(p, costs)
		res.Tickets = append(res.Tickets, p)
	}
	return res, nil
}

// GetTicket implements TicketService.
// Compatibility layer: see the file comment.
func (s *Server) GetTicket(ctx context.Context, req *ticketv1alpha1.GetTicketRequest) (*ticketv1alpha1.GetTicketResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("ticket_id", req.GetTicketId())
	if err != nil {
		return nil, err
	}
	v, err := s.loadTask(ctx, c, id)
	if err != nil {
		return nil, s.dbError(err, "ticket")
	}
	res := &ticketv1alpha1.GetTicketResponse{Ticket: ticketProto(v.task), Runs: v.runs}
	res.Ticket.Cost = v.cost
	if res.Ticket.Cost == nil {
		res.Ticket.Cost = noCost()
	}
	res.Ticket.AgentSummary = v.task.AgentSummary.String
	res.Ticket.AgentSummaryAt = tsProto(v.task.AgentSummaryAt)
	for _, e := range v.events {
		res.Events = append(res.Events, ticketEventProto(e))
	}
	return res, nil
}

// AppendMessage implements TicketService.
// Compatibility layer: see the file comment.
func (s *Server) AppendMessage(ctx context.Context, req *ticketv1alpha1.AppendMessageRequest) (*ticketv1alpha1.AppendMessageResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("ticket_id", req.GetTicketId())
	if err != nil {
		return nil, err
	}
	msg, err := checkMessage(req.GetText(), true)
	if err != nil {
		return nil, err
	}
	ev, run, err := s.appendMessage(ctx, c, id, msg)
	if errors.Is(err, errTaskEnded) {
		err = connect.NewError(connect.CodeFailedPrecondition, "ticket is done; move it back to a board column first")
	}
	if err != nil {
		return nil, s.dbError(err, "ticket")
	}
	return &ticketv1alpha1.AppendMessageResponse{Event: ticketEventProto(ev), Run: run}, nil
}

// MoveTicket implements TicketService. Moving to Done completes the task and cancels its queued
// runs, each with a timeline event.
// People do not move tasks; TaskService has CancelTask.
func (s *Server) MoveTicket(ctx context.Context, req *ticketv1alpha1.MoveTicketRequest) (*ticketv1alpha1.MoveTicketResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("ticket_id", req.GetTicketId())
	if err != nil {
		return nil, err
	}
	to, ok := columnState(req.GetStatus())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, "status is required")
	}
	t, runs, err := s.moveToColumn(ctx, c, id, req.GetStatus(), to)
	if err != nil {
		return nil, s.dbError(err, "ticket")
	}
	return &ticketv1alpha1.MoveTicketResponse{Ticket: ticketProto(t), CanceledRuns: runsProto(runs)}, nil
}

// moveToColumn is setState, except that a task already shown in the column stays in its state (a
// failed task moved to Done stays failed).
func (s *Server) moveToColumn(ctx context.Context, c caller, id pgtype.UUID, col ticketv1alpha1.TicketStatus, to store.TaskState) (store.Task, []store.Run, error) {
	cur, err := s.q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: id})
	if err != nil {
		return store.Task{}, nil, err
	}
	if ticketColumn[cur.State] == col {
		to = cur.State
	}
	return s.setState(ctx, c, id, to)
}
