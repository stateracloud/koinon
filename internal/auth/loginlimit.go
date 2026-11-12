// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

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

// loginKeys are the limiter slots of one login attempt: the client address with the username, checked
// before the password hash runs (with the address and group levels the limiter adds to it); the
// address's shared slot, which is decided after it (as the admin API's shared slot is), so a neighbour
// behind the same ingress who keeps failing does not lock out a user who knows their password; and
// the account, over all addresses.
type loginKeys struct {
	addr, user, shared, account string
	// device is the limiter key of the request's verified device cookie, or "".
	device string
}

// The account level counts failures against one username from every address, so guesses spread over
// many addresses are still slowed down (OWASP: the counter belongs to the account, not only to the
// source). It is a trade-off between guessing and denial of service. An attacker who fails 100 times
// in a window against a name makes that name wait, and by failing once after each wait (capped at
// accountMaxBackoff) keeps it waiting: the lockout is durable for any client without a device cookie.
// The owner is not locked out: a login with a valid device cookie for that name (devicecookie.go) skips
// this level, and its failures count against the cookie instead. A cookie is only set by a successful
// login, so an owner who never logged in from this browser, or lost the cookie, waits.
const (
	accountFreeFails  = 100
	accountMaxBackoff = time.Minute
)

func newLoginKeys(r *http.Request, username string) loginKeys {
	addr := ratelimit.ClientAddr(r)
	name := auditedUsername(username)
	return loginKeys{
		addr: addr, user: ratelimit.Key(addr, "user:"+name), shared: ratelimit.Key(addr, ""),
		account: ratelimit.Key("account", strings.ToLower(name)),
	}
}

// deviceUsable reports whether the request's device cookie still earns the account exemption: it
// verified, and the limiter does not refuse it for failing too often.
func (s *Service) deviceUsable(k loginKeys, now time.Time) bool {
	return k.device != "" && s.limiter.BlockedKey(k.device, now) == 0
}

// loginWait is how long a login must wait before the password hash: the longest wait over the
// username at its address, the address and its group, and the account unless a device cookie exempts it.
func (s *Service) loginWait(k loginKeys, now time.Time) time.Duration {
	if s.deviceUsable(k, now) {
		return s.limiter.BlockedTrusted(k.user, now)
	}
	return max(s.limiter.Blocked(k.user, now), s.limiter.BlockedKey(k.account, now))
}

// loginEvents counts the outcomes of the password login limiter. The labels are fixed (ADR 0023):
// never a user, an address or an org.
var loginEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "koinon_login_limiter_total",
	Help: "Password login failures recorded and requests refused by the limiter.",
}, []string{"event"})

const (
	loginEventFailed  = "failed"
	loginEventRefused = "refused"
)

// RegisterMetrics registers koinon_login_limiter_total on reg (nil: none) with every series at zero.
func RegisterMetrics(reg prometheus.Registerer) {
	for _, e := range []string{loginEventFailed, loginEventRefused} {
		loginEvents.WithLabelValues(e)
	}
	if reg != nil {
		reg.MustRegister(loginEvents)
	}
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
	loginEvents.WithLabelValues(loginEventFailed).Inc()
	wait = s.limiter.Blocked(k.shared, now)
	s.limiter.Fail(k.user, now)
	s.limiter.FailAfter(k.shared, ratelimit.FreeFails, now)
	if s.deviceUsable(k, now) {
		s.limiter.FailAfter(k.device, deviceFreeFails, now)
	} else {
		s.limiter.FailCapped(k.account, accountFreeFails, accountMaxBackoff, now)
	}
	if wait > 0 {
		s.limiter.Refuse(k.user)
		loginEvents.WithLabelValues(loginEventRefused).Inc()
		return true, wait
	}
	s.auditLoginFailure(ctx, k, username, s.limiter.TakeRefused(k.user)+s.limiter.TakeRefused(k.shared), nil)
	return false, 0
}

// auditLoginFailure writes the install-chain event of a failed login, with the failures counted but
// not written since the last event (refused). The extra fields are added to the event.
func (s *Service) auditLoginFailure(ctx context.Context, k loginKeys, username string, refused int, extra map[string]any) {
	after := map[string]any{"username": auditedUsername(username), "client_address": k.addr}
	if refused > 0 {
		after["refused_while_limited"] = refused
	}
	maps.Copy(after, extra)
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
}
