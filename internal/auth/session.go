// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/procoduck/koinon/internal/db/store"
)

// SessionCookieName is the session cookie. The __Host- prefix makes browsers
// insist on Secure, Path=/ and no Domain, so a sibling subdomain cannot set
// or shadow it.
const SessionCookieName = "__Host-koinon_session"

// Defaults for session lifetimes. Group changes in the IdP take effect at the
// next login, so the absolute lifetime bounds how long a removed group keeps
// granting access (ADR 0004).
const (
	DefaultSessionIdleTimeout = 2 * time.Hour
	DefaultSessionMaxLifetime = 12 * time.Hour
)

// ErrUnauthenticated means the request carries no valid session.
var ErrUnauthenticated = errors.New("unauthenticated")

// Config configures a Service.
type Config struct {
	// SessionIdleTimeout ends a session after this long without requests.
	SessionIdleTimeout time.Duration
	// SessionMaxLifetime ends a session this long after login, however
	// active it is.
	SessionMaxLifetime time.Duration
	// BootstrapLogin enables /auth/login for the bootstrap admin.
	BootstrapLogin bool
	// OIDC enables /auth/oidc/*; nil disables OIDC login.
	OIDC *OIDCConfig
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now; tests replace it.
	Now func() time.Time
}

// Service authenticates requests and serves the login endpoints and the
// AuthService RPCs.
type Service struct {
	pool *pgxpool.Pool
	q    *store.Queries
	cfg  Config
	log  *slog.Logger
	oidc *oidcClient

	// verifySlots bounds concurrent argon2id verifications (64 MiB each), so
	// a burst of login attempts cannot exhaust memory.
	verifySlots chan struct{}
}

// New returns a Service backed by pool.
func New(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.SessionIdleTimeout <= 0 {
		cfg.SessionIdleTimeout = DefaultSessionIdleTimeout
	}
	if cfg.SessionMaxLifetime <= 0 {
		cfg.SessionMaxLifetime = DefaultSessionMaxLifetime
	}
	if cfg.SessionIdleTimeout > cfg.SessionMaxLifetime {
		return nil, fmt.Errorf("session idle timeout %s exceeds max lifetime %s",
			cfg.SessionIdleTimeout, cfg.SessionMaxLifetime)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{
		pool:        pool,
		q:           store.New(pool),
		cfg:         cfg,
		log:         cfg.Logger,
		verifySlots: make(chan struct{}, 4),
	}
	if cfg.OIDC != nil {
		c, err := newOIDCClient(*cfg.OIDC)
		if err != nil {
			return nil, err
		}
		s.oidc = c
	}
	return s, nil
}

func (s *Service) now() time.Time { return s.cfg.Now() }

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// newToken returns a random URL-safe token and its SHA-256.
func newToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// createSession stores a new session for user and returns the cookie token.
func (s *Service) createSession(ctx context.Context, q *store.Queries, userID pgtype.UUID, method store.AuthMethod) (string, store.Session, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", store.Session{}, err
	}
	now := s.now()
	sess, err := q.CreateSession(ctx, store.CreateSessionParams{
		TokenHash:     hash,
		UserID:        userID,
		AuthMethod:    method,
		Now:           ts(now),
		IdleExpiresAt: ts(now.Add(s.cfg.SessionIdleTimeout)),
		ExpiresAt:     ts(now.Add(s.cfg.SessionMaxLifetime)),
	})
	return token, sess, err
}

// Authenticate resolves a session token into a Principal. It extends the
// session's idle deadline.
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	now := s.now()
	sess, err := s.q.TouchSession(ctx, store.TouchSessionParams{
		Now:           ts(now),
		IdleExpiresAt: ts(now.Add(s.cfg.SessionIdleTimeout)),
		TokenHash:     hashToken(token),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	return s.loadPrincipal(ctx, sess)
}

func (s *Service) loadPrincipal(ctx context.Context, sess store.Session) (*Principal, error) {
	user, err := s.q.GetActiveUser(ctx, sess.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	p := &Principal{
		UserID:        user.ID,
		Email:         user.Email.String,
		DisplayName:   user.DisplayName,
		PersonalOrgID: user.PersonalOrgID,
		SessionID:     sess.ID,
		AuthMethod:    sess.AuthMethod,
	}
	if p.PlatformAdmin, err = s.q.IsPlatformAdmin(ctx, user.ID); err != nil {
		return nil, fmt.Errorf("load platform admin: %w", err)
	}
	if sess.AuthMethod == store.AuthMethodPassword {
		// Disabling bootstrap login ends bootstrap sessions too, on every
		// replica, whether or not they were revoked at startup.
		if !s.cfg.BootstrapLogin {
			return nil, ErrUnauthenticated
		}
		cred, err := s.q.GetLocalCredential(ctx, user.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUnauthenticated
		}
		if err != nil {
			return nil, fmt.Errorf("load credential: %w", err)
		}
		p.PasswordChangeRequired = cred.MustChangePassword
	}
	rows, err := s.q.ListEffectiveMemberships(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("load memberships: %w", err)
	}
	for _, r := range rows {
		p.Memberships = append(p.Memberships, Membership{
			OrgID: r.OrgID, OrgSlug: r.OrgSlug, OrgDisplayName: r.OrgDisplayName, Role: r.Role,
		})
	}
	return p, nil
}

// AuthenticateRequest resolves the session cookie of r. It is for plain HTTP
// handlers; RPCs use the interceptor.
func (s *Service) AuthenticateRequest(r *http.Request) (*Principal, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	return s.Authenticate(r.Context(), c.Value)
}

// revokeToken revokes the session behind token, if any.
func (s *Service) revokeToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.q.RevokeSession(ctx, store.RevokeSessionParams{Now: ts(s.now()), TokenHash: hashToken(token)})
}

func (s *Service) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionMaxLifetime / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// PurgeExpired deletes sessions and OIDC login flows that ended before
// cutoff. Revoked and expired sessions stay until then, for audit.
func (s *Service) PurgeExpired(ctx context.Context, cutoff time.Time) error {
	if _, err := s.q.DeleteExpiredSessions(ctx, ts(cutoff)); err != nil {
		return err
	}
	_, err := s.q.DeleteExpiredOIDCLoginFlows(ctx, ts(s.now()))
	return err
}

// RunPurger purges expired sessions every interval until ctx ends, keeping
// ended sessions for retain.
func (s *Service) RunPurger(ctx context.Context, interval, retain time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.PurgeExpired(ctx, s.now().Add(-retain)); err != nil && ctx.Err() == nil {
			s.log.Warn("purging expired sessions failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
