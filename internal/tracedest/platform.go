// SPDX-License-Identifier: Apache-2.0

package tracedest

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"

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

// SetPlatformDestination saves the platform destination, testing it first when the change touches its
// connection (see Update). Stored secrets are kept only while backend and endpoint origin stay the
// same. Audited in the install audit chain (ADR 0100 decision 3), in the transaction of the change.
func (s *Service) SetPlatformDestination(ctx context.Context, actor audit.Actor, expectedVersion *int64, in Input) (Destination, error) {
	q := store.New(s.pool)
	cur, err := q.GetPlatformTraceDestination(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, err
	}
	exists := err == nil
	if expectedVersion != nil {
		have := int64(0)
		if exists {
			have = cur.ConfigVersion
		}
		if *expectedVersion != have {
			return Destination{}, ErrChanged
		}
	}
	endpoint, err := s.endpoint(in.EndpointURL)
	if err != nil {
		return Destination{}, err
	}
	var before *Destination
	touched := true
	if exists {
		v := toPlatform(cur)
		before = &v
		touched = string(cur.Backend) != in.Backend || cur.EndpointUrl != endpoint ||
			!maps.Equal(decodeMap(cur.Params), nonNil(in.Params)) || len(in.Secrets) > 0 || len(in.RemoveSecrets) > 0
	}
	var p prepared
	var outcome export.Outcome
	var at time.Time
	if touched {
		var existing map[string]string
		if exists && string(cur.Backend) == in.Backend && export.SameOrigin(cur.EndpointUrl, endpoint) {
			if existing, err = s.open(cur.Credentials, PlatformBinding()); err != nil {
				return Destination{}, err
			}
		}
		if p, err = s.prepare(ctx, in, existing, PlatformBinding()); err != nil {
			return Destination{}, err
		}
		if outcome, at = s.test(ctx, p.dest, "", in.ResourceAttributes); !outcome.OK {
			return Destination{}, &TestFailedError{Outcome: outcome}
		}
	} else {
		if err := validateAttributes(in.ResourceAttributes); err != nil {
			return Destination{}, err
		}
		p = prepared{endpoint: cur.EndpointUrl, envelope: cur.Credentials, fields: cur.SecretFields, params: cur.Params}
		p.dest.Preset = export.Preset(cur.Backend)
		if p.attrs, err = json.Marshal(nonNil(in.ResourceAttributes)); err != nil {
			return Destination{}, err
		}
	}
	testAt, testOK, testMsg := testColumns(outcome, at)
	var out Destination
	err = s.tx(ctx, func(q *store.Queries) error {
		// Only the version that was read (and tested against) is replaced.
		now, err := q.LockPlatformTraceDestination(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if exists {
				return ErrChanged
			}
		case err != nil:
			return err
		case !exists || now.ConfigVersion != cur.ConfigVersion:
			return ErrChanged
		}
		row, err := q.UpsertPlatformTraceDestination(ctx, store.UpsertPlatformTraceDestinationParams{
			Backend: store.TraceBackend(p.dest.Preset), EndpointUrl: p.endpoint, Params: p.params, Credentials: p.envelope,
			SecretFields: p.fields, ResourceAttributes: p.attrs, Enabled: in.Enabled, SetTest: touched,
			LastTestAt: testAt, LastTestOk: testOK, LastTestMessage: testMsg, UpdatedBy: actor.String(),
		})
		if err != nil {
			return err
		}
		out = toPlatform(row)
		ev := audit.InstallEvent{
			Actor: actor, Action: "trace.platform_destination.set", TargetKind: "trace_platform_destination",
			TargetID: "platform", After: auditView(out),
		}
		if before != nil {
			ev.Before = auditView(*before)
		}
		return audit.RecordInstallErr(ctx, q, ev)
	})
	return out, err
}

// DeletePlatformDestination removes the platform destination.
func (s *Service) DeletePlatformDestination(ctx context.Context, actor audit.Actor) error {
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
		return audit.RecordInstallErr(ctx, q, audit.InstallEvent{
			Actor: actor, Action: "trace.platform_destination.delete", TargetKind: "trace_platform_destination",
			TargetID: "platform", Before: auditView(toPlatform(cur)),
		})
	})
}

// TestPlatform sends the platform destination a synthetic trace again and keeps the result, only if
// the configuration is still the one that was tested: a destination deleted meanwhile is ErrNotFound
// and one changed meanwhile is ErrChanged; neither is written back.
func (s *Service) TestPlatform(ctx context.Context, actor audit.Actor) (Destination, export.Outcome, error) {
	cur, err := store.New(s.pool).GetPlatformTraceDestination(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, export.Outcome{}, ErrNotFound
	}
	if err != nil {
		return Destination{}, export.Outcome{}, err
	}
	if cur.CredentialLostAt.Valid {
		return Destination{}, export.Outcome{}, ErrCredentialLost
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
		n, err := q.SetPlatformTraceDestinationTest(ctx, store.SetPlatformTraceDestinationTestParams{
			ExpectedVersion: cur.ConfigVersion, LastTestAt: testAt, LastTestOk: testOK, LastTestMessage: testMsg,
		})
		if err != nil {
			return err
		}
		row, err := q.GetPlatformTraceDestination(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrChanged
		}
		out = toPlatform(row)
		return audit.RecordInstallErr(ctx, q, audit.InstallEvent{
			Actor: actor, Action: "trace.platform_destination.test", TargetKind: "trace_platform_destination",
			TargetID: "platform", After: map[string]any{"ok": outcome.OK, "status": outcome.Status, "rejectedSpans": outcome.Rejected},
		})
	})
	return out, outcome, err
}

// SetPlatformCeiling sets the highest content level an org may pick. Lowering it lowers every org
// above it in the same transaction. The change is one install event, written first (ADR 0100
// decision 1.7: domain locks, then the install event, then org events in org ID order); each lowered
// org then gets a trace.settings.lowered event of its own, pointing back with install_event_id, so its
// admins see why the level changed. It returns how many orgs were lowered.
func (s *Service) SetPlatformCeiling(ctx context.Context, actor audit.Actor, ceiling string) (int, error) {
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
		install, err := audit.RecordInstall(ctx, q, audit.InstallEvent{
			Actor: actor, Action: "trace.platform_ceiling.set", TargetKind: "trace_platform_ceiling", TargetID: "platform",
			Before: map[string]any{"ceiling": string(before.ContentCeiling)},
			After:  map[string]any{"ceiling": ceiling, "orgsLowered": lowered},
		})
		if err != nil {
			return err
		}
		for _, r := range rows { // LowerOrgTraceLevels returns them in org ID order
			if err := audit.RecordErr(ctx, q, audit.Event{
				Org: r.OrgID, Actor: actor, Action: "trace.settings.lowered", TargetKind: "trace_settings", TargetID: r.OrgID.String(),
				Before:         map[string]any{"contentLevel": string(r.OldLevel), "platformCopyLevel": string(r.OldCopyLevel)},
				After:          map[string]any{"contentLevel": ceiling, "platformCopyLevel": MinLevel(string(r.OldCopyLevel), ceiling), "ceiling": ceiling},
				InstallEventID: install.ID.String(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return lowered, err
}
