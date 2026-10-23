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

	"github.com/procoduck/koinon/internal/db/store"
)

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
		if err := q.GrantPlatformAdmin(ctx, store.GrantPlatformAdminParams{UserID: admin.ID}); err != nil {
			return fmt.Errorf("grant platform admin: %w", err)
		}

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
			if err := q.SetOrgIdentityMode(ctx, store.SetOrgIdentityModeParams{
				ID: org.ID, IdentityMode: cfg.DefaultOrgIdentityMode,
			}); err != nil {
				return fmt.Errorf("default org identity mode: %w", err)
			}
		}

		if cfg.OIDCIssuer != "" {
			keep := make([]string, 0, len(cfg.GroupMappings))
			for _, m := range cfg.GroupMappings {
				if err := q.UpsertGroupMapping(ctx, store.UpsertGroupMappingParams{
					OrgID: org.ID, OidcIssuer: cfg.OIDCIssuer, GroupName: m.Group, Role: m.Role,
				}); err != nil {
					return fmt.Errorf("group mapping %q: %w", m.Group, err)
				}
				keep = append(keep, m.Group)
			}
			if err := q.DeleteGroupMappingsExcept(ctx, store.DeleteGroupMappingsExceptParams{
				OrgID: org.ID, OidcIssuer: cfg.OIDCIssuer, KeepGroups: keep,
			}); err != nil {
				return fmt.Errorf("prune group mappings: %w", err)
			}
		}

		if !cfg.LoginEnabled {
			n, err := q.RevokeSessionsByMethod(ctx, store.RevokeSessionsByMethodParams{
				Now: ts(now), AuthMethod: store.AuthMethodPassword,
			})
			res.RevokedSessions = n
			return err
		}
		_, err = q.GetLocalCredential(ctx, admin.ID)
		if err == nil {
			// Already set; the file is not read again.
			if err := q.SetLocalUsername(ctx, store.SetLocalUsernameParams{
				UserID: admin.ID, Username: cfg.Username,
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
		hash, err := HashPassword(password)
		if err != nil {
			return err
		}
		if err := q.CreateLocalCredential(ctx, store.CreateLocalCredentialParams{
			UserID: admin.ID, Username: cfg.Username, PasswordHash: hash,
		}); err != nil {
			return fmt.Errorf("create credential: %w", err)
		}
		res.CreatedCredential = true
		return nil
	})
	return res, err
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
