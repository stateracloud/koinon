// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

const (
	oidcStateCookieName = "koinon_oidc_state"
	oidcCookiePath      = "/auth/oidc/"
	oidcFlowLifetime    = 10 * time.Minute
)

// OIDCConfig configures generic OIDC login (authorization code with PKCE).
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// RedirectURL is Koinon's /auth/oidc/callback as registered at the IdP.
	RedirectURL string
	// GroupsClaim names the ID token claim holding the user's groups.
	GroupsClaim string
	// PlatformAdminGroups are IdP groups (in GroupsClaim) whose members are platform admins. It is
	// reconciled at every login: gaining one of the groups grants, losing all of them revokes, but
	// only a grant this setting made. Empty changes nothing.
	PlatformAdminGroups []string
	// Scopes are requested besides openid. Defaults to email and profile.
	Scopes []string
	// HTTPClient talks to the IdP; defaults to http.DefaultClient.
	HTTPClient *http.Client
}

type oidcClient struct {
	cfg OIDCConfig

	mu       sync.Mutex
	provider *oidc.Provider
}

func newOIDCClient(cfg OIDCConfig) (*oidcClient, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: issuer URL, client ID and redirect URL are required")
	}
	if _, err := url.Parse(cfg.RedirectURL); err != nil {
		return nil, fmt.Errorf("oidc: redirect URL: %w", err)
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.Scopes == nil {
		cfg.Scopes = []string{"email", "profile"}
	}
	return &oidcClient{cfg: cfg}, nil
}

func (c *oidcClient) ctx(ctx context.Context) context.Context {
	if c.cfg.HTTPClient != nil {
		return oidc.ClientContext(ctx, c.cfg.HTTPClient)
	}
	return ctx
}

// setup discovers the provider on first use and caches it, so an IdP that is
// down at startup does not keep the API from starting.
func (c *oidcClient) setup(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider == nil {
		// Discovery and key fetches must outlive this request: go-oidc
		// keeps using the context for JWKS refreshes.
		p, err := oidc.NewProvider(c.ctx(context.WithoutCancel(ctx)), c.cfg.IssuerURL)
		if err != nil {
			return nil, nil, fmt.Errorf("oidc discovery: %w", err)
		}
		c.provider = p
	}
	return c.provider, &oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		RedirectURL:  c.cfg.RedirectURL,
		Endpoint:     c.provider.Endpoint(),
		Scopes:       append([]string{oidc.ScopeOpenID}, c.cfg.Scopes...),
	}, nil
}

// identity is what Koinon takes from a verified ID token.
type identity struct {
	Issuer, Subject, Email, DisplayName string
	Groups                              []string
}

func (c *oidcClient) identityFrom(tok *oidc.IDToken) (identity, error) {
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return identity{}, err
	}
	str := func(k string) string { s, _ := claims[k].(string); return s }
	id := identity{Issuer: tok.Issuer, Subject: tok.Subject, Email: str("email")}
	for _, k := range []string{"name", "preferred_username", "email"} {
		if id.DisplayName = str(k); id.DisplayName != "" {
			break
		}
	}
	switch g := claims[c.cfg.GroupsClaim].(type) {
	case string:
		id.Groups = []string{g}
	case []any:
		for _, v := range g {
			if s, ok := v.(string); ok && s != "" {
				id.Groups = append(id.Groups, s)
			}
		}
	}
	slices.Sort(id.Groups)
	id.Groups = slices.Compact(id.Groups)
	return id, nil
}

// handleOIDCStart begins the authorization code flow with PKCE (S256), a
// nonce, and a state bound to this browser by a short-lived cookie.
func (s *Service) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	_, oc, err := s.oidc.setup(r.Context())
	if err != nil {
		s.log.Error("oidc login unavailable", "err", err)
		http.Error(w, "login provider unavailable", http.StatusBadGateway)
		return
	}
	state, stateHash, err := newToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, _, err := newToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	now := s.now()
	if err := s.q.CreateOIDCLoginFlow(r.Context(), store.CreateOIDCLoginFlowParams{
		StateHash:    stateHash,
		CodeVerifier: verifier,
		Nonce:        nonce,
		ReturnTo:     safeReturnTo(r.URL.Query().Get("return_to")),
		Now:          ts(now),
		ExpiresAt:    ts(now.Add(oidcFlowLifetime)),
	}); err != nil {
		s.log.Error("storing oidc login flow failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is off only for a plain http:// public URL (Config.PublicURL)
		Name:     oidcStateCookieName,
		Value:    state,
		Path:     oidcCookiePath,
		MaxAge:   int(oidcFlowLifetime / time.Second),
		HttpOnly: true,
		Secure:   s.secureCookies,
		// Lax: the IdP's redirect back is a top-level GET navigation.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oc.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

// handleOIDCCallback completes the flow: it checks state against the cookie,
// redeems the code with the PKCE verifier, verifies the ID token and nonce,
// upserts the user by (issuer, subject), syncs group memberships and starts a
// session.
func (s *Service) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	query := r.URL.Query()
	s.clearCookie(w, oidcStateCookieName, oidcCookiePath)
	if e := query.Get("error"); e != "" {
		s.log.Info("oidc login refused by provider", "error", e)
		http.Error(w, "login was refused by the identity provider", http.StatusUnauthorized)
		return
	}
	state := query.Get("state")
	c, err := r.Cookie(oidcStateCookieName)
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "login state mismatch; start the login again", http.StatusBadRequest)
		return
	}
	flow, err := s.q.ConsumeOIDCLoginFlow(ctx, store.ConsumeOIDCLoginFlowParams{
		StateHash: hashToken(state), Now: ts(s.now()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "login expired; start the login again", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Error("loading oidc login flow failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	provider, oc, err := s.oidc.setup(ctx)
	if err != nil {
		s.log.Error("oidc login unavailable", "err", err)
		http.Error(w, "login provider unavailable", http.StatusBadGateway)
		return
	}
	tok, err := oc.Exchange(s.oidc.ctx(ctx), query.Get("code"), oauth2.VerifierOption(flow.CodeVerifier))
	if err != nil {
		s.log.Warn("oidc code exchange failed", "err", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		s.log.Warn("oidc token response has no id_token")
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	idTok, err := provider.VerifierContext(s.oidc.ctx(ctx), &oidc.Config{
		ClientID: s.oidc.cfg.ClientID,
		Now:      s.now,
	}).Verify(ctx, raw)
	if err != nil {
		s.log.Warn("oidc id token rejected", "err", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idTok.Nonce), []byte(flow.Nonce)) != 1 {
		s.log.Warn("oidc id token nonce mismatch")
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	id, err := s.oidc.identityFrom(idTok)
	if err != nil {
		s.log.Warn("oidc claims unreadable", "err", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}

	var token string
	_, err = audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		user, err := s.upsertOIDCUser(ctx, q, b, id)
		if err != nil {
			return err
		}
		token, _, err = s.createSession(ctx, q, user, store.AuthMethodOidc)
		return err
	})
	if errors.Is(err, errUserDisabled) {
		http.Error(w, "this account is disabled", http.StatusForbidden)
		return
	}
	if err != nil {
		s.log.Error("oidc login failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if old, err := r.Cookie(s.SessionCookie()); err == nil {
		_ = s.revokeToken(ctx, old.Value)
	}
	s.setSessionCookie(w, token)
	s.log.Info("user logged in", "method", "oidc", "issuer", id.Issuer)
	http.Redirect(w, r, flow.ReturnTo, http.StatusFound)
}

var errUserDisabled = errors.New("user is disabled")

// upsertOIDCUser finds or creates the user for (issuer, subject), refreshes
// profile fields and reconciles IdP-sourced memberships with id.Groups.
func (s *Service) upsertOIDCUser(ctx context.Context, q *store.Queries, b audit.Batch, id identity) (pgtype.UUID, error) {
	user, err := q.GetUserByIdentity(ctx, store.GetUserByIdentityParams{
		OidcIssuer: id.Issuer, OidcSubject: id.Subject,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		user, err = createUser(ctx, q, id.Issuer, id.Subject, id.Email, id.DisplayName)
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("upsert user: %w", err)
	}
	if user.DeletedAt.Valid {
		return pgtype.UUID{}, errUserDisabled
	}
	if err := q.RecordUserLogin(ctx, store.RecordUserLoginParams{
		ID:          user.ID,
		Email:       pgtype.Text{String: id.Email, Valid: id.Email != ""},
		DisplayName: id.DisplayName,
		LastLoginAt: ts(s.now()),
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("record login: %w", err)
	}
	groups := id.Groups
	if groups == nil {
		groups = []string{}
	}
	if err := q.DeleteStaleGroupMemberships(ctx, store.DeleteStaleGroupMembershipsParams{
		UserID: user.ID, OidcIssuer: id.Issuer, Groups: groups,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("remove stale group memberships: %w", err)
	}
	if err := q.UpsertGroupMemberships(ctx, store.UpsertGroupMembershipsParams{
		UserID: user.ID, OidcIssuer: id.Issuer, Groups: groups,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("sync group memberships: %w", err)
	}
	if err := s.syncPlatformAdmin(ctx, q, b, user, groups); err != nil {
		return pgtype.UUID{}, err
	}
	return user.ID, nil
}

// OIDCSyncActor is the actor of a platform admin grant or revoke from IdP groups at login (ADR 0100
// decision 2).
var OIDCSyncActor = audit.System("koinon-api:oidc-sync")

// syncPlatformAdmin grants or revokes platform admin from the user's groups (oidc.platformAdminGroups).
// It never touches a grant from another source, such as the bootstrap admin's. A grant or revoke is an
// install-wide action, so its event goes in the install audit chain (ADR 0100 decision 3), in the
// transaction of the change (through the audit batch), not in the user's personal org.
func (s *Service) syncPlatformAdmin(ctx context.Context, q *store.Queries, b audit.Batch, user store.User, groups []string) error {
	if s.oidc == nil || len(s.oidc.cfg.PlatformAdminGroups) == 0 {
		return nil
	}
	var matched []string
	for _, g := range s.oidc.cfg.PlatformAdminGroups {
		if slices.Contains(groups, g) {
			matched = append(matched, g)
		}
	}
	ev := audit.InstallEvent{
		Actor: OIDCSyncActor, TargetKind: "user", TargetID: user.ID.String(),
	}
	if len(matched) > 0 {
		_, err := q.GrantPlatformAdminFromGroup(ctx, user.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already a platform admin
		}
		if err != nil {
			return fmt.Errorf("grant platform admin: %w", err)
		}
		ev.Action, ev.After = "platform_admin.grant", map[string]any{"source": "oidc_group", "groups": matched}
		b.Install(ev)
		return nil
	}
	_, err := q.RevokePlatformAdminFromGroup(ctx, user.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("revoke platform admin: %w", err)
	}
	ev.Action, ev.Before = "platform_admin.revoke", map[string]any{"source": "oidc_group"}
	b.Install(ev)
	return nil
}

// safeReturnTo keeps post-login redirects on this site: only absolute paths,
// never "//host" or "/\host", which browsers treat as another origin.
func safeReturnTo(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}
