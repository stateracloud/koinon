// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Page sizes of List.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ErrBadFilter is returned for a filter or cursor List cannot use.
var ErrBadFilter = errors.New("audit: invalid filter")

var (
	actionPrefixRE = regexp.MustCompile(`^[a-z0-9_.-]{1,100}$`)
	actorKinds     = map[string]bool{KindUser: true, KindToken: true, KindAgent: true, KindSecret: true, KindSystem: true, KindCLI: true}
)

// Filter narrows List. The zero value lists every event of the org.
type Filter struct {
	// Org scopes the listing. A filter without a valid Org is refused unless AllOrgs is set.
	Org pgtype.UUID
	// AllOrgs reads every org. Callers set it for platform admins only; it replaces Org.
	AllOrgs bool
	// Since and Until bound created_at: Since <= created_at < Until.
	Since, Until time.Time
	// ActionPrefix matches the start of the action, for example "approval." or "agent.create".
	ActionPrefix string
	// ActorKind is user, token, agent, secret, system or cli; ActorID narrows it to one actor.
	ActorKind, ActorID string
	// PageSize defaults to DefaultPageSize and is at most MaxPageSize.
	PageSize int
	// PageToken is the NextToken of the previous page.
	PageToken string
}

func textArg(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func timeArg(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()} }

// prepare validates the filters both reads share and returns the page size and the cursor.
func (f Filter) prepare() (size int, before pgtype.UUID, err error) {
	if f.ActionPrefix != "" && !actionPrefixRE.MatchString(f.ActionPrefix) {
		return 0, before, fmt.Errorf("%w: action prefix %q", ErrBadFilter, f.ActionPrefix)
	}
	if f.ActorKind != "" && !actorKinds[f.ActorKind] {
		return 0, before, fmt.Errorf("%w: actor kind %q", ErrBadFilter, f.ActorKind)
	}
	if f.ActorID != "" && f.ActorKind == "" {
		return 0, before, fmt.Errorf("%w: an actor ID needs an actor kind", ErrBadFilter)
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return 0, before, fmt.Errorf("%w: until is not after since", ErrBadFilter)
	}
	size = f.PageSize
	switch {
	case size < 0:
		return 0, before, fmt.Errorf("%w: negative page size", ErrBadFilter)
	case size == 0:
		size = DefaultPageSize
	case size > MaxPageSize:
		size = MaxPageSize
	}
	if f.PageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(f.PageToken)
		if err != nil || before.Scan(string(raw)) != nil || !before.Valid {
			return 0, before, fmt.Errorf("%w: page token", ErrBadFilter)
		}
	}
	return size, before, nil
}

// nextToken trims rows (fetched with one more than the page) and returns the token of the next page.
func nextToken[T any](rows []T, size int, id func(T) pgtype.UUID) ([]T, string) {
	if len(rows) <= size {
		return rows, ""
	}
	rows = rows[:size]
	return rows, base64.RawURLEncoding.EncodeToString([]byte(id(rows[size-1]).String()))
}

// List returns one page of events, newest first, and the token of the next page ("" at the end).
// Events are read by id; ids are UUIDv7, so that is creation order.
func List(ctx context.Context, q *store.Queries, f Filter) ([]store.AuditEvent, string, error) {
	if !f.AllOrgs && !f.Org.Valid {
		return nil, "", fmt.Errorf("%w: an org is required", ErrBadFilter)
	}
	if f.AllOrgs && f.Org.Valid {
		return nil, "", fmt.Errorf("%w: AllOrgs and an org together", ErrBadFilter)
	}
	size, before, err := f.prepare()
	if err != nil {
		return nil, "", err
	}
	var rows []store.AuditEvent
	// One more than the page, to know whether another page follows.
	limit := int32(size + 1)
	if !f.AllOrgs {
		rows, err = q.ListAuditEventsFiltered(ctx, store.ListAuditEventsFilteredParams{
			OrgID: f.Org, BeforeID: before, Since: timeArg(f.Since), Until: timeArg(f.Until),
			ActionPrefix: textArg(f.ActionPrefix), ActorKind: textArg(f.ActorKind), ActorID: textArg(f.ActorID), PageSize: limit,
		})
	} else {
		rows, err = q.ListAuditEventsAllOrgs(ctx, store.ListAuditEventsAllOrgsParams{
			BeforeID: before, Since: timeArg(f.Since), Until: timeArg(f.Until),
			ActionPrefix: textArg(f.ActionPrefix), ActorKind: textArg(f.ActorKind), ActorID: textArg(f.ActorID), PageSize: limit,
		})
	}
	if err != nil {
		return nil, "", fmt.Errorf("audit: list: %w", err)
	}
	rows, next := nextToken(rows, size, func(e store.AuditEvent) pgtype.UUID { return e.ID })
	return rows, next, nil
}

// ListInstall returns one page of the install chain, newest first, with the same filters as List.
// The caller must be a platform admin: the check is the service layer's, as for AllOrgs. Org and
// AllOrgs must not be set; the install chain has no org.
func ListInstall(ctx context.Context, q *store.Queries, f Filter) ([]store.InstallAuditEvent, string, error) {
	if f.Org.Valid || f.AllOrgs {
		return nil, "", fmt.Errorf("%w: the install chain has no org", ErrBadFilter)
	}
	size, before, err := f.prepare()
	if err != nil {
		return nil, "", err
	}
	rows, err := q.ListInstallAuditEventsFiltered(ctx, store.ListInstallAuditEventsFilteredParams{
		BeforeID: before, Since: timeArg(f.Since), Until: timeArg(f.Until),
		ActionPrefix: textArg(f.ActionPrefix), ActorKind: textArg(f.ActorKind), ActorID: textArg(f.ActorID), PageSize: int32(size + 1),
	})
	if err != nil {
		return nil, "", fmt.Errorf("audit: list install: %w", err)
	}
	rows, next := nextToken(rows, size, func(e store.InstallAuditEvent) pgtype.UUID { return e.ID })
	return rows, next, nil
}
