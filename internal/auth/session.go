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
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/installbinding"
)

// SessionCookieName is the session cookie when Koinon is served over HTTPS.
// The __Host- prefix makes browsers insist on Secure, Path=/ and no Domain, so
// a sibling subdomain cannot set or shadow it.
const SessionCookieName = "__Host-koinon_session"

// InsecureSessionCookieName is the session cookie when Config.PublicURL is
// plain http://. Browsers drop Secure cookies (and so every __Host- cookie)
// over plain HTTP on hosts other than localhost.
const InsecureSessionCookieName = "koinon_session"

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
	// PublicURL is the URL browsers reach Koinon at. Its scheme decides the
	// cookies' Secure attribute: an http:// URL turns Secure (and the
	// __Host- session cookie name) off, for installs without TLS. Empty
	// means https.
	PublicURL string
	// OIDC enables /auth/oidc/*; nil disables OIDC login.
	OIDC *OIDCConfig
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now; tests replace it.
	Now func() time.Time
	// Binding stamps new sessions with the install's mark and refuses sessions that another install
	// issued (a restored copy, ADR 0110 decision 3). Nil: no mark, nothing refused.
	Binding *installbinding.Binding
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

	// secureCookies is false only for a plain http:// PublicURL.
	secureCookies bool
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
	secure, err := secureCookies(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{
		pool:          pool,
		q:             store.New(pool),
		cfg:           cfg,
		log:           cfg.Logger,
		verifySlots:   make(chan struct{}, 4),
		secureCookies: secure,
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

// secureCookies reports whether cookies need the Secure attribute for
// publicURL: everything but plain http.
func secureCookies(publicURL string) (bool, error) {
	if publicURL == "" {
		return true, nil
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return false, fmt.Errorf("public URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return true, nil
	case "http":
		return false, nil
	default:
		return false, fmt.Errorf("public URL %q: scheme must be http or https", publicURL)
	}
}

// SessionCookie returns the name of the session cookie this Service sets:
// SessionCookieName, or InsecureSessionCookieName for a plain http://
// PublicURL.
func (s *Service) SessionCookie() string {
	if s.secureCookies {
		return SessionCookieName
	}
	return InsecureSessionCookieName
}

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
func (s *Service) createSession(ctx context.Context, q *store.Queries, b audit.Batch, userID pgtype.UUID, method store.AuthMethod) (string, store.Session, error) {
	// While the restore release step has not run, the release would delete the new session, and the
	// sessions of the dump are refused: no sign-in makes a session (ADR 0110 decision 1).
	if s.cfg.Binding.SessionsHeld() {
		return "", store.Session{}, installbinding.ErrReleasePending
	}
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
		InstallMark:   s.cfg.Binding.Stamp(),
	})
	if err != nil {
		return "", store.Session{}, err
	}
	// A login is audited in the same transaction as its session. The event sits in the user's
	// personal org, the one org every user has. It carries the method, never a password or token.
	user, err := q.GetActiveUser(ctx, userID)
	if err != nil {
		return "", store.Session{}, fmt.Errorf("load user: %w", err)
	}
	// It goes into the batch, not straight to the org head: a login that also grants or revokes platform
	// admin adds an install event, and the batch writes that one first (ADR 0100 decision 1.7).
	b.Org(audit.Event{
		Org: user.PersonalOrgID, Actor: audit.User(userID), Action: "auth.login", TargetKind: "user", TargetID: userID.String(),
		After: map[string]any{"method": string(method)},
	})
	return token, sess, nil
}

// logout revokes the end of the session that token names, if it was still live.
func (s *Service) logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		uid, err := q.RevokeSessionReturningUser(ctx, store.RevokeSessionReturningUserParams{Now: ts(s.now()), TokenHash: hashToken(token)})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // expired, revoked or unknown: nothing ended now
		}
		if err != nil {
			return err
		}
		user, err := q.GetActiveUser(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: user.PersonalOrgID, Actor: audit.User(uid), Action: "auth.logout", TargetKind: "user", TargetID: uid.String(),
		})
		return nil
	})
	return err
}

// Authenticate resolves a session token into a Principal. It extends the
// session's idle deadline.
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	now := s.now()
	hash := hashToken(token)
	// Until the restore release step has revoked the sessions of the dump, no session is served.
	if s.cfg.Binding.SessionsHeld() {
		s.cfg.Binding.Refused("session")
		return nil, ErrUnauthenticated
	}
	cur, err := s.q.GetValidSession(ctx, store.GetValidSessionParams{Now: ts(now), TokenHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	// A session another install issued (a restored copy) is refused and never comes back: the user
	// logs in again, which makes a session of this install. The check runs before the touch, so a
	// refused session's idle deadline does not move.
	if ok, err := s.cfg.Binding.Allows(ctx, cur.InstallMark); err != nil {
		return nil, fmt.Errorf("check session mark: %w", err)
	} else if !ok {
		s.cfg.Binding.Refused("session")
		s.log.Debug("session refused", "reason", installbinding.QuarantineReason)
		return nil, ErrUnauthenticated
	}
	sess, err := s.q.TouchSession(ctx, store.TouchSessionParams{
		Now:           ts(now),
		IdleExpiresAt: ts(now.Add(s.cfg.SessionIdleTimeout)),
		TokenHash:     hash,
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
	c, err := r.Cookie(s.SessionCookie())
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
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is off only for a plain http:// public URL (Config.PublicURL)
		Name:     s.SessionCookie(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionMaxLifetime / time.Second),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is off only for a plain http:// public URL (Config.PublicURL)
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
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
