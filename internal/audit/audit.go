// SPDX-License-Identifier: Apache-2.0

// Package audit writes the append-only audit log (ADR 0023, plan item PV1).
//
// Events are written by the shared service layer (packages catalog, tokens and
// connections' callers), so a change made in the UI over connect-rpc, through the
// admin API with a token, or by a declarative Secret is audited the same way and
// in the same transaction as the change itself.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
)

// Actor kinds.
const (
	KindUser   = "user"
	KindToken  = "token"
	KindAgent  = "agent"
	KindSecret = "secret"
	KindSystem = "system"
	// KindCLI is an operator's koinon-api command run in the pod (koinon-api keys forget); its ID
	// names the command.
	KindCLI = "cli"
)

// Actor is who made a change.
type Actor struct {
	Kind string
	// ID is the user, token or agent ID, the declaring Secret's name, or the command a CLI actor ran.
	ID string
}

// User is a signed-in user.
func User(id pgtype.UUID) Actor { return Actor{Kind: KindUser, ID: id.String()} }

// Token is an admin API token.
func Token(id pgtype.UUID) Actor { return Actor{Kind: KindToken, ID: id.String()} }

// Agent is an agent.
func Agent(id pgtype.UUID) Actor { return Actor{Kind: KindAgent, ID: id.String()} }

// Secret is a declarative Kubernetes Secret ("secret <name>" in the log).
func Secret(name string) Actor { return Actor{Kind: KindSecret, ID: name} }

// System is Koinon itself, named by the component and function that acted, as
// "<component>:<function>" ("koinon-api:price-refresh", ADR 0100 decision 2). The ID is required:
// no caller can write an unnamed system actor, and the install chain refuses an empty one.
func System(id string) Actor {
	if id == "" {
		panic("audit: a system actor needs an ID, <component>:<function>")
	}
	return Actor{Kind: KindSystem, ID: id}
}

// CLI is an operator's koinon-api command, named by command ("keys forget"). The command runs in
// the pod with the database's credentials, so there is no signed-in person to name.
func CLI(command string) Actor { return Actor{Kind: KindCLI, ID: command} }

// String renders the actor as it reads in the log.
func (a Actor) String() string {
	if a.ID == "" {
		return a.Kind
	}
	return a.Kind + " " + a.ID
}

// Event is one audit entry. Before and After describe the target without secret values;
// Record refuses a summary with a field that looks like one.
type Event struct {
	Org        pgtype.UUID
	Actor      Actor
	Action     string
	TargetKind string
	TargetID   string
	Before     any
	After      any
	RequestID  string
	// Reason is the reason a person gave, when the action takes one; it lands in the summary as "reason".
	Reason string
	// KoinonVersion is the build that wrote the entry. Record fills it from the build; callers leave
	// it empty. It lands in the summary as "koinon_version".
	KoinonVersion string
	// InstallEventID is the install event that caused this org event (ADR 0100 decision 1.6); it
	// lands in the summary as "install_event_id".
	InstallEventID string
}

// InstallEvent is one entry of the install chain, an action that belongs to no org (ADR 0100).
// Before and After describe the target without secret values; RecordInstall refuses a summary with a
// field that looks like one.
type InstallEvent struct {
	Actor      Actor
	Action     string
	TargetKind string
	TargetID   string
	Before     any
	After      any
	RequestID  string
	// Reason is the reason a person gave, when the action takes one ("reason" in the summary).
	Reason string
	// KoinonVersion is filled by RecordInstall from the build; callers leave it empty.
	KoinonVersion string
}

type requestIDKey struct{}

// WithRequestID returns ctx carrying the request ID the service layer stamps on events.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request ID in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// ErrSecretInSummary is returned for a summary that has a field named like a secret value.
var ErrSecretInSummary = errors.New("audit: summary has a field that looks like a secret value")

// secretNames are field names (lower case, no separators) a summary must not carry.
var secretNames = []string{"apikey", "privatekey", "password", "secret", "token", "credential", "authorization"}

// allowed are field names that contain a secret word but are not values.
var allowed = map[string]bool{
	"haskey": true, "keyfingerprint": true, "tokenid": true, "parenttokenid": true, "revokedtokenids": true, "tokenprefix": true, "secretname": true, "managedbysecret": true,
	// MCP servers (plan item MC1): whether a credential is set, the name of its header, that it changed.
	"hascredential": true, "credentialheader": true, "credentialchanged": true,
	// Trace destinations (ADR 0074): the names of the secret fields that are set, never their values.
	"secretfields": true,
}

func looksSecret(name string) bool {
	n := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
	if allowed[n] {
		return false
	}
	for _, s := range secretNames {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

func scrub(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if looksSecret(k) {
				return fmt.Errorf("%w: %q", ErrSecretInSummary, k)
			}
			if err := scrub(x); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range t {
			if err := scrub(x); err != nil {
				return err
			}
		}
	}
	return nil
}

// buildSummary renders the summary of an event: before and after, and the keys of ADR 0100
// decision 1.2a. It goes through the secret scrubber like any other summary.
func buildSummary(before, after any, reason, version, installEventID string) ([]byte, error) {
	summary := map[string]any{}
	if before != nil {
		summary["before"] = before
	}
	if after != nil {
		summary["after"] = after
	}
	if reason != "" {
		summary["reason"] = reason
	}
	if version == "" {
		version = project.Version
	}
	summary["koinon_version"] = version
	if installEventID != "" {
		summary["install_event_id"] = installEventID
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		return nil, fmt.Errorf("audit: summary: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := scrub(generic); err != nil {
		return nil, err
	}
	return raw, nil
}

// Record writes e with q, which belongs to the transaction of the change being audited. It writes
// at once, so the caller keeps the lock order of ADR 0100 decision 1.7 by hand; new code adds its
// events to an audit Batch (InTx) instead, and the module writes them in order. Record goes away
// when the last caller has moved (#371).
func Record(ctx context.Context, q *store.Queries, e Event) (store.AuditEvent, error) {
	return record(ctx, q, e)
}

func record(ctx context.Context, q *store.Queries, e Event) (store.AuditEvent, error) {
	raw, err := buildSummary(e.Before, e.After, e.Reason, e.KoinonVersion, e.InstallEventID)
	if err != nil {
		return store.AuditEvent{}, err
	}
	if e.RequestID == "" {
		e.RequestID = RequestID(ctx)
	}
	row, err := q.InsertAuditEvent(ctx, store.InsertAuditEventParams{
		OrgID: e.Org, ActorKind: e.Actor.Kind, ActorID: e.Actor.ID, Action: e.Action,
		TargetKind: e.TargetKind, TargetID: e.TargetID, Summary: raw, RequestID: e.RequestID,
	})
	countWrite(ChainOrg, err)
	if err != nil {
		return store.AuditEvent{}, fmt.Errorf("audit: record %s: %w", e.Action, err)
	}
	return row, nil
}

// RecordAfter records an event after the work it describes, outside the transaction of the change
// (the reconciler's events, the admin API's call log, failed token checks). It is the one entry point
// for an event that is not atomic with its change; the name says so. It must not run inside a
// transaction that holds an audit head or that will write a Batch (ADR 0100 decision 1.7).
func RecordAfter(ctx context.Context, q *store.Queries, e Event) error {
	_, err := record(ctx, q, e)
	return err
}

// RecordErr is Record for callers that only need the error.
func RecordErr(ctx context.Context, q *store.Queries, e Event) error {
	_, err := Record(ctx, q, e)
	return err
}

// RecordInstall writes e to the install chain with q, which belongs to the transaction of the change
// being audited (ADR 0100 decision 1.2). Only koinon-api may call it: the database refuses the insert
// to any other role. The actor must be named (the chain refuses an empty ID). Lock order (decision
// 1.7): write the install event before the org events of the same transaction, never after.
func RecordInstall(ctx context.Context, q *store.Queries, e InstallEvent) (store.InstallAuditEvent, error) {
	return recordInstall(ctx, q, e)
}

func recordInstall(ctx context.Context, q *store.Queries, e InstallEvent) (store.InstallAuditEvent, error) {
	if e.Actor.Kind == "" || e.Actor.ID == "" {
		return store.InstallAuditEvent{}, errors.New("audit: an install event needs a named actor")
	}
	raw, err := buildSummary(e.Before, e.After, e.Reason, e.KoinonVersion, "")
	if err != nil {
		return store.InstallAuditEvent{}, err
	}
	if e.RequestID == "" {
		e.RequestID = RequestID(ctx)
	}
	row, err := q.InsertInstallAuditEvent(ctx, store.InsertInstallAuditEventParams{
		ActorKind: e.Actor.Kind, ActorID: e.Actor.ID, Action: e.Action,
		TargetKind: e.TargetKind, TargetID: e.TargetID, Summary: raw, RequestID: e.RequestID,
	})
	countWrite(ChainInstall, err)
	if err != nil {
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "42501" { // insufficient_privilege
			return store.InstallAuditEvent{}, fmt.Errorf("audit: record install event %s: %w: %w", e.Action, ErrInstallWriteDenied, err)
		}
		return store.InstallAuditEvent{}, fmt.Errorf("audit: record install event %s: %w", e.Action, err)
	}
	return row, nil
}

// ErrInstallWriteDenied is in the error RecordInstall returns when the database refuses the insert
// for lack of privilege: the connecting role is not (yet) a member of the install audit writer role,
// or "koinon-api migrate --grants-only" has not yet granted that role its privileges (ADR 0100
// decision 1.1a). On an upgrade of a CloudNativePG cluster this is the state between the new pods
// and the chart's post-upgrade grant hook, so startup writes retry instead of failing the start.
var ErrInstallWriteDenied = errors.New("the install audit writer role may not insert yet")

// RecordInstallErr is RecordInstall for callers that only need the error.
func RecordInstallErr(ctx context.Context, q *store.Queries, e InstallEvent) error {
	_, err := RecordInstall(ctx, q, e)
	return err
}
