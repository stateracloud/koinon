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

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Actor kinds.
const (
	KindUser   = "user"
	KindToken  = "token"
	KindAgent  = "agent"
	KindSecret = "secret"
	KindSystem = "system"
)

// Actor is who made a change.
type Actor struct {
	Kind string
	// ID is the user, token or agent ID, or the declaring Secret's name.
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

// System is Koinon itself.
func System() Actor { return Actor{Kind: KindSystem} }

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

// Record writes e with q, which belongs to the transaction of the change being audited.
func Record(ctx context.Context, q *store.Queries, e Event) (store.AuditEvent, error) {
	summary := map[string]any{}
	if e.Before != nil {
		summary["before"] = e.Before
	}
	if e.After != nil {
		summary["after"] = e.After
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		return store.AuditEvent{}, fmt.Errorf("audit: summary: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return store.AuditEvent{}, err
	}
	if err := scrub(generic); err != nil {
		return store.AuditEvent{}, err
	}
	if e.RequestID == "" {
		e.RequestID = RequestID(ctx)
	}
	row, err := q.InsertAuditEvent(ctx, store.InsertAuditEventParams{
		OrgID: e.Org, ActorKind: e.Actor.Kind, ActorID: e.Actor.ID, Action: e.Action,
		TargetKind: e.TargetKind, TargetID: e.TargetID, Summary: raw, RequestID: e.RequestID,
	})
	if err != nil {
		return store.AuditEvent{}, fmt.Errorf("audit: record %s: %w", e.Action, err)
	}
	return row, nil
}

// RecordErr is Record for callers that only need the error.
func RecordErr(ctx context.Context, q *store.Queries, e Event) error {
	_, err := Record(ctx, q, e)
	return err
}
