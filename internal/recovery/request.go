// SPDX-License-Identifier: Apache-2.0

package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tokens"
)

// Actions a PlatformAdminRecovery request may ask for (a closed set: a new action is a new decision,
// ADR 0110 decision 6). adopt-restore is decided there and is built by issue #516.
const (
	ActionMintAdminToken = "mint-admin-token"
	ActionResetPassword  = "reset-password"
)

// Limits of a request (ADR 0110 decision 6). The admission policy enforces the same ones; the handler
// checks them again because it must not trust that the policy was in force for an old object.
const (
	DefaultRequestTTL = 15 * time.Minute
	MaxRequestTTL     = time.Hour
)

var nonceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// Request is a PlatformAdminRecovery object, parsed.
type Request struct {
	// Name and UID identify the object; Created is metadata.creationTimestamp, set by the API server.
	Name, UID string
	Created   time.Time
	Action    string
	// User is the local login name the action is for; empty means the bootstrap admin.
	User string
	// Org is the slug or ID of the org a minted token belongs to; empty means the default org.
	Org string
	// Requester and Groups are the Kubernetes identity the admission policy recorded at creation.
	Requester string
	Groups    []string
	Nonce     string
	// TTL is how long the request may be acted on, from Created; zero means DefaultRequestTTL.
	TTL                time.Duration
	RecipientPublicKey string
	// TokenTTL is the lifetime of a minted token; zero means MaxTokenTTL.
	TokenTTL time.Duration
}

// Outcome is what the handler decided. Done is true when the action ran (now or on an earlier pass of
// the same object); a refusal has Reason and no Sealed.
type Outcome struct {
	Done   bool
	Reason string
	// Sealed is the age-armored result for the requester.
	Sealed string
}

func refused(format string, a ...any) Outcome {
	return Outcome{Reason: fmt.Sprintf(format, a...)}
}

// Handler carries out requests against the database. It knows nothing of Kubernetes: the controller
// authenticates the object (policy in force, identity recorded by admission) and passes the parsed
// request.
type Handler struct {
	Pool   *pgxpool.Pool
	Tokens *tokens.Service
	// Now defaults to time.Now.
	Now func() time.Time
}

var errNonceUsed = errors.New("nonce already used")

// Expired reports whether the request's TTL, counted from the object's creation, has passed.
func (r Request) Expired(now time.Time) bool { return now.After(r.Created.Add(r.ttl())) }

func (r Request) ttl() time.Duration {
	if r.TTL == 0 {
		return DefaultRequestTTL
	}
	return r.TTL
}

// Handle validates the request and runs its action in one transaction: the nonce row, the action, the
// seal of the result and the install chain event recovery.<action> commit together or not at all, so a
// crash cannot leave an action without its nonce and a retry cannot act twice. A returned error is
// transient (the database); a refusal is an Outcome the controller writes to the object's status.
func (h *Handler) Handle(ctx context.Context, req Request) (Outcome, error) {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	if o, ok := validate(req, now); !ok {
		return o, nil
	}
	rcpt, err := ParseRecipient(req.RecipientPublicKey)
	if err != nil {
		return refused("%v", err), nil
	}
	q := store.New(h.Pool)
	// A retry of an object whose status write failed finds its own row and hands the stored result back.
	if o, found, err := h.storedOutcome(ctx, q, req); err != nil || found {
		return o, err
	}

	var sealed string
	_, err = audit.InTx(ctx, h.Pool, func(q *store.Queries, b audit.Batch) error {
		n, err := q.ClaimRecoveryNonce(ctx, store.ClaimRecoveryNonceParams{Nonce: req.Nonce, CrUid: req.UID, Action: req.Action, Requester: req.Requester})
		if err != nil {
			return err
		}
		if n == 0 {
			return errNonceUsed
		}
		user := req.User
		if user == "" {
			if user, err = q.FirstBootstrapAdminUsername(ctx); errors.Is(err, pgx.ErrNoRows) {
				return errors.New("no bootstrap platform admin to act for: name a user in spec.user")
			} else if err != nil {
				return err
			}
		}
		actor := Actor(req.Requester)
		after := map[string]any{"requester": req.Requester, "groups": req.Groups, "crName": req.Name, "crUID": req.UID, "nonce": req.Nonce, "user": user}
		var plain []byte
		switch req.Action {
		case ActionResetPassword:
			res, err := ResetPassword(ctx, q, b, actor, user, now)
			if err != nil {
				return err
			}
			after["revokedSessions"] = res.RevokedSessions
			plain, err = json.Marshal(map[string]any{"action": req.Action, "username": user, "password": res.Password, "changeRequired": true})
			if err != nil {
				return err
			}
		case ActionMintAdminToken:
			org := req.Org
			if org == "" {
				org = auth.DefaultOrgSlug
			}
			created, err := MintAdminToken(ctx, h.Tokens, q, b, actor, MintTokenParams{
				Username: user, Org: org, Name: "recovery-" + req.Nonce[:8], TTL: req.TokenTTL,
			})
			if err != nil {
				return err
			}
			after["tokenID"], after["org"], after["expiresAt"] = created.ID, org, created.ExpiresAt
			plain, err = json.Marshal(map[string]any{"action": req.Action, "username": user, "org": org, "token": created.Secret, "expiresAt": created.ExpiresAt})
			if err != nil {
				return err
			}
		}
		if sealed, err = Seal(rcpt, plain); err != nil {
			return err
		}
		if err := q.SetRecoveryResult(ctx, store.SetRecoveryResultParams{Nonce: req.Nonce, SealedResult: sealed}); err != nil {
			return err
		}
		b.Install(audit.InstallEvent{Actor: actor, Action: "recovery." + req.Action, TargetKind: "platform_admin_recovery", TargetID: req.UID, After: after})
		return nil
	})
	switch {
	case errors.Is(err, errNonceUsed):
		// Another replica won the race for this very object, or the nonce belongs to an older one.
		o, found, err := h.storedOutcome(ctx, store.New(h.Pool), req)
		if err != nil || found {
			return o, err
		}
		return refused("the nonce was used by an earlier request"), nil
	case errors.Is(err, ErrNoLocalUser), errors.Is(err, ErrNotPlatformAdmin), errors.Is(err, ErrNoOrg):
		return refused("%v", err), nil
	case err != nil:
		return Outcome{}, err
	}
	return Outcome{Done: true, Sealed: sealed}, nil
}

// Actor is the audit actor of a request: the requester's Kubernetes identity (ADR 0110 decision 6).
func Actor(requester string) audit.Actor {
	return audit.Actor{Kind: audit.KindSystem, ID: "kubernetes:" + requester}
}

func validate(req Request, now time.Time) (Outcome, bool) {
	switch req.Action {
	case ActionMintAdminToken, ActionResetPassword:
	default:
		return refused("action %q is not supported", req.Action), false
	}
	switch {
	case req.Requester == "":
		return refused("spec.requester is empty"), false
	case !nonceRE.MatchString(req.Nonce):
		return refused("spec.nonce must be 16 to 64 characters of [A-Za-z0-9_-]"), false
	case req.RecipientPublicKey == "":
		return refused("spec.recipientPublicKey is required for %s", req.Action), false
	case req.TTL < 0 || req.TTL > MaxRequestTTL:
		return refused("spec.ttl must be at most %s", MaxRequestTTL), false
	case req.TokenTTL < 0 || req.TokenTTL > MaxTokenTTL:
		return refused("spec.tokenTTL must be at most %s", MaxTokenTTL), false
	case req.Expired(now):
		return refused("the request expired (ttl %s from creation)", req.ttl()), false
	}
	return Outcome{}, true
}

// storedOutcome returns the result of an earlier pass over the same object (same nonce, same UID).
func (h *Handler) storedOutcome(ctx context.Context, q *store.Queries, req Request) (Outcome, bool, error) {
	row, err := q.GetRecoveryRequest(ctx, req.Nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, err
	}
	if row.CrUid != req.UID {
		return refused("the nonce was used by an earlier request"), true, nil
	}
	return Outcome{Done: true, Sealed: row.SealedResult}, true, nil
}
