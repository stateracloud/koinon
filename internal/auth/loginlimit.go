// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth/ratelimit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// PasswordLoginActor writes the audit events of failed password logins: the client is not signed in,
// so the component that refused it is the actor.
var PasswordLoginActor = audit.System("koinon-api:password-login")

// ActionLoginFailed is the install-chain action of a failed password login. A failed login belongs
// to no org: the username may name nobody.
const ActionLoginFailed = "auth.login_failed"

// maxAuditedUsername bounds the username kept in a key and an audit event, in runes.
const maxAuditedUsername = 128

// loginKeys are the limiter slots of one login attempt: the client address with the username, which
// is checked before the password hash runs, and the address's shared slot, which is decided after it
// (as the admin API's shared slot is), so a neighbour behind the same ingress who keeps failing does
// not lock out a user who knows their password.
type loginKeys struct {
	addr, user, shared string
}

func newLoginKeys(r *http.Request, username string) loginKeys {
	addr := ratelimit.ClientAddr(r)
	return loginKeys{addr: addr, user: ratelimit.Key(addr, "user:"+auditedUsername(username)), shared: ratelimit.Key(addr, "")}
}

// auditedUsername is the username as a key and an audit event keep it: valid UTF-8 without NUL (which
// a jsonb summary cannot hold), at most maxAuditedUsername runes.
func auditedUsername(u string) string {
	u = strings.ToValidUTF8(u, "�")
	u = strings.ReplaceAll(u, "\x00", "�")
	if utf8.RuneCountInString(u) > maxAuditedUsername {
		u = string([]rune(u)[:maxAuditedUsername])
	}
	return u
}

// writeLoginLimited answers 429 to a client the limiter refuses, with how long it must wait.
func writeLoginLimited(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
	writeJSON(w, http.StatusTooManyRequests, errorResponse{"too many failed login attempts; retry later"})
}

// loginFailed records a failed login in the limiter, and in the install chain unless the client's
// address is already over the limit; then the failure is only counted, and the next event written
// for these keys carries the count. It reports whether the client is over the limit.
func (s *Service) loginFailed(ctx context.Context, k loginKeys, username string, now time.Time) (limited bool, wait time.Duration) {
	wait = s.limiter.Blocked(k.shared, now)
	s.limiter.Fail(k.user, now)
	s.limiter.Fail(k.shared, now)
	if wait > 0 {
		s.limiter.Refuse(k.shared)
		return true, wait
	}
	refused := s.limiter.TakeRefused(k.user) + s.limiter.TakeRefused(k.shared)
	after := map[string]any{"username": auditedUsername(username), "client_address": k.addr}
	if refused > 0 {
		after["refused_while_limited"] = refused
	}
	// The client may have gone; the failure is recorded all the same.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := audit.InTx(actx, s.pool, func(_ *store.Queries, b audit.Batch) error {
		b.Install(audit.InstallEvent{
			Actor: PasswordLoginActor, Action: ActionLoginFailed, TargetKind: "login", TargetID: auditedUsername(username),
			After: after, RequestID: audit.RequestID(ctx),
		})
		return nil
	}); err != nil {
		s.log.Error("could not audit a failed password login", "err", err)
	}
	return false, 0
}
