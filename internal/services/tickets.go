// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"

	"github.com/procoduck/koinon/internal/db/store"
	runv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/run/v1alpha1"
	ticketv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/procoduck/koinon/internal/scheduler"
)

// Ticket event kinds written by the API.
const (
	KindTicketCreated = "ticket.created"
	KindMessage       = "message"
	KindStatusChanged = "status.changed"
)

const (
	maxMessageBytes  = 64 << 10
	maxTimelineItems = 10_000
)

func checkMessage(text string, required bool) (string, error) {
	if strings.TrimSpace(text) == "" {
		if required {
			return "", connect.NewError(connect.CodeInvalidArgument, "text is required")
		}
		return "", nil
	}
	if len(text) > maxMessageBytes {
		return "", connect.NewError(connect.CodeInvalidArgument, "message is longer than 64 KiB")
	}
	if !utf8.ValidString(text) {
		return "", connect.NewError(connect.CodeInvalidArgument, "message is not valid UTF-8")
	}
	return text, nil
}

func jsonPayload(v map[string]any) []byte {
	b, _ := json.Marshal(v) // plain values only
	return b
}

// enqueue maps scheduler errors for the API.
func enqueue(ctx context.Context, q *store.Queries, t store.Ticket) (*runv1alpha1.Run, error) {
	run, queued, err := scheduler.Enqueue(ctx, q, t)
	if errors.Is(err, scheduler.ErrAgentNotActive) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the ticket's agent is not active")
	}
	if err != nil || !queued {
		return nil, err
	}
	return runProto(run), nil
}

// CreateTicket implements TicketService.
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

	res := &ticketv1alpha1.CreateTicketResponse{}
	err = s.inTx(ctx, func(q *store.Queries) error {
		agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: agentID})
		if err != nil {
			return err
		}
		if agent.Status != store.AgentStatusActive {
			return connect.NewError(connect.CodeFailedPrecondition, "agent is not active")
		}
		t, err := q.CreateTicket(ctx, store.CreateTicketParams{
			OrgID: c.org, Title: title, AgentID: agent.ID, CreatedBy: c.actor(),
		})
		if err != nil {
			return err
		}
		if _, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
			OrgID: c.org, TicketID: t.ID, Kind: KindTicketCreated,
			ActorKind: store.TicketActorKindUser, ActorUserID: c.actor(),
			Payload: jsonPayload(map[string]any{"title": title}),
		}); err != nil {
			return err
		}
		if msg != "" {
			if _, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
				OrgID: c.org, TicketID: t.ID, Kind: KindMessage,
				ActorKind: store.TicketActorKindUser, ActorUserID: c.actor(),
				Payload: jsonPayload(map[string]any{"text": msg}),
			}); err != nil {
				return err
			}
		}
		if res.Run, err = enqueue(ctx, q, t); err != nil {
			return err
		}
		t, err = q.GetTicket(ctx, store.GetTicketParams{OrgID: c.org, ID: t.ID})
		res.Ticket = ticketProto(t)
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return res, nil
}

// ListTickets implements TicketService.
func (s *Server) ListTickets(ctx context.Context, req *ticketv1alpha1.ListTicketsRequest) (*ticketv1alpha1.ListTicketsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	var tickets []store.Ticket
	if req.GetStatus() == ticketv1alpha1.TicketStatus_TICKET_STATUS_UNSPECIFIED {
		tickets, err = s.q.ListTickets(ctx, c.org)
	} else {
		status, ok := ticketStatusFromProto(req.GetStatus())
		if !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, "unknown status")
		}
		tickets, err = s.q.ListTicketsByStatus(ctx, store.ListTicketsByStatusParams{OrgID: c.org, Status: status})
	}
	if err != nil {
		return nil, s.dbError(err, "tickets")
	}
	res := &ticketv1alpha1.ListTicketsResponse{}
	for _, t := range tickets {
		res.Tickets = append(res.Tickets, ticketProto(t))
	}
	if err := setTicketCosts(ctx, s.q, c.org, res.Tickets); err != nil {
		return nil, s.dbError(err, "tickets")
	}
	return res, nil
}

// GetTicket implements TicketService.
func (s *Server) GetTicket(ctx context.Context, req *ticketv1alpha1.GetTicketRequest) (*ticketv1alpha1.GetTicketResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("ticket_id", req.GetTicketId())
	if err != nil {
		return nil, err
	}
	res := &ticketv1alpha1.GetTicketResponse{}
	// One snapshot, so the timeline and the runs agree.
	err = s.inSnapshot(ctx, func(q *store.Queries) error {
		t, err := q.GetTicket(ctx, store.GetTicketParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		res.Ticket = ticketProto(t)
		events, err := q.ListTicketEvents(ctx, store.ListTicketEventsParams{
			OrgID: c.org, TicketID: id, AfterSeq: 0, MaxEvents: maxTimelineItems,
		})
		if err != nil {
			return err
		}
		for _, e := range events {
			res.Events = append(res.Events, ticketEventProto(e))
		}
		runs, err := q.ListRunsForTicket(ctx, store.ListRunsForTicketParams{OrgID: c.org, TicketID: id})
		if err != nil {
			return err
		}
		res.Runs = runsProto(runs)
		if err := setRunCosts(ctx, q, c.org, res.Runs); err != nil {
			return err
		}
		return setTicketCosts(ctx, q, c.org, []*ticketv1alpha1.Ticket{res.Ticket})
	})
	if err != nil {
		return nil, s.dbError(err, "ticket")
	}
	return res, nil
}

// AppendMessage implements TicketService.
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
	res := &ticketv1alpha1.AppendMessageResponse{}
	err = s.inTx(ctx, func(q *store.Queries) error {
		t, err := q.GetTicket(ctx, store.GetTicketParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if t.Status == store.TicketStatusDone {
			return connect.NewError(connect.CodeFailedPrecondition, "ticket is done; move it back to a board column first")
		}
		// Appending locks the ticket row until commit, which serialises
		// concurrent messages and so the queued-run check in Enqueue.
		ev, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
			OrgID: c.org, TicketID: t.ID, Kind: KindMessage,
			ActorKind: store.TicketActorKindUser, ActorUserID: c.actor(),
			Payload: jsonPayload(map[string]any{"text": msg}),
		})
		if err != nil {
			return err
		}
		res.Event = ticketEventProto(ev)
		res.Run, err = enqueue(ctx, q, t)
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "ticket")
	}
	return res, nil
}

// MoveTicket implements TicketService. Moving to Done cancels the ticket's
// queued runs, each with a ticket event.
func (s *Server) MoveTicket(ctx context.Context, req *ticketv1alpha1.MoveTicketRequest) (*ticketv1alpha1.MoveTicketResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("ticket_id", req.GetTicketId())
	if err != nil {
		return nil, err
	}
	to, ok := ticketStatusFromProto(req.GetStatus())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, "status is required")
	}
	res := &ticketv1alpha1.MoveTicketResponse{}
	err = s.inTx(ctx, func(q *store.Queries) error {
		t, err := q.GetTicket(ctx, store.GetTicketParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if from := t.Status; from != to {
			if t, err = q.MoveTicket(ctx, store.MoveTicketParams{OrgID: c.org, ID: id, Status: to}); err != nil {
				return err
			}
			if _, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
				OrgID: c.org, TicketID: id, Kind: KindStatusChanged,
				ActorKind: store.TicketActorKindUser, ActorUserID: c.actor(),
				Payload: jsonPayload(map[string]any{"from": string(from), "to": string(to)}),
			}); err != nil {
				return err
			}
		}
		if to == store.TicketStatusDone {
			runs, err := scheduler.CancelQueuedForTicket(ctx, q, t, scheduler.Actor{UserID: c.actor()})
			if err != nil {
				return err
			}
			res.CanceledRuns = runsProto(runs)
		}
		t, err = q.GetTicket(ctx, store.GetTicketParams{OrgID: c.org, ID: id})
		res.Ticket = ticketProto(t)
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "ticket")
	}
	return res, nil
}
