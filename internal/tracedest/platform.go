// SPDX-License-Identifier: Apache-2.0

package tracedest

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tracing/export"
)

// Platform is the platform's trace settings: its destination, if any, and the content ceiling.
type Platform struct {
	Destination *Destination
	Ceiling     string
}

// GetPlatform returns the platform destination and ceiling.
func (s *Service) GetPlatform(ctx context.Context) (Platform, error) {
	q := store.New(s.pool)
	st, err := q.GetPlatformTraceSettings(ctx)
	if err != nil {
		return Platform{}, err
	}
	out := Platform{Ceiling: string(st.ContentCeiling)}
	d, err := q.GetPlatformTraceDestination(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return Platform{}, err
	}
	v := toPlatform(d)
	out.Destination = &v
	return out, nil
}

// SetPlatformDestination tests, then saves, the platform destination. Audited in auditOrg, the
// platform admin's personal org: the log has no org-less events (ADR 0023).
func (s *Service) SetPlatformDestination(ctx context.Context, auditOrg pgtype.UUID, actor audit.Actor, in Input) (Destination, error) {
	q := store.New(s.pool)
	cur, err := q.GetPlatformTraceDestination(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, err
	}
	var existing map[string]string
	var before *Destination
	if err == nil {
		v := toPlatform(cur)
		before = &v
		if string(cur.Backend) == in.Backend {
			if existing, err = s.open(cur.Credentials, PlatformBinding()); err != nil {
				return Destination{}, err
			}
		}
	}
	p, err := s.prepare(ctx, in, existing, PlatformBinding())
	if err != nil {
		return Destination{}, err
	}
	outcome, at := s.test(ctx, p.dest, "", in.ResourceAttributes)
	if !outcome.OK {
		return Destination{}, &TestFailedError{Outcome: outcome}
	}
	testAt, testOK, testMsg := testColumns(outcome, at)
	var out Destination
	err = s.tx(ctx, func(q *store.Queries) error {
		row, err := q.UpsertPlatformTraceDestination(ctx, store.UpsertPlatformTraceDestinationParams{
			Backend: store.TraceBackend(p.dest.Preset), EndpointUrl: in.EndpointURL, Params: p.params, Credentials: p.envelope,
			SecretFields: p.fields, ResourceAttributes: p.attrs, Enabled: in.Enabled,
			LastTestAt: testAt, LastTestOk: testOK, LastTestMessage: testMsg, UpdatedBy: actor.String(),
		})
		if err != nil {
			return err
		}
		out = toPlatform(row)
		ev := audit.Event{
			Org: auditOrg, Actor: actor, Action: "trace.platform_destination.set", TargetKind: "trace_platform_destination",
			TargetID: "platform", After: auditView(out),
		}
		if before != nil {
			ev.Before = auditView(*before)
		}
		return audit.RecordErr(ctx, q, ev)
	})
	return out, err
}

// DeletePlatformDestination removes the platform destination.
func (s *Service) DeletePlatformDestination(ctx context.Context, auditOrg pgtype.UUID, actor audit.Actor) error {
	return s.tx(ctx, func(q *store.Queries) error {
		cur, err := q.GetPlatformTraceDestination(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := q.DeletePlatformTraceDestination(ctx); err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: auditOrg, Actor: actor, Action: "trace.platform_destination.delete", TargetKind: "trace_platform_destination",
			TargetID: "platform", Before: auditView(toPlatform(cur)),
		})
	})
}

// TestPlatform sends the platform destination a synthetic trace again and keeps the result.
func (s *Service) TestPlatform(ctx context.Context, auditOrg pgtype.UUID, actor audit.Actor) (Destination, export.Outcome, error) {
	cur, err := store.New(s.pool).GetPlatformTraceDestination(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, export.Outcome{}, ErrNotFound
	}
	if err != nil {
		return Destination{}, export.Outcome{}, err
	}
	secrets, err := s.open(cur.Credentials, PlatformBinding())
	if err != nil {
		return Destination{}, export.Outcome{}, err
	}
	d := export.Destination{Preset: export.Preset(cur.Backend), Endpoint: cur.EndpointUrl, Params: decodeMap(cur.Params), Secrets: secrets}
	if err := s.policy.Check(ctx, d.Endpoint); err != nil {
		return Destination{}, export.Outcome{}, err
	}
	outcome, at := s.test(ctx, d, "", decodeMap(cur.ResourceAttributes))
	testAt, testOK, testMsg := testColumns(outcome, at)
	var out Destination
	err = s.tx(ctx, func(q *store.Queries) error {
		row, err := q.UpsertPlatformTraceDestination(ctx, store.UpsertPlatformTraceDestinationParams{
			Backend: cur.Backend, EndpointUrl: cur.EndpointUrl, Params: cur.Params, Credentials: cur.Credentials,
			SecretFields: cur.SecretFields, ResourceAttributes: cur.ResourceAttributes, Enabled: cur.Enabled,
			LastTestAt: testAt, LastTestOk: testOK, LastTestMessage: testMsg, UpdatedBy: cur.UpdatedBy,
		})
		if err != nil {
			return err
		}
		out = toPlatform(row)
		return audit.RecordErr(ctx, q, audit.Event{
			Org: auditOrg, Actor: actor, Action: "trace.platform_destination.test", TargetKind: "trace_platform_destination",
			TargetID: "platform", After: map[string]any{"ok": outcome.OK, "status": outcome.Status, "rejectedSpans": outcome.Rejected},
		})
	})
	return out, outcome, err
}

// SetPlatformCeiling sets the highest content level an org may pick. Lowering it lowers every org
// above it in the same transaction, and each of those orgs gets an audit event of its own, so its
// admins see why the level changed. The change itself is audited in auditOrg. It returns how many
// orgs were lowered.
func (s *Service) SetPlatformCeiling(ctx context.Context, auditOrg pgtype.UUID, actor audit.Actor, ceiling string) (int, error) {
	if !ValidLevel(ceiling) {
		return 0, invalid("ceiling must be none, redacted or full")
	}
	lowered := 0
	err := s.tx(ctx, func(q *store.Queries) error {
		before, err := q.LockPlatformTraceSettings(ctx)
		if err != nil {
			return err
		}
		if _, err := q.SetPlatformTraceCeiling(ctx, store.SetPlatformTraceCeilingParams{
			ContentCeiling: store.TraceContentLevel(ceiling), UpdatedBy: actor.String(),
		}); err != nil {
			return err
		}
		rows, err := q.LowerOrgTraceLevels(ctx, store.LowerOrgTraceLevelsParams{Ceiling: store.TraceContentLevel(ceiling), UpdatedBy: actor.String()})
		if err != nil {
			return err
		}
		lowered = len(rows)
		for _, r := range rows {
			if err := audit.RecordErr(ctx, q, audit.Event{
				Org: r.OrgID, Actor: actor, Action: "trace.settings.lowered", TargetKind: "trace_settings", TargetID: r.OrgID.String(),
				Before: map[string]any{"contentLevel": string(r.OldLevel), "platformCopyLevel": string(r.OldCopyLevel)},
				After:  map[string]any{"contentLevel": ceiling, "platformCopyLevel": MinLevel(string(r.OldCopyLevel), ceiling), "ceiling": ceiling},
			}); err != nil {
				return err
			}
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: auditOrg, Actor: actor, Action: "trace.platform_ceiling.set", TargetKind: "trace_platform_ceiling", TargetID: "platform",
			Before: map[string]any{"ceiling": string(before.ContentCeiling)},
			After:  map[string]any{"ceiling": ceiling, "orgsLowered": lowered},
		})
	})
	return lowered, err
}
