// SPDX-License-Identifier: Apache-2.0

// Package outward is the one admin switch for everything Koinon calls outside the install (ADR 0110
// decision 5, issue #516): MCP servers, model providers, code-host connections and trace
// destinations (the org's and the platform's). A disabled target is not called, not probed and not
// offered to runs; it keeps its bindings and its credential, so enabling it again restores it.
//
// Every target table carries the same three columns, admin_disabled_at, admin_disabled_by and
// admin_disabled_note. They are separate from the system states (disabled_reason, removed_at, enabled),
// so re-declaring or un-shadowing an item never clears an admin's disable.
//
// The callers of a target decide in SQL (the "served" queries test admin_disabled_at) or with Check on
// the row they hold; the service here only changes the state, audited, and syncs the declarative
// field. Disable is always allowed, also in restore-hold mode, because it reaches nothing. Enable is
// refused while the install is held (restorehold.ErrHeld), because an enabled target may be dialled.
//
// Prior art: Discourse's "disable emails" site setting that a restore turns on, and Vault's seal/step-down
// switches: one fail-safe off state per outward effect, set by an admin, separate from the item's own
// configuration (ADR 0110 Prior art).
package outward

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// Kind is a kind of outward target.
type Kind string

// The outward target kinds. Koinon has no outward notification target yet (notifications is the in-app
// inbox); a future sink (Slack, a webhook) adds its kind here from its first migration.
const (
	MCPServer                Kind = "mcp-server"
	ModelProvider            Kind = "model-provider"
	CodeHostConnection       Kind = "connection"
	TraceDestination         Kind = "trace-destination"
	PlatformTraceDestination Kind = "platform-trace-destination"
)

// Kinds is every kind, in the order a listing shows them.
var Kinds = []Kind{MCPServer, ModelProvider, CodeHostConnection, TraceDestination, PlatformTraceDestination}

// Audit actions of the switch: one pair for every kind, the kind is the event's target kind.
const (
	ActionDisable = "outward.disable"
	ActionEnable  = "outward.enable"
)

// DeclaredActorPrefix starts admin_disabled_by when the declaring Secret says disabled: true. The
// admin API cannot enable such a target: the Secret wins.
const DeclaredActorPrefix = audit.KindSecret + " "

// declaredNote is the note of a disable that comes from a Secret.
const declaredNote = "declared disabled: true"

var (
	// ErrNotFound: the target does not exist in the org.
	ErrNotFound = errors.New("outward target not found")
	// ErrDisabled is what a caller returns instead of calling a disabled target.
	ErrDisabled = errors.New("disabled by an admin (outward switch); enable it first")
	// ErrDeclared: the declaring Secret disables the target, so the API cannot enable it.
	ErrDeclared = errors.New("the declaring Secret sets disabled: true; remove that field from the Secret to enable it")
)

// InvalidError is a request the caller can fix.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// MaxNoteLen bounds the note.
const MaxNoteLen = 500

// table describes where a kind lives.
type table struct {
	name string
	// nameCol is the column an audit event names the target by; empty for the platform singleton.
	nameCol string
	// orgScoped is false for the platform singleton.
	orgScoped bool
}

var tables = map[Kind]table{
	MCPServer:                {"mcp_servers", "name", true},
	ModelProvider:            {"providers", "name", true},
	CodeHostConnection:       {"connections", "display_name", true},
	TraceDestination:         {"trace_destinations", "name", true},
	PlatformTraceDestination: {"platform_trace_destination", "", false},
}

// Valid says whether k is a known kind.
func (k Kind) Valid() bool { _, ok := tables[k]; return ok }

// Platform says whether k belongs to the install and not to an org.
func (k Kind) Platform() bool { return k == PlatformTraceDestination }

// State is the switch of one target.
type State struct {
	Disabled bool
	At       time.Time
	// By is the actor as the audit log reads it ("user <id>", "secret <name>").
	By   string
	Note string
	// Name is the target's name, for messages.
	Name string
}

// Declared says whether the declaring Secret holds the target disabled.
func (s State) Declared() bool { return s.Disabled && strings.HasPrefix(s.By, DeclaredActorPrefix) }

// Check is the guard a caller of a target runs before it dials, on the admin_disabled_at it already read
// with the row. It returns ErrDisabled when the target is disabled.
func Check(adminDisabledAt pgtype.Timestamptz) error {
	if adminDisabledAt.Valid {
		return ErrDisabled
	}
	return nil
}

// DB is what the service needs from the pool: transactions for audited changes, plain queries for reads.
type DB interface {
	audit.Beginner
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Service changes the switch.
type Service struct {
	db   DB
	hold *restorehold.Guard
}

// New returns a Service on db (a *pgxpool.Pool).
func New(db DB) *Service { return &Service{db: db} }

// WithRestoreHold makes Enable refuse with restorehold.ErrHeld while g holds (ADR 0110 decision 5).
// Disable stays allowed.
func (s *Service) WithRestoreHold(g *restorehold.Guard) *Service {
	s.hold = g
	return s
}

// where is the WHERE clause that selects the target, and its arguments.
func (t table) where(org, id pgtype.UUID) (string, []any) {
	if !t.orgScoped {
		return "singleton", nil
	}
	return "org_id = $1 AND id = $2", []any{org, id}
}

func (t table) nameExpr() string {
	if t.nameCol == "" {
		return "'platform'::text"
	}
	return t.nameCol + "::text"
}

func scanState(row pgx.Row) (State, error) {
	var (
		at       pgtype.Timestamptz
		by, note pgtype.Text
		name     string
	)
	if err := row.Scan(&at, &by, &note, &name); err != nil {
		return State{}, err
	}
	return State{Disabled: at.Valid, At: at.Time, By: by.String, Note: note.String, Name: name}, nil
}

// Get returns the switch of one target.
func (s *Service) Get(ctx context.Context, kind Kind, org, id pgtype.UUID) (State, error) {
	t, ok := tables[kind]
	if !ok {
		return State{}, &InvalidError{Msg: "unknown outward target kind"}
	}
	w, args := t.where(org, id)
	st, err := scanState(s.db.QueryRow(ctx,
		fmt.Sprintf(`SELECT admin_disabled_at, admin_disabled_by, admin_disabled_note, %s FROM %s WHERE %s`, t.nameExpr(), t.name, w), args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrNotFound
	}
	return st, err
}

// SetDisabled disables or enables one target and audits the change. org and id are ignored for the platform
// kind. Disabling an already disabled target (or enabling an enabled one) changes and audits nothing.
// Enabling is refused with restorehold.ErrHeld while the install is held, and with ErrDeclared when the
// declaring Secret holds the target disabled.
func (s *Service) SetDisabled(ctx context.Context, org pgtype.UUID, actor audit.Actor, kind Kind, id pgtype.UUID, disabled bool, note string) (State, error) {
	t, ok := tables[kind]
	if !ok {
		return State{}, &InvalidError{Msg: "unknown outward target kind"}
	}
	if !disabled {
		if err := s.hold.Err(); err != nil {
			return State{}, err
		}
	}
	note = strings.TrimSpace(note)
	if len(note) > MaxNoteLen {
		return State{}, &InvalidError{Msg: fmt.Sprintf("note is at most %d characters", MaxNoteLen)}
	}
	var out State
	_, err := audit.InPgTx(ctx, s.db, func(tx pgx.Tx, b audit.Batch) error {
		w, args := t.where(org, id)
		cur, err := scanState(tx.QueryRow(ctx,
			fmt.Sprintf(`SELECT admin_disabled_at, admin_disabled_by, admin_disabled_note, %s FROM %s WHERE %s FOR UPDATE`, t.nameExpr(), t.name, w), args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Disabled && disabled && cur.By == actor.String() && cur.Note != note {
			// The same owner disables again with a new note: the note changes, the first disable time stays.
			if _, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET admin_disabled_note = NULLIF($%d, '') WHERE %s`, t.name, len(args)+1, w), append(args, note)...); err != nil {
				return err
			}
			out = cur
			out.Note = note
			s.audit(b, org, actor, kind, id, cur, out)
			return nil
		}
		if cur.Disabled == disabled {
			out = cur
			return nil
		}
		if !disabled && cur.Declared() {
			return ErrDeclared
		}
		if disabled {
			_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET admin_disabled_at = now(), admin_disabled_by = $%d, admin_disabled_note = NULLIF($%d, '') WHERE %s`,
				t.name, len(args)+1, len(args)+2, w), append(args, actor.String(), note)...)
		} else {
			_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET admin_disabled_at = NULL, admin_disabled_by = NULL, admin_disabled_note = NULL, admin_disabled_under_by = NULL, admin_disabled_under_note = NULL WHERE %s`, t.name, w), args...)
		}
		if err != nil {
			return err
		}
		out = State{Disabled: disabled, At: time.Now(), Name: cur.Name}
		if disabled {
			out.By, out.Note = actor.String(), note
		}
		s.audit(b, org, actor, kind, id, cur, out)
		return notifyProvider(ctx, tx, kind, org)
	})
	if err != nil {
		return State{}, err
	}
	return out, nil
}

// audit adds the event of a change. The platform kind belongs to no org: its event is an install event.
func (s *Service) audit(b audit.Batch, org pgtype.UUID, actor audit.Actor, kind Kind, id pgtype.UUID, before, after State) {
	auditChange(b, org, actor, kind, id, before, after)
}

func auditChange(b audit.Batch, org pgtype.UUID, actor audit.Actor, kind Kind, id pgtype.UUID, before, after State) {
	action := ActionEnable
	if after.Disabled {
		action = ActionDisable
	}
	sum := func(st State) map[string]any {
		m := map[string]any{"disabled": st.Disabled, "name": st.Name}
		if st.Note != "" {
			m["note"] = st.Note
		}
		return m
	}
	if kind.Platform() {
		b.Install(audit.InstallEvent{Actor: actor, Action: action, TargetKind: string(kind), TargetID: "platform", Before: sum(before), After: sum(after)})
		return
	}
	b.Org(audit.Event{Org: org, Actor: actor, Action: action, TargetKind: string(kind), TargetID: id.String(), Before: sum(before), After: sum(after)})
}

// SyncDeclared applies the disabled field of a declaring Secret to what that Secret manages in org: when
// disabled is true every item it manages that is not disabled yet is disabled with actor "secret <name>"; when
// it is false, an item the same Secret disabled is enabled again (an admin's own disable stays). Enabling
// is skipped while the install is held. Each change is audited. It returns the number of items changed.
func (s *Service) SyncDeclared(ctx context.Context, org pgtype.UUID, kind Kind, secretName string, disabled bool) (int, error) {
	t, ok := tables[kind]
	if !ok || kind == TraceDestination || kind == PlatformTraceDestination {
		return 0, &InvalidError{Msg: "this kind has no declarative form"}
	}
	if !disabled && s.hold.Held() {
		return 0, nil
	}
	actor := audit.Secret(secretName)
	changed := 0
	_, err := audit.InPgTx(ctx, s.db, func(tx pgx.Tx, b audit.Batch) error {
		if disabled {
			n, err := DisableDeclared(ctx, tx, b, org, kind, secretName)
			changed = n
			return err // DisableDeclared wakes the reconciler
		}
		// An admin's disable that the Secret claimed comes back to the admin; the item stays disabled.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET admin_disabled_by = admin_disabled_under_by, admin_disabled_note = admin_disabled_under_note,
			admin_disabled_under_by = NULL, admin_disabled_under_note = NULL
			WHERE org_id = $1 AND managed_by_secret = $2 AND admin_disabled_by = $3 AND admin_disabled_under_by IS NOT NULL`, t.name), org, secretName, actor.String()); err != nil {
			return err
		}
		q := fmt.Sprintf(`UPDATE %[1]s SET admin_disabled_at = NULL, admin_disabled_by = NULL, admin_disabled_note = NULL
			WHERE org_id = $1 AND managed_by_secret = $2 AND admin_disabled_by = $3 RETURNING id, %[2]s::text`, t.name, t.nameExpr())
		rows, err := tx.Query(ctx, q, org, secretName, actor.String())
		if err != nil {
			return err
		}
		type hit struct {
			id   pgtype.UUID
			name string
		}
		var hits []hit
		for rows.Next() {
			var h hit
			if err := rows.Scan(&h.id, &h.name); err != nil {
				rows.Close()
				return err
			}
			hits = append(hits, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, h := range hits {
			s.audit(b, org, actor, kind, h.id, State{Disabled: true, Name: h.name, By: actor.String()}, State{Name: h.name})
		}
		changed = len(hits)
		return notifyProvider(ctx, tx, kind, org)
	})
	return changed, err
}

// DisableDeclared marks every item that secretName manages in org as disabled by that Secret, in the
// caller's transaction: the importer calls it in the transaction of the upsert, so a new item declared
// disabled is never visible enabled. An item an admin disabled earlier is claimed too (its actor becomes
// "secret <name>", so the API cannot enable it while the Secret says disabled; the first disable time
// stays). The admin's actor and note are kept underneath and come back when the Secret drops the field.
// Each change is audited. It returns the number of items changed.
func DisableDeclared(ctx context.Context, tx pgx.Tx, b audit.Batch, org pgtype.UUID, kind Kind, secretName string) (int, error) {
	t, ok := tables[kind]
	if !ok || kind == TraceDestination || kind == PlatformTraceDestination {
		return 0, &InvalidError{Msg: "this kind has no declarative form"}
	}
	actor := audit.Secret(secretName)
	q := fmt.Sprintf(`WITH old AS (
			SELECT id, admin_disabled_at, admin_disabled_by FROM %[1]s
			WHERE org_id = $1 AND managed_by_secret = $2 AND (admin_disabled_at IS NULL OR admin_disabled_by IS DISTINCT FROM $3) FOR UPDATE)
		UPDATE %[1]s AS t SET admin_disabled_at = COALESCE(t.admin_disabled_at, now()),
			admin_disabled_under_by = CASE WHEN t.admin_disabled_at IS NOT NULL AND t.admin_disabled_by NOT LIKE $5 THEN t.admin_disabled_by END,
			admin_disabled_under_note = CASE WHEN t.admin_disabled_at IS NOT NULL AND t.admin_disabled_by NOT LIKE $5 THEN t.admin_disabled_note END,
			admin_disabled_by = $3, admin_disabled_note = $4
		FROM old WHERE t.org_id = $1 AND t.id = old.id RETURNING t.id, %[2]s::text, old.admin_disabled_at IS NOT NULL`, t.name, t.nameExpr())
	rows, err := tx.Query(ctx, q, org, secretName, actor.String(), declaredNote, DeclaredActorPrefix+"%")
	if err != nil {
		return 0, err
	}
	type hit struct {
		id     pgtype.UUID
		name   string
		before bool
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.id, &h.name, &h.before); err != nil {
			rows.Close()
			return 0, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, h := range hits {
		auditChange(b, org, actor, kind, h.id, State{Disabled: h.before, Name: h.name}, State{Disabled: true, Name: h.name, Note: declaredNote, By: actor.String()})
	}
	if len(hits) > 0 {
		if err := notifyProvider(ctx, tx, kind, org); err != nil {
			return 0, err
		}
	}
	return len(hits), nil
}

// notifyProvider wakes the reconciler when a model provider changes: providers have no NOTIFY trigger, and
// the gateway plan skips a disabled provider, so without it the change waits for the next resync. The
// notification is sent when tx commits.
func notifyProvider(ctx context.Context, tx pgx.Tx, kind Kind, org pgtype.UUID) error {
	if kind != ModelProvider {
		return nil
	}
	_, err := tx.Exec(ctx, "SELECT pg_notify('koinon_reconcile', $1::uuid::text)", org)
	return err
}

// Target is one outward target in a listing.
type Target struct {
	Kind Kind
	Org  pgtype.UUID
	ID   pgtype.UUID
	State
}

// List returns the targets of org (and the platform destination when it exists), disabled or not, for the
// restore status view and the UI.
func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]Target, error) {
	var out []Target
	for _, kind := range Kinds {
		t := tables[kind]
		var (
			rows pgx.Rows
			err  error
		)
		if t.orgScoped {
			rows, err = s.db.Query(ctx, fmt.Sprintf(`SELECT id, admin_disabled_at, admin_disabled_by, admin_disabled_note, %s FROM %s WHERE org_id = $1 ORDER BY 5, id`, t.nameExpr(), t.name), org)
		} else {
			rows, err = s.db.Query(ctx, fmt.Sprintf(`SELECT '00000000-0000-0000-0000-000000000000'::uuid, admin_disabled_at, admin_disabled_by, admin_disabled_note, %s FROM %s WHERE singleton`, t.nameExpr(), t.name))
		}
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				id       pgtype.UUID
				at       pgtype.Timestamptz
				by, note pgtype.Text
				name     string
			)
			if err := rows.Scan(&id, &at, &by, &note, &name); err != nil {
				rows.Close()
				return nil, err
			}
			tg := Target{Kind: kind, ID: id, State: State{Disabled: at.Valid, At: at.Time, By: by.String, Note: note.String, Name: name}}
			if t.orgScoped {
				tg.Org = org
			}
			out = append(out, tg)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Disabled is the switch as a target's Get and List show it. A nil *Disabled is an enabled target.
type Disabled struct {
	At   time.Time
	By   string
	Note string
	// Declared is true when the declaring Secret holds the target disabled; the API cannot enable it.
	Declared bool
}

// FromRow reads the three admin_disabled_* columns of a row: nil when the target is enabled.
func FromRow(at pgtype.Timestamptz, by, note pgtype.Text) *Disabled {
	if !at.Valid {
		return nil
	}
	return &Disabled{At: at.Time, By: by.String, Note: note.String, Declared: strings.HasPrefix(by.String, DeclaredActorPrefix)}
}
