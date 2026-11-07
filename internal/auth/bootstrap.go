// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// BootstrapActor is the actor of everything the start-up bootstrap changes (ADR 0100 decision 2).
var BootstrapActor = audit.System("koinon-api:bootstrap")

// The bootstrap admin is a local user, not an OIDC one. users requires an
// (issuer, subject) pair, so it gets a reserved non-URL issuer that no OIDC
// provider can produce, and a fixed subject: the login username is
// configurable and may change, the user it names may not.
const (
	LocalIssuer                   = "koinon:local"
	BootstrapAdminSubject         = "admin"
	DefaultBootstrapAdminUsername = "admin"
	DefaultOrgSlug                = "default"
	DefaultOrgDisplayName         = "Default"
)

// GroupMapping maps an IdP group to a role in the default org.
type GroupMapping struct {
	Group string
	Role  store.OrgRole
}

// ParseGroupMappings parses "group=role,group=role".
func ParseGroupMappings(s string) ([]GroupMapping, error) {
	var out []GroupMapping
	seen := map[string]bool{}
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		group, role, ok := strings.Cut(item, "=")
		group, role = strings.TrimSpace(group), strings.TrimSpace(role)
		if !ok || group == "" {
			return nil, fmt.Errorf("group mapping %q: want group=role", item)
		}
		if roleRank(store.OrgRole(role)) == 0 {
			return nil, fmt.Errorf("group mapping %q: role must be viewer, member, admin or owner", item)
		}
		if seen[group] {
			return nil, fmt.Errorf("group mapping %q: group listed twice", item)
		}
		seen[group] = true
		out = append(out, GroupMapping{Group: group, Role: store.OrgRole(role)})
	}
	return out, nil
}

// ParseIdentityMode validates an org identity mode.
func ParseIdentityMode(s string) (store.IdentityMode, error) {
	switch m := store.IdentityMode(s); m {
	case store.IdentityModePlatform, store.IdentityModeIdp, store.IdentityModeHybrid:
		return m, nil
	default:
		return "", fmt.Errorf("identity mode %q: want platform, idp or hybrid", s)
	}
}

// BootstrapConfig configures Bootstrap.
type BootstrapConfig struct {
	// Username is the bootstrap admin's login name, default "admin". It is
	// applied on every start, so changing it renames the existing login.
	Username string
	// PasswordFile holds the bootstrap admin's one-time password, typically a
	// mounted Kubernetes Secret. It is read only when the credential does
	// not exist yet, and only its argon2id hash is stored. A configured but
	// unreadable or empty file is an error.
	PasswordFile string
	// LoginEnabled mirrors Config.BootstrapLogin. When false, no credential
	// is created and existing bootstrap sessions are revoked.
	LoginEnabled bool
	// DefaultOrgIdentityMode is applied to the default org on every start.
	DefaultOrgIdentityMode store.IdentityMode
	// OIDCIssuer and GroupMappings define the default org's group mappings.
	// They are reconciled on every start: mappings for this issuer that are
	// not listed are removed, with the memberships they granted.
	OIDCIssuer    string
	GroupMappings []GroupMapping
}

// BootstrapResult reports what Bootstrap created.
type BootstrapResult struct {
	CreatedAdmin      bool
	CreatedOrg        bool
	CreatedCredential bool
	RevokedSessions   int64
	// MissingPasswordFile is set when bootstrap login is enabled, the admin
	// has no password yet and no password file is configured: nobody can
	// log in with a password until one is.
	MissingPasswordFile bool
}

// Bootstrap idempotently creates the default org and the bootstrap platform
// admin, who is the org's manual owner (plan item A1). It is safe to run on
// every start and on several replicas at once.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, cfg BootstrapConfig, now time.Time) (BootstrapResult, error) {
	var res BootstrapResult
	if cfg.DefaultOrgIdentityMode == "" {
		cfg.DefaultOrgIdentityMode = store.IdentityModeHybrid
	}
	if cfg.Username == "" {
		cfg.Username = DefaultBootstrapAdminUsername
	}
	if len(cfg.Username) > 64 || strings.TrimSpace(cfg.Username) != cfg.Username {
		return res, fmt.Errorf("bootstrap admin username %q: want at most 64 characters, no surrounding spaces", cfg.Username)
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockBootstrap(ctx); err != nil {
			return err
		}

		admin, err := q.GetUserByIdentity(ctx, store.GetUserByIdentityParams{
			OidcIssuer: LocalIssuer, OidcSubject: BootstrapAdminSubject,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			admin, err = createUser(ctx, q, LocalIssuer, BootstrapAdminSubject, "", "Bootstrap admin")
			res.CreatedAdmin = true
		}
		if err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
		grantedRows, err := q.GrantPlatformAdmin(ctx, store.GrantPlatformAdminParams{UserID: admin.ID})
		if err != nil {
			return fmt.Errorf("grant platform admin: %w", err)
		}
		// Audit events wait until every domain write of this transaction is done: the lock order is
		// domain locks first, then the install event, then org events (ADR 0100 decision 1.7). An OIDC
		// login takes its group-mapping locks before the install head, so bootstrap must too.
		var orgEvents []audit.Event

		org, err := q.GetOrgBySlug(ctx, DefaultOrgSlug)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			org, err = q.CreateOrg(ctx, store.CreateOrgParams{
				Slug: DefaultOrgSlug, DisplayName: DefaultOrgDisplayName,
				Kind: store.OrgKindTeam, IdentityMode: cfg.DefaultOrgIdentityMode,
			})
			if err != nil {
				return fmt.Errorf("create default org: %w", err)
			}
			if err := q.AddManualMembership(ctx, store.AddManualMembershipParams{
				OrgID: org.ID, UserID: admin.ID, Role: store.OrgRoleOwner,
			}); err != nil {
				return fmt.Errorf("default org owner: %w", err)
			}
			res.CreatedOrg = true
		case err != nil:
			return fmt.Errorf("load default org: %w", err)
		default:
			n, err := q.SetOrgIdentityMode(ctx, store.SetOrgIdentityModeParams{
				ID: org.ID, IdentityMode: cfg.DefaultOrgIdentityMode,
			})
			if err != nil {
				return fmt.Errorf("default org identity mode: %w", err)
			}
			if n > 0 {
				orgEvents = append(orgEvents, bootstrapOrgEvent(org.ID, "org.identity_mode.set",
					map[string]any{"identityMode": string(org.IdentityMode)}, map[string]any{"identityMode": string(cfg.DefaultOrgIdentityMode)}))
			}
		}

		// flush writes the install event (the first admin's creation, or a grant that came back
		// after it was removed), then the org events of this transaction.
		flush := func() error {
			var installID string
			switch {
			case res.CreatedAdmin:
				ev, err := audit.RecordInstall(ctx, q, audit.InstallEvent{
					Actor: BootstrapActor, Action: "platform_admin.bootstrap", TargetKind: "org", TargetID: org.ID.String(),
					After: map[string]any{"userId": admin.ID.String(), "source": "bootstrap", "defaultOrgSlug": org.Slug},
				})
				if err != nil {
					return err
				}
				installID = ev.ID.String()
			case grantedRows > 0:
				ev, err := audit.RecordInstall(ctx, q, audit.InstallEvent{
					Actor: BootstrapActor, Action: "platform_admin.grant", TargetKind: "user", TargetID: admin.ID.String(),
					After: map[string]any{"source": "bootstrap"},
				})
				if err != nil {
					return err
				}
				installID = ev.ID.String()
			}
			for _, ev := range orgEvents {
				ev.InstallEventID = installID
				if err := audit.RecordErr(ctx, q, ev); err != nil {
					return err
				}
			}
			return nil
		}

		if cfg.OIDCIssuer != "" {
			keep := make([]string, 0, len(cfg.GroupMappings))
			var changed int64
			for _, m := range cfg.GroupMappings {
				n, err := q.UpsertGroupMapping(ctx, store.UpsertGroupMappingParams{
					OrgID: org.ID, OidcIssuer: cfg.OIDCIssuer, GroupName: m.Group, Role: m.Role,
				})
				if err != nil {
					return fmt.Errorf("group mapping %q: %w", m.Group, err)
				}
				changed += n
				keep = append(keep, m.Group)
			}
			removed, err := q.DeleteGroupMappingsExcept(ctx, store.DeleteGroupMappingsExceptParams{
				OrgID: org.ID, OidcIssuer: cfg.OIDCIssuer, KeepGroups: keep,
			})
			if err != nil {
				return fmt.Errorf("prune group mappings: %w", err)
			}
			if changed+removed > 0 {
				mappings := make([]string, 0, len(cfg.GroupMappings))
				for _, m := range cfg.GroupMappings {
					mappings = append(mappings, m.Group+"="+string(m.Role))
				}
				orgEvents = append(orgEvents, bootstrapOrgEvent(org.ID, "org.group_mappings.set", nil,
					map[string]any{"issuer": cfg.OIDCIssuer, "mappings": mappings, "added": changed, "removed": removed}))
			}
		}

		if !cfg.LoginEnabled {
			n, err := q.RevokeSessionsByMethod(ctx, store.RevokeSessionsByMethodParams{
				Now: ts(now), AuthMethod: store.AuthMethodPassword,
			})
			res.RevokedSessions = n
			if err != nil {
				return err
			}
			if n > 0 {
				orgEvents = append(orgEvents, bootstrapOrgEvent(org.ID, "org.bootstrap_sessions.revoked", nil, map[string]any{"revoked": n}))
			}
			return flush()
		}
		if err := ensureCredential(ctx, q, cfg, admin.ID, &res); err != nil {
			return err
		}
		return flush()
	})
	return res, err
}

// bootstrapOrgEvent is a change bootstrap made to the default org, in that org's chain.
func bootstrapOrgEvent(org pgtype.UUID, action string, before, after any) audit.Event {
	return audit.Event{Org: org, Actor: BootstrapActor, Action: action, TargetKind: "org", TargetID: org.String(), Before: before, After: after}
}

// ensureCredential creates the bootstrap admin's login on the first start and keeps its username
// current afterwards. The credential is not audited: it is the admin's own secret, and creating the
// admin is the install event.
func ensureCredential(ctx context.Context, q *store.Queries, cfg BootstrapConfig, admin pgtype.UUID, res *BootstrapResult) error {
	_, err := q.GetLocalCredential(ctx, admin)
	if err == nil {
		// Already set; the file is not read again.
		if err := q.SetLocalUsername(ctx, store.SetLocalUsernameParams{
			UserID: admin, Username: cfg.Username,
		}); err != nil {
			return fmt.Errorf("rename bootstrap admin: %w", err)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load credential: %w", err)
	}
	if cfg.PasswordFile == "" {
		res.MissingPasswordFile = true
		return nil
	}
	password, err := readPasswordFile(cfg.PasswordFile)
	if err != nil {
		return err
	}
	hash, err := HashPasswordContext(ctx, password)
	if err != nil {
		return err
	}
	if err := q.CreateLocalCredential(ctx, store.CreateLocalCredentialParams{
		UserID: admin, Username: cfg.Username, PasswordHash: hash,
	}); err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	res.CreatedCredential = true
	return nil
}

func readPasswordFile(path string) (string, error) {
	password, err := ReadSecretFile(path)
	if err != nil {
		return "", fmt.Errorf("read bootstrap password file: %w", err)
	}
	if password == "" {
		return "", errors.New("bootstrap password file is empty")
	}
	return password, nil
}

// createUser creates a user with its personal org and manual owner membership
// (every user has exactly one personal org). Call it inside a transaction:
// the org's manual-owner check runs at commit.
func createUser(ctx context.Context, q *store.Queries, issuer, subject, email, displayName string) (store.User, error) {
	slug, err := personalOrgSlug()
	if err != nil {
		return store.User{}, err
	}
	org, err := q.CreateOrg(ctx, store.CreateOrgParams{
		Slug: slug, DisplayName: "Personal", Kind: store.OrgKindPersonal,
		IdentityMode: store.IdentityModePlatform,
	})
	if err != nil {
		return store.User{}, fmt.Errorf("create personal org: %w", err)
	}
	user, err := q.CreateUser(ctx, store.CreateUserParams{
		OidcIssuer:    issuer,
		OidcSubject:   subject,
		Email:         pgtype.Text{String: email, Valid: email != ""},
		DisplayName:   displayName,
		PersonalOrgID: org.ID,
	})
	if err != nil {
		return store.User{}, fmt.Errorf("create user: %w", err)
	}
	if err := q.AddManualMembership(ctx, store.AddManualMembershipParams{
		OrgID: org.ID, UserID: user.ID, Role: store.OrgRoleOwner,
	}); err != nil {
		return store.User{}, fmt.Errorf("personal org owner: %w", err)
	}
	return user, nil
}

// personalOrgSlug returns a random DNS-safe slug. Personal org slugs are not
// derived from user names, which can change and collide.
func personalOrgSlug() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "u-" + hex.EncodeToString(b), nil
}
