// SPDX-License-Identifier: Apache-2.0

package tracedest

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tracing/export"
)

// Settings are an org's trace settings with the platform ceiling they are held to.
type Settings struct {
	ContentLevel      string
	SampleRate        float64
	PlatformCopyLevel string
	Ceiling           string
	// HasPlatformDestination says the platform has an enabled destination, so PlatformCopyLevel matters.
	HasPlatformDestination bool
	UpdatedAt              *time.Time
	UpdatedBy              string
}

// SettingsInput is what an admin writes.
type SettingsInput struct {
	ContentLevel string
	// SampleRate nil keeps the stored rate (1 for an org that never set one).
	SampleRate        *float64
	PlatformCopyLevel string
}

func rate(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 1
	}
	return f.Float64
}

func numeric(f float64) pgtype.Numeric {
	var n pgtype.Numeric
	// Four decimals, the column's scale.
	_ = n.Scan(strconv.FormatFloat(f, 'f', 4, 64))
	return n
}

func (s *Service) settings(ctx context.Context, q *store.Queries, org pgtype.UUID) (Settings, error) {
	plat, err := q.GetPlatformTraceSettings(ctx)
	if err != nil {
		return Settings{}, err
	}
	out := Settings{ContentLevel: LevelNone, SampleRate: 1, PlatformCopyLevel: LevelNone, Ceiling: string(plat.ContentCeiling)}
	if d, err := q.GetPlatformTraceDestination(ctx); err == nil {
		out.HasPlatformDestination = d.Enabled
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, err
	}
	row, err := q.GetOrgTraceSettings(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return Settings{}, err
	}
	out.ContentLevel, out.SampleRate, out.PlatformCopyLevel = string(row.ContentLevel), rate(row.SampleRate), string(row.PlatformCopyLevel)
	out.UpdatedAt, out.UpdatedBy = optTime(row.UpdatedAt), row.UpdatedBy
	return out, nil
}

// GetSettings returns the org's settings; an org that never set any has the defaults: level none,
// every run, nothing extra to the platform.
func (s *Service) GetSettings(ctx context.Context, org pgtype.UUID) (Settings, error) {
	return s.settings(ctx, store.New(s.pool), org)
}

func settingsView(v Settings) map[string]any {
	return map[string]any{"contentLevel": v.ContentLevel, "sampleRate": v.SampleRate, "platformCopyLevel": v.PlatformCopyLevel}
}

// UpdateSettings sets the org's content level, sampling and platform opt-in. Raising the content
// level, or the platform copy, to full needs an org owner, and any raise of either needs a member of
// the org: a platform admin acting from outside may lower them but not raise them (who comes from
// real membership, not from the platform role). Neither level may exceed the platform ceiling, and the platform copy may not exceed the org's own level. Every change is audited.
func (s *Service) UpdateSettings(ctx context.Context, org pgtype.UUID, actor audit.Actor, who Authority, in SettingsInput) (Settings, error) {
	switch {
	case !ValidLevel(in.ContentLevel):
		return Settings{}, invalid("content level must be none, redacted or full")
	case !ValidLevel(in.PlatformCopyLevel):
		return Settings{}, invalid("platform copy level must be none, redacted or full")
	case in.SampleRate != nil && (*in.SampleRate < 0 || *in.SampleRate > 1 || *in.SampleRate != *in.SampleRate):
		return Settings{}, invalid("sample rate must be between 0 and 1")
	case LevelRank(in.PlatformCopyLevel) > LevelRank(in.ContentLevel):
		return Settings{}, invalid("the platform copy cannot carry more than the org's own content level")
	}
	var out Settings
	err := s.tx(ctx, func(q *store.Queries) error {
		// The ceiling row is share-locked so a lowering of the ceiling cannot pass this write, while
		// two org writes do not wait for each other.
		ceiling, err := q.ShareLockPlatformTraceSettings(ctx)
		if err != nil {
			return err
		}
		before, err := s.settings(ctx, q, org)
		if err != nil {
			return err
		}
		for _, lv := range []struct{ now, prev string }{{in.ContentLevel, before.ContentLevel}, {in.PlatformCopyLevel, before.PlatformCopyLevel}} {
			if LevelRank(lv.now) > LevelRank(lv.prev) && !who.Member {
				return ErrNotMember
			}
			if lv.now == LevelFull && lv.prev != LevelFull && !who.Owner {
				return ErrOwnerOnly
			}
			if LevelRank(lv.now) > LevelRank(string(ceiling.ContentCeiling)) {
				return ErrAboveCeiling
			}
		}
		sampleRate := before.SampleRate
		if in.SampleRate != nil {
			sampleRate = *in.SampleRate
		}
		if _, err := q.UpsertOrgTraceSettings(ctx, store.UpsertOrgTraceSettingsParams{
			OrgID: org, ContentLevel: store.TraceContentLevel(in.ContentLevel), SampleRate: numeric(sampleRate),
			PlatformCopyLevel: store.TraceContentLevel(in.PlatformCopyLevel), UpdatedBy: actor.String(),
		}); err != nil {
			return err
		}
		if out, err = s.settings(ctx, q, org); err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "trace.settings.update", TargetKind: "trace_settings", TargetID: org.String(),
			Before: settingsView(before), After: settingsView(out),
		})
	})
	return out, err
}

// DecideRun decides, once, whether a run is traced and stores it on the run (decision 1). A run is
// traced when the org has an enabled destination, or the platform has one, and the org's sampling
// picks it. A run decided before keeps its decision. agentd calls this when it claims the run.
func (s *Service) DecideRun(ctx context.Context, org, run pgtype.UUID) (bool, error) {
	q := store.New(s.pool)
	if traced, err := q.GetRunTraced(ctx, store.GetRunTracedParams{OrgID: org, ID: run}); err != nil {
		return false, err
	} else if traced.Valid {
		return traced.Bool, nil
	}
	decision, err := s.wouldTrace(ctx, q, org, rand.Float64()) //nolint:gosec // sampling, not security
	if err != nil {
		return false, err
	}
	return q.DecideRunTraced(ctx, store.DecideRunTracedParams{OrgID: org, ID: run, Traced: decision})
}

// wouldTrace is the decision for a draw in [0, 1).
func (s *Service) wouldTrace(ctx context.Context, q *store.Queries, org pgtype.UUID, draw float64) (bool, error) {
	st, err := s.settings(ctx, q, org)
	if err != nil {
		return false, err
	}
	n, err := q.CountEnabledTraceDestinations(ctx, org)
	if err != nil {
		return false, err
	}
	if n == 0 && !st.HasPlatformDestination {
		return false, nil
	}
	return draw < st.SampleRate, nil
}

// Route is one destination a span may go to, with what the router needs to send it.
type Route struct {
	// ID is empty for the platform destination.
	ID                 string
	Name               string
	Platform           bool
	Destination        export.Destination
	ResourceAttributes map[string]string
	// Level is the content level for this destination: the org's own level for an org destination,
	// and for the platform's the org's opt-in, never above the ceiling.
	Level string
}

// Routes returns the enabled destinations for org's traces, with their credentials opened: the org's
// own, at the org's level (held to the ceiling), and the platform's, at the org's opt-in. Credentials
// are in the returned values; callers must not log them.
func (s *Service) Routes(ctx context.Context, org pgtype.UUID) ([]Route, error) {
	q := store.New(s.pool)
	st, err := s.settings(ctx, q, org)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListTraceDestinations(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []Route
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		secrets, err := s.open(r.Credentials, Binding(org, r.ID))
		if err != nil {
			// One destination that cannot be opened must not stop the org's other traces: skip it, log
			// it and record the error on its health, where the admin sees it.
			s.log.Error("trace destination credentials cannot be opened; skipping it", "destination", r.ID.String(), "err", err)
			_ = q.RecordTraceDestinationError(ctx, store.RecordTraceDestinationErrorParams{
				OrgID: org, ID: r.ID, LastError: pgtype.Text{String: "the stored credentials cannot be opened", Valid: true},
			})
			continue
		}
		out = append(out, Route{
			ID: r.ID.String(), Name: r.Name,
			Destination:        export.Destination{Preset: export.Preset(r.Backend), Endpoint: r.EndpointUrl, Params: decodeMap(r.Params), Secrets: secrets},
			ResourceAttributes: decodeMap(r.ResourceAttributes), Level: MinLevel(st.ContentLevel, st.Ceiling),
		})
	}
	p, err := q.GetPlatformTraceDestination(ctx)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !p.Enabled {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	secrets, err := s.open(p.Credentials, PlatformBinding())
	if err != nil {
		s.log.Error("platform trace destination credentials cannot be opened; skipping it", "err", err)
		_ = q.RecordPlatformTraceDestinationError(ctx, store.RecordPlatformTraceDestinationErrorParams{
			LastError: pgtype.Text{String: "the stored credentials cannot be opened", Valid: true},
		})
		return out, nil
	}
	return append(out, Route{
		Name: "platform", Platform: true,
		Destination:        export.Destination{Preset: export.Preset(p.Backend), Endpoint: p.EndpointUrl, Params: decodeMap(p.Params), Secrets: secrets},
		ResourceAttributes: decodeMap(p.ResourceAttributes), Level: MinLevel(st.PlatformCopyLevel, MinLevel(st.ContentLevel, st.Ceiling)),
	}), nil
}
