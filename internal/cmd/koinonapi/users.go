// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
)

// newUsersCommand is "koinon-api users", the operator's way to add a local (password) user to an org.
// Run it in a koinon-api pod: kubectl exec -i deploy/koinon-api -- koinon-api users create --org acme
// --username bob --role member --password-stdin. The restore drill seeds its admin and member this way.
func newUsersCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "users",
		Short: "Create local (password) users",
		Long: "Local users sign in with a username and password at /auth/login (ADR 0022). Creating one is audited " +
			"as org.member.added. The password is read from stdin or a file, never from an argument.",
	}
	var dbURL, org, username, display, role, passwordFile string
	var passwordStdin, mustChange bool
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a local user with a role in an org",
		Args:  cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cli.Resolve(cmd, nil); err != nil {
				return err
			}
			switch {
			case dbURL == "":
				return errors.New("--database-url is required (or KOINON_DATABASE_URL, or its alias DATABASE_URL)")
			case org == "" || username == "":
				return errors.New("--org (the org's slug) and --username are required")
			case passwordStdin == (passwordFile != ""):
				return errors.New("give the password with exactly one of --password-stdin and --password-file")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			var password string
			if passwordStdin {
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4096))
				if err != nil {
					return fmt.Errorf("read password: %w", err)
				}
				password = strings.TrimRight(string(b), "\r\n")
			} else {
				p, err := auth.ReadSecretFile(passwordFile)
				if err != nil {
					return fmt.Errorf("read password file: %w", err)
				}
				password = p
			}
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			u, err := auth.CreateLocalUser(cmd.Context(), pool, audit.CLI("users create"), auth.LocalUserSpec{
				Username: username, DisplayName: display, Password: password,
				OrgSlug: org, Role: store.OrgRole(role), MustChangePassword: mustChange,
			}, time.Now())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "created local user %s (%s) with role %s in %s\n", username, u.ID.String(), role, org)
			return err
		},
	}
	f := create.Flags()
	f.StringVar(&dbURL, "database-url", "", "Postgres connection string")
	cli.EnvAlias(create, "database-url", "DATABASE_URL")
	f.StringVar(&org, "org", "", "the org's slug")
	f.StringVar(&username, "username", "", "the login name (1 to 64 characters)")
	f.StringVar(&display, "display-name", "", "the name shown in the console (default: the username)")
	f.StringVar(&role, "role", "member", "the org role: viewer, member, admin or owner")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	f.StringVar(&passwordFile, "password-file", "", "read the password from this file")
	f.BoolVar(&mustChange, "must-change-password", true, "make the first login change the password; --must-change-password=false for a seeded test user")
	root.AddCommand(create)
	return root
}
