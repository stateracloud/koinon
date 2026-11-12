// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth/ratelimit"
	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// The password is fake and obviously so.
const fakeLimitPassword = "koinon_test_not-a-real-password-limit"

type loginEvent struct {
	Username, Address string
	Refused           int
}

// These specs skip without Docker (pgtest requires it); CI runs them.
var _ = Describe("password login limiter (issue 658)", func() {
	var (
		pool *pgxpool.Pool
		svc  *Service
		now  atomic.Pointer[time.Time]
	)

	BeforeEach(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		pw := filepath.Join(GinkgoT().TempDir(), "password")
		Expect(os.WriteFile(pw, []byte(fakeLimitPassword+"\n"), 0o600)).To(Succeed())
		_, err := Bootstrap(ctx, pool, BootstrapConfig{Username: "admin", PasswordFile: pw, LoginEnabled: true}, time.Now())
		Expect(err).NotTo(HaveOccurred())
		t0 := time.Now()
		now.Store(&t0)
		svc, err = New(pool, Config{BootstrapLogin: true, PublicURL: "http://koinon.test", Now: func() time.Time { return *now.Load() }})
		Expect(err).NotTo(HaveOccurred())
	})

	advance := func(d time.Duration) {
		t := now.Load().Add(d)
		now.Store(&t)
	}

	login := func(ctx context.Context, addr, username, password string) *httptest.ResponseRecorder {
		GinkgoHelper()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/auth/login",
			strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = addr + ":40000"
		rec := httptest.NewRecorder()
		svc.handleLogin(rec, req)
		return rec
	}

	events := func(ctx context.Context) []loginEvent {
		GinkgoHelper()
		rows, err := pool.Query(ctx, `SELECT summary->'after'->>'username', summary->'after'->>'client_address',
			COALESCE((summary->'after'->>'refused_while_limited')::int, 0)
			FROM install_audit_events WHERE action = $1 ORDER BY chain_seq`, ActionLoginFailed)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var out []loginEvent
		for rows.Next() {
			var e loginEvent
			Expect(rows.Scan(&e.Username, &e.Address, &e.Refused)).To(Succeed())
			out = append(out, e)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return out
	}

	// fillVerifySlots takes every password-check slot, so a request that reaches the hash answers
	// errBusy's 429 ("retry shortly") instead of the limiter's.
	fillVerifySlots := func() {
		for range cap(svc.verifySlots) {
			svc.verifySlots <- struct{}{}
		}
		DeferCleanup(func() {
			for range cap(svc.verifySlots) {
				<-svc.verifySlots
			}
		})
	}

	failUntilBlocked := func(ctx context.Context, addr, username string) {
		GinkgoHelper()
		for i := range ratelimit.FreeFails {
			Expect(login(ctx, addr, username, "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized), "failure %d", i+1)
		}
	}

	It("answers 429 with Retry-After for an address and username over the limit, before the hash runs", func(ctx SpecContext) {
		failUntilBlocked(ctx, "192.0.2.10", "admin")
		fillVerifySlots()

		rec := login(ctx, "192.0.2.10", "admin", fakeLimitPassword)
		Expect(rec.Code).To(Equal(http.StatusTooManyRequests))
		Expect(rec.Header().Get("Retry-After")).To(Equal("1"))
		Expect(rec.Body.String()).To(ContainSubstring("too many failed login attempts"), "the limiter answered, not the busy hash: %s", rec.Body.String())
		Expect(rec.Result().Cookies()).To(BeEmpty(), "even the right password is refused while blocked")
	})

	It("keeps the key per address and username", func(ctx SpecContext) {
		failUntilBlocked(ctx, "192.0.2.11", "admin")
		Expect(login(ctx, "192.0.2.12", "admin", fakeLimitPassword).Code).To(Equal(http.StatusOK), "another address")
	})

	It("lets a user who knows the password in although a neighbour keeps failing on the address", func(ctx SpecContext) {
		for i := range ratelimit.FreeFails {
			Expect(login(ctx, "192.0.2.13", "nobody-"+string(rune('a'+i)), "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized))
		}
		Expect(login(ctx, "192.0.2.13", "someone-else", "koinon_test_wrong").Code).To(Equal(http.StatusTooManyRequests), "the address is over the limit")
		Expect(login(ctx, "192.0.2.13", "admin", fakeLimitPassword).Code).To(Equal(http.StatusOK))
	})

	It("forgets the failures of a username after a good login", func(ctx SpecContext) {
		for range ratelimit.FreeFails - 1 {
			Expect(login(ctx, "192.0.2.14", "admin", "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized))
		}
		Expect(login(ctx, "192.0.2.14", "admin", fakeLimitPassword).Code).To(Equal(http.StatusOK))
		Expect(login(ctx, "192.0.2.14", "admin", "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized), "the count started afresh")
	})

	It("writes an audit event with the username and the client address for a failed login", func(ctx SpecContext) {
		Expect(login(ctx, "192.0.2.20", "admin", "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized))
		Expect(login(ctx, "192.0.2.20", "ghost", "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized))
		Expect(events(ctx)).To(Equal([]loginEvent{
			{Username: "admin", Address: "192.0.2.20"},
			{Username: "ghost", Address: "192.0.2.20"},
		}))
	})

	It("counts the failures of a key over the limit instead of writing one event each", func(ctx SpecContext) {
		failUntilBlocked(ctx, "192.0.2.21", "admin")
		Expect(events(ctx)).To(HaveLen(ratelimit.FreeFails))
		for range 3 {
			Expect(login(ctx, "192.0.2.21", "admin", "koinon_test_wrong").Code).To(Equal(http.StatusTooManyRequests))
		}
		Expect(events(ctx)).To(HaveLen(ratelimit.FreeFails), "no event while blocked")

		advance(ratelimit.BaseBackoff + time.Millisecond)
		Expect(login(ctx, "192.0.2.21", "admin", "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized))
		got := events(ctx)
		Expect(got).To(HaveLen(ratelimit.FreeFails + 1))
		Expect(got[len(got)-1]).To(Equal(loginEvent{Username: "admin", Address: "192.0.2.21", Refused: 3}))
	})

	It("counts failures on an address over the limit too", func(ctx SpecContext) {
		for i := range ratelimit.FreeFails {
			Expect(login(ctx, "192.0.2.22", "u"+string(rune('a'+i)), "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized))
		}
		Expect(login(ctx, "192.0.2.22", "uz", "koinon_test_wrong").Code).To(Equal(http.StatusTooManyRequests))
		Expect(events(ctx)).To(HaveLen(ratelimit.FreeFails))
	})

	It("keeps a username a jsonb summary cannot hold out of the event", func(ctx SpecContext) {
		Expect(login(ctx, "192.0.2.23", `bad\u0000`+strings.Repeat("x", 300), "koinon_test_wrong").Code).To(Equal(http.StatusUnauthorized))
		got := events(ctx)
		Expect(got).To(HaveLen(1))
		Expect(got[0].Username).To(HavePrefix("bad�"))
		Expect([]rune(got[0].Username)).To(HaveLen(maxAuditedUsername))
	})

	It("keeps the argon2id semaphore's answer when every check slot is taken", func(ctx SpecContext) {
		fillVerifySlots()
		rec := login(ctx, "192.0.2.30", "admin", fakeLimitPassword)
		Expect(rec.Code).To(Equal(http.StatusTooManyRequests))
		Expect(rec.Header().Get("Retry-After")).To(Equal("1"))
		Expect(rec.Body.String()).To(ContainSubstring("retry shortly"))
		Expect(events(ctx)).To(BeEmpty(), "a busy check is not a failed login")
	})

	It("leaves ChangePassword outside the login limiter", func(ctx SpecContext) {
		failUntilBlocked(ctx, "192.0.2.31", "admin")
		cred, err := store.New(pool).GetLocalCredentialByUsername(ctx, "admin")
		Expect(err).NotTo(HaveOccurred())
		p := &Principal{UserID: cred.UserID, AuthMethod: store.AuthMethodPassword}
		_, err = rpcHandler{s: svc}.ChangePassword(WithPrincipal(ctx, p), &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: fakeLimitPassword, NewPassword: "koinon_test_not-a-real-password-next",
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = rpcHandler{s: svc}.ChangePassword(WithPrincipal(ctx, p), &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: "koinon_test_wrong", NewPassword: "koinon_test_not-a-real-password-other",
		})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied), "a wrong current password is checked, not rate-limited")
		Expect(events(ctx)).To(HaveLen(ratelimit.FreeFails), "ChangePassword writes no login event")
	})
})
