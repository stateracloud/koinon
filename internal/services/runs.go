// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/db/store"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/runwatch"
)

// watchBatch is how many events WatchRun reads per query.
const watchBatch = 500

// GetRun implements RunService.
func (s *Server) GetRun(ctx context.Context, req *runv1alpha1.GetRunRequest) (*runv1alpha1.GetRunResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("run_id", req.GetRunId())
	if err != nil {
		return nil, err
	}
	r, err := s.q.GetRun(ctx, store.GetRunParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "run")
	}
	out := runProto(r)
	if err := setRunCosts(ctx, s.q, c.org, []*runv1alpha1.Run{out}); err != nil {
		return nil, s.dbError(err, "run")
	}
	return &runv1alpha1.GetRunResponse{Run: out}, nil
}

// CancelRun implements RunService.
func (s *Server) CancelRun(ctx context.Context, req *runv1alpha1.CancelRunRequest) (*runv1alpha1.CancelRunResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("run_id", req.GetRunId())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.GetReason())
	if len(reason) > 1000 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "reason is longer than 1000 bytes")
	}
	if reason == "" {
		who := c.p.DisplayName
		if who == "" {
			who = c.p.Email
		}
		reason = "canceled by " + who
	}
	var out store.Run
	err = s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetRun(ctx, store.GetRunParams{OrgID: c.org, ID: id}); err != nil {
			return err
		}
		out, err = runoutcome.CancelUnowned(ctx, q, c.org, id, runoutcome.Cancellation{
			Reason: runoutcome.ReasonUser, Message: reason, Actor: runoutcome.Actor{UserID: c.actor()},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return connect.NewError(connect.CodeFailedPrecondition, "run has already finished")
		}
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "run")
	}
	return &runv1alpha1.CancelRunResponse{Run: runProto(out)}, nil
}

// WatchRun implements RunService: replay after after_seq, then follow.
//
// The stream subscribes to the run's wake-ups before its first read, then
// loops: send every stored event with seq above its cursor, send a run
// snapshot if the state changed, and wait for a wake-up or the poll
// interval. Events get their seq under the run's row lock, so they commit in
// seq order and "seq > cursor" never skips one that commits later; each
// event is sent once because the cursor only moves forward. A snapshot is
// sent only once the cursor has reached the run's last_event_seq, so it never
// precedes an event it already includes, and the last snapshot of a finished
// run follows its last event (the web client relies on this to tell a finished
// run from a cut stream). A canceled run is the exception: its worker may still store the item it
// was typing after the cancel (ADR 0074 item 8), so a canceled run that was claimed keeps its stream
// open until finished_at + cancelGrace (at once for an old canceled run), sends such late events and then a second final snapshot. The stream ends once the run has finished and its
// last event and final snapshot have been sent. A stream that has sent nothing for
// the heartbeat interval sends a heartbeat message (old issue 141). Live text, the chunks of the
// message or thought the agent is typing (runwatch/chunks.go), is forwarded as it arrives; it is not
// stored, not replayed and not in seq order (old issue 344).
func (s *Server) WatchRun(ctx context.Context, req *runv1alpha1.WatchRunRequest, stream runv1alpha1connect.RunServiceWatchRunServerStream) error {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return err
	}
	id, err := parseID("run_id", req.GetRunId())
	if err != nil {
		return err
	}
	if req.GetAfterSeq() < 0 {
		return connect.NewError(connect.CodeInvalidArgument, "after_seq must not be negative")
	}

	sub := s.hub.Subscribe(c.org, id)
	defer sub.Close()

	run, err := s.q.GetRun(ctx, store.GetRunParams{OrgID: c.org, ID: id})
	if err != nil {
		return s.dbError(err, "run")
	}
	if err := stream.Send(&runv1alpha1.WatchRunResponse{Item: &runv1alpha1.WatchRunResponse_Run{Run: runProto(run)}}); err != nil {
		return err
	}
	cursor, state := req.GetAfterSeq(), run.State

	poll := time.NewTicker(s.poll)
	defer poll.Stop()
	// The heartbeat timer restarts with every message sent, so only a quiet stream gets one.
	beat := time.NewTimer(s.heartbeat)
	defer beat.Stop()
	sent := func(resp *runv1alpha1.WatchRunResponse) error {
		if !beat.Stop() {
			select {
			case <-beat.C:
			default:
			}
		}
		beat.Reset(s.heartbeat)
		return stream.Send(resp)
	}
	// drain sends the live batches the Hub already holds. It runs before every read of the stored
	// events, so a batch handed over before an event was stored usually precedes that event. That is
	// an optimisation only: a batch can still arrive after its item's event (the Hub hands over
	// notifications and the store is read on separate paths). Every chunk names its item and the
	// stored event carries the same name, so the client drops a late tail (old issue 344).
	drain := func() error {
		for {
			select {
			case cs := <-sub.Chunks:
				if err := sent(liveProto(cs)); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}
	// A canceled run whose worker had claimed it may still store the item it was typing (ADR 0074),
	// after the run is terminal. Its stream stays open for the grace and sends the late events and
	// a second final snapshot.
	var graceEnd <-chan time.Time
	graceOver := false
	for {
		if err := drain(); err != nil {
			return err
		}
		events, err := s.q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{
			OrgID: c.org, RunID: id, AfterSeq: cursor, MaxEvents: watchBatch,
		})
		if err != nil {
			return s.dbError(err, "run events")
		}
		for _, e := range events {
			if err := sent(&runv1alpha1.WatchRunResponse{
				Item: &runv1alpha1.WatchRunResponse_Event{Event: runEventProto(e)},
			}); err != nil {
				return err
			}
			cursor = e.Seq
		}
		if len(events) == watchBatch {
			continue
		}
		// Read the run after the events: if it finished, every event it
		// will ever have is committed and counted in last_event_seq.
		run, err := s.q.GetRun(ctx, store.GetRunParams{OrgID: c.org, ID: id})
		if err != nil {
			return s.dbError(err, "run")
		}
		if cursor < run.LastEventSeq {
			// The run moved on after the events were read. Send the events
			// first: a snapshot sent now would precede events it already
			// includes, and the final one must follow the last event (old issue 140).
			continue
		}
		// A finished run gets another snapshot when events arrived after the one it had.
		if run.State != state || (len(events) > 0 && state.Finished()) {
			state = run.State
			if err := sent(&runv1alpha1.WatchRunResponse{Item: &runv1alpha1.WatchRunResponse_Run{Run: runProto(run)}}); err != nil {
				return err
			}
		}
		if run.State.Finished() {
			if run.State != store.RunStateCanceled || run.Epoch == 0 || s.cancelGrace <= 0 || graceOver {
				return nil
			}
			if graceEnd == nil {
				// The grace counts from the cancel, not from this stream: a stream opened on an old
				// canceled run has no late event to wait for and ends at once.
				left := s.cancelGrace
				if run.FinishedAt.Valid {
					left = time.Until(run.FinishedAt.Time.Add(s.cancelGrace))
				}
				if left <= 0 {
					return nil
				}
				t := time.NewTimer(left)
				defer t.Stop()
				graceEnd = t.C
			}
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-sub.C:
				break wait
			case <-poll.C:
				break wait
			case <-graceEnd:
				graceOver = true
				break wait
			case cs := <-sub.Chunks:
				// Live text (old issue 344): forwarded as it comes, without re-reading the events.
				if err := sent(liveProto(cs)); err != nil {
					return err
				}
			case <-beat.C:
				if err := sent(&runv1alpha1.WatchRunResponse{Item: &runv1alpha1.WatchRunResponse_Heartbeat{Heartbeat: &runv1alpha1.Heartbeat{}}}); err != nil {
					return err
				}
			}
		}
	}
}

// liveProto is a batch of live chunks as a WatchRun message.
func liveProto(cs []runwatch.Chunk) *runv1alpha1.WatchRunResponse {
	out := make([]*runv1alpha1.LiveChunk, len(cs))
	for i, c := range cs {
		out[i] = &runv1alpha1.LiveChunk{Kind: c.Kind, Text: c.Text, Item: c.Item}
	}
	return &runv1alpha1.WatchRunResponse{Item: &runv1alpha1.WatchRunResponse_Live{Live: &runv1alpha1.LiveChunks{Chunks: out}}}
}
