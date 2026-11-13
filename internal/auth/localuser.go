// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// LocalUserSpec describes a local (password) user created by an operator with
// "koinon-api users create". The bootstrap admin is the only other local user.
type LocalUserSpec struct {
	Username    string
	DisplayName string
	Password    string
	// OrgSlug is the org the user joins with Role.
	OrgSlug string
	Role    store.OrgRole
	// MustChangePassword makes the first login change the password, as for the bootstrap admin.
	MustChangePassword bool
}

// CreateLocalUser creates a local user with a personal org, an org membership and an argon2id
// credential, and audits it as org.member.added in the target org. It refuses a username that is
// taken, an unknown org and a password the password policy rejects.
func CreateLocalUser(ctx context.Context, pool *pgxpool.Pool, actor audit.Actor, spec LocalUserSpec, now time.Time) (store.User, error) {
	spec.Username = strings.TrimSpace(spec.Username)
	if spec.Username == "" || len(spec.Username) > 64 {
		return store.User{}, errors.New("username: want 1 to 64 characters")
	}
	if roleRank(spec.Role) == 0 {
		return store.User{}, errors.New("role must be viewer, member, admin or owner")
	}
	if err := ValidateNewPassword(spec.Password); err != nil {
		return store.User{}, err
	}
	displayName := spec.DisplayName
	if displayName == "" {
		displayName = spec.Username
	}
	hash, err := HashPasswordContext(ctx, spec.Password)
	if err != nil {
		return store.User{}, err
	}
	var user store.User
	_, err = audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
		if _, err := q.GetUserByIdentity(ctx, store.GetUserByIdentityParams{
			OidcIssuer: LocalIssuer, OidcSubject: spec.Username,
		}); err == nil {
			return fmt.Errorf("a local user %q exists already", spec.Username)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		org, err := q.GetOrgBySlug(ctx, spec.OrgSlug)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("no org with the slug %q", spec.OrgSlug)
			}
			return fmt.Errorf("load org: %w", err)
		}
		user, err = createUser(ctx, q, LocalIssuer, spec.Username, "", displayName)
		if err != nil {
			return err
		}
		if err := q.AddManualMembership(ctx, store.AddManualMembershipParams{
			OrgID: org.ID, UserID: user.ID, Role: spec.Role,
		}); err != nil {
			return fmt.Errorf("org membership: %w", err)
		}
		if err := q.CreateLocalCredential(ctx, store.CreateLocalCredentialParams{
			UserID: user.ID, Username: spec.Username, PasswordHash: hash,
		}); err != nil {
			return fmt.Errorf("create credential: %w", err)
		}
		if !spec.MustChangePassword {
			if err := q.SetLocalPassword(ctx, store.SetLocalPasswordParams{
				UserID: user.ID, PasswordHash: hash, MustChangePassword: false, PasswordChangedAt: ts(now),
			}); err != nil {
				return fmt.Errorf("set credential: %w", err)
			}
		}
		b.Org(audit.Event{
			Org: org.ID, Actor: actor, Action: "org.member.added", TargetKind: "user", TargetID: user.ID.String(),
			After: map[string]any{"username": spec.Username, "role": string(spec.Role), "source": "cli"},
		})
		return nil
	})
	return user, err
}
