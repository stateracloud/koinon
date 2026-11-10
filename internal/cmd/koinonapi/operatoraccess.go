// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tokens"
)

// The break-glass commands of issue #512: an operator with kubectl exec on the api pod regains admin
// access with one audited command, without a session. Both write locally, so the restore hold does not
// refuse them (it guards the paths that reach out: runs, approvals, MCP, scope sync).
//
//	kubectl exec -i deploy/koinon-api -- koinon-api users reset-password --username admin < new-password.txt
//	kubectl exec deploy/koinon-api -- koinon-api tokens create --org default --as-user admin --name restore-drill

// maxPasswordStdin bounds what the reset reads from stdin; a password is far shorter.
const maxPasswordStdin = 4096

// operatorGroup is a command group that needs only the database (the pattern of org-limits).
func operatorGroup(use, short, long string) (*cobra.Command, *string) {
	var dbURL string
	root := &cobra.Command{Use: use, Short: short, Long: long}
	root.PersistentFlags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	cli.EnvAlias(root, "database-url", "DATABASE_URL")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// Replaces the root's hook, which resolves flags from the environment; resolve here.
		if err := cli.Resolve(cmd, nil); err != nil {
			return err
		}
		if dbURL == "" {
			return errors.New("--database-url is required (or KOINON_DATABASE_URL, or its alias DATABASE_URL)")
		}
		return nil
	}
	return root, &dbURL
}

func newUsersCommand() *cobra.Command {
	root, dbURL := operatorGroup("users", "Break-glass commands for local users",
		"Run these in a koinon-api pod (kubectl exec); the pod holds the database credentials, so no session is needed. "+
			"Every change is audited with the command as actor.")
	var username, passwordFile string
	reset := &cobra.Command{
		Use:   "reset-password",
		Short: "Set a new password for a local user, end the user's sessions and require a change at the next login",
		Long: "Reads the new password from stdin (never from an argument, so it stays out of the process list and shell " +
			"history) or, with --password-file, from a file such as the mounted bootstrap Secret. The user must choose " +
			"another password at the next login, and every session of the user ends. The event auth.password.reset is " +
			"written to the user's audit trail in the same transaction. The password is never logged or audited.",
		Example: "  kubectl exec -i deploy/koinon-api -- koinon-api users reset-password --username admin < new-password.txt",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if username == "" {
				return errors.New("--username is required: the local login name")
			}
			password, err := readNewPassword(cmd.InOrStdin(), passwordFile)
			if err != nil {
				return err
			}
			pool, err := pgxpool.New(cmd.Context(), *dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			res, err := auth.ResetLocalPassword(cmd.Context(), pool, audit.CLI("users reset-password"), username, password, time.Now())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "password of %s reset; %d session(s) ended; a new password is required at the next login\n", username, res.RevokedSessions)
			return nil
		},
	}
	reset.Flags().StringVar(&username, "username", "", "the local login name (the bootstrap admin is \"admin\" unless the chart renamed it)")
	reset.Flags().StringVar(&passwordFile, "password-file", "", "read the new password from this file instead of stdin (a trailing newline is dropped)")
	root.AddCommand(reset)
	return root
}

// readNewPassword takes the password from file, else from in until EOF. One trailing line break goes
// (echo and editors add it); nothing else is trimmed.
func readNewPassword(in io.Reader, file string) (string, error) {
	if file != "" {
		p, err := auth.ReadSecretFile(file)
		if err != nil {
			return "", fmt.Errorf("read --password-file: %w", err)
		}
		if p == "" {
			return "", errors.New("--password-file is empty")
		}
		return p, nil
	}
	b, err := io.ReadAll(io.LimitReader(in, maxPasswordStdin+1))
	if err != nil {
		return "", fmt.Errorf("read the password from stdin: %w", err)
	}
	if len(b) > maxPasswordStdin {
		return "", errors.New("the password on stdin is too long")
	}
	p := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if p == "" {
		return "", errors.New("no password on stdin: pipe it in, for example: koinon-api users reset-password --username admin < new-password.txt (kubectl exec needs -i)")
	}
	return p, nil
}

func newTokensCommand() *cobra.Command {
	root, dbURL := operatorGroup("tokens", "Break-glass commands for API tokens",
		"Run these in a koinon-api pod (kubectl exec). A token is made on behalf of an existing platform admin or org owner "+
			"(--as-user) and is only as valid as that user: it stops working when the user loses the role. "+
			"Tokens are hashed at rest and every change is audited with the command as actor.")
	var org, asUser, name, role string
	var expiresIn time.Duration
	create := &cobra.Command{
		Use:   "create",
		Short: "Create an API token for an org; the secret is printed once on stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org == "" || asUser == "" || name == "" {
				return errors.New("--org, --as-user and --name are required")
			}
			r := store.OrgRole(role)
			if r != store.OrgRoleAdmin && r != store.OrgRoleOwner {
				return errors.New("--role must be admin or owner")
			}
			pool, err := pgxpool.New(cmd.Context(), *dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			q := store.New(pool)
			orgID, err := resolveOrg(cmd.Context(), q, org)
			if err != nil {
				return err
			}
			user, creatorRole, err := operatorCreator(cmd.Context(), q, orgID, asUser)
			if err != nil {
				return err
			}
			created, err := tokens.New(pool, nil).Create(cmd.Context(), orgID, audit.CLI("tokens create"),
				tokens.Creator{User: user, Role: creatorRole}, name, r, expiresIn)
			if err != nil {
				return err
			}
			// The secret is the only thing on stdout, so `$(koinon-api tokens create ...)` captures it.
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), created.Secret)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "token %s (%s, %s) expires %s; the secret above is not shown again\n",
				created.ID, created.Name, created.Role, created.ExpiresAt.UTC().Format(time.RFC3339))
			return nil
		},
	}
	create.Flags().StringVar(&org, "org", "", "the org's slug or ID")
	create.Flags().StringVar(&asUser, "as-user", "", "the platform admin or org owner the token acts for: a local username or a user ID")
	create.Flags().StringVar(&name, "name", "", "the token's name")
	create.Flags().StringVar(&role, "role", "admin", "the token's role: admin or owner")
	create.Flags().DurationVar(&expiresIn, "expires-in", 0, fmt.Sprintf("how long the token lives (%s to %s; default %s)", tokens.MinTTL, tokens.MaxTTL, tokens.DefaultTTL))

	var id string
	revoke := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a token and every token made from it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org == "" || id == "" {
				return errors.New("--org and --id are required")
			}
			pool, err := pgxpool.New(cmd.Context(), *dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			orgID, err := resolveOrg(cmd.Context(), store.New(pool), org)
			if err != nil {
				return err
			}
			// The operator outranks every token.
			t, err := tokens.New(pool, nil).Revoke(cmd.Context(), orgID, audit.CLI("tokens revoke"), store.OrgRoleOwner, id)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "token %s (%s) revoked\n", t.ID, t.Name)
			return nil
		},
	}
	revoke.Flags().StringVar(&org, "org", "", "the org's slug or ID")
	revoke.Flags().StringVar(&id, "id", "", "the token's ID")
	root.AddCommand(create, revoke)
	return root
}

// operatorCreator finds the user a break-glass token acts for and the role it carries: a platform
// admin counts as owner of every org; anyone else must be an effective owner of this org. A viewer,
// a member or an admin is refused, so the command cannot mint more power than a person holds.
func operatorCreator(ctx context.Context, q *store.Queries, org pgtype.UUID, ref string) (pgtype.UUID, store.OrgRole, error) {
	var uid pgtype.UUID
	if err := uid.Scan(ref); err != nil || !uid.Valid {
		cred, err := q.GetLocalCredentialByUsername(ctx, ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, "", fmt.Errorf("no local user %q (--as-user takes a local username or a user ID)", ref)
		}
		if err != nil {
			return pgtype.UUID{}, "", err
		}
		uid = cred.UserID
	}
	if _, err := q.GetActiveUser(ctx, uid); errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, "", fmt.Errorf("no active user %q", ref)
	} else if err != nil {
		return pgtype.UUID{}, "", err
	}
	if admin, err := q.IsPlatformAdmin(ctx, uid); err != nil {
		return pgtype.UUID{}, "", err
	} else if admin {
		return uid, store.OrgRoleOwner, nil
	}
	ms, err := q.ListEffectiveMemberships(ctx, uid)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	for _, m := range ms {
		if m.OrgID == org && m.Role == store.OrgRoleOwner {
			return uid, store.OrgRoleOwner, nil
		}
	}
	return pgtype.UUID{}, "", fmt.Errorf("%q is neither a platform admin nor an owner of the org: pick another --as-user", ref)
}
