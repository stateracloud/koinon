// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"

	"github.com/procoduck/koinon/internal/db/store"
	runv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/run/v1alpha1"
	"github.com/procoduck/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/procoduck/koinon/internal/scheduler"
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
	return &runv1alpha1.GetRunResponse{Run: runProto(r)}, nil
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
		out, err = scheduler.Cancel(ctx, q, c.org, id, scheduler.ReasonUser, reason, scheduler.Actor{UserID: c.actor()})
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
// event is sent once because the cursor only moves forward. The stream ends
// once the run has finished and its last event has been sent.
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
	for {
		events, err := s.q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{
			OrgID: c.org, RunID: id, AfterSeq: cursor, MaxEvents: watchBatch,
		})
		if err != nil {
			return s.dbError(err, "run events")
		}
		for _, e := range events {
			if err := stream.Send(&runv1alpha1.WatchRunResponse{
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
		if run.State != state {
			state = run.State
			if err := stream.Send(&runv1alpha1.WatchRunResponse{Item: &runv1alpha1.WatchRunResponse_Run{Run: runProto(run)}}); err != nil {
				return err
			}
		}
		if cursor < run.LastEventSeq {
			continue
		}
		if run.State.Finished() {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-sub.C:
		case <-poll.C:
		}
	}
}
