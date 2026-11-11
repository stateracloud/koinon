// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// NewRootCommand is koinon-api's command tree (ADR 0088). Every command declares its own flags and
// the ones it requires, so a check that only the server needs cannot reach "migrate"; the chart's
// contract test (test/chartsmoke) builds this tree to check what each rendered container passes.
// Settings come from the flag, then KOINON_<FLAG>, then the default (internal/cli).
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "koinon-api",
		Short: "Koinon's API server and operator commands",
		Long: "koinon-api serves the Koinon API and web UI (serve, also what runs without a subcommand), " +
			"applies the database migrations (migrate), checks the install prerequisites (preflight), " +
			"creates generated Secrets (ensure-secrets), wraps new KEKs with a KMS (key-admin) and offers the operator " +
			"tools (keys, audit, org-limits).\n\n" +
			"Every flag can also be set by an environment variable: KOINON_ plus the flag name in upper case with " +
			"dashes as underscores (--database-url is KOINON_DATABASE_URL). Secrets are read from files, never from flags.",
	}
	cli.Bind(root, cli.NewViper())
	// The API pod's environment is also the one of "kubectl exec ... koinon-api keys status", which
	// reads KOINON_KEK_ACTIVE_PERIOD (--kek-active-period of the keys commands); serve ignores it.
	cli.AllowEnv(root, "KOINON_MEMORY_LIMIT_BYTES", "KOINON_KEK_ACTIVE_PERIOD")
	cli.DefaultTo(root, "serve")
	root.AddCommand(newServeCommand(), newMigrateCommand(), newPreflightCommand(), newEnsureSecretsCommand(),
		newAuditCommand(), newKeysCommand(), newKeyAdminCommand(), newCheckpointsCommand(), newPricesCommand(), newOrgLimitsCommand())
	return root
}

// databaseFlag registers --database-url. DATABASE_URL, as the dev manifests set it, is its alias.
func (c *config) databaseFlag(fs *pflag.FlagSet) {
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string")
}

func (c *config) durableFlag(fs *pflag.FlagSet, usage string) {
	fs.BoolVar(&c.durable, "durable", false, usage)
}

// restoreHoldFlag registers --restore-hold (issue #462).
func (c *config) restoreHoldFlag(fs *pflag.FlagSet) { restorehold.Flag(fs, &c.restoreHold) }

func (c *config) waitFlag(fs *pflag.FlagSet) {
	fs.DurationVar(&c.wait, "wait", 5*time.Minute, "how long migrations wait for the database to come up")
}

func newServeCommand() *cobra.Command {
	var c config
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the API, the web UI and the listeners beside them",
		Long: "Serves the API and web UI on --addr. It also serves the GitHub connector's MCP tools on --tools-addr, " +
			"a separate listener that only agentgateway may reach: the tools trust the gateway's agent header, so the " +
			"listener must never be exposed through the API's Service or ingress.",
		Args: cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			// Only the server opens the OTLP receiver; "migrate" (the chart's migrate Job, which
			// mounts no token) must not need its token.
			if c.otlpAddr != "" && c.otlpTokenDir == "" {
				return errors.New("--otlp-token-dir is required with --otlp-addr: the OTLP receiver never runs without its bearer token (pass --otlp-addr= to turn the receiver off)")
			}
			if c.otlpAddr != "" && c.otlpTracesTokenDir == "" {
				return errors.New("--otlp-traces-token-dir is required with --otlp-addr: /v1/traces has its own bearer token, held only by koinon-agentd (pass --otlp-addr= to turn the receiver off)")
			}
			if c.oidcIssuer == "" {
				mappings, err := auth.ParseGroupMappings(c.defaultOrgGroups)
				if err != nil {
					return err
				}
				if len(mappings) > 0 {
					return errors.New("--default-org-group-mappings needs --oidc-issuer-url")
				}
				if c.oidcPlatformAdmins != "" {
					return errors.New("--oidc-platform-admin-groups needs --oidc-issuer-url")
				}
			}
			if c.oidcRedirectURL == "" && c.publicURL != "" {
				c.oidcRedirectURL = strings.TrimRight(c.publicURL, "/") + "/auth/oidc/callback"
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c.flags = cmd.Flags()
			return serve(cmd.Context(), c)
		},
	}
	c.serveFlags(cmd.Flags())
	cli.EnvAlias(cmd, "database-url", "DATABASE_URL")
	cli.EnvAlias(cmd, "secrets-namespace", "POD_NAMESPACE")
	mustRequire(cmd, "database-url")
	return cmd
}

func newMigrateCommand() *cobra.Command {
	var (
		c                         config
		grantsOnly, requireWriter bool
	)
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply the database migrations and exit",
		Long: "Waits for the database, applies the pending migrations and exits; the Helm chart runs it as a hook Job " +
			"with the migration role (ADR 0060). It creates the DBOS tables only with --durable. With --grants-only it " +
			"applies no migration: it waits for the install audit writer role and grants it its privileges, as the " +
			"chart's post-upgrade hook does for a CloudNativePG cluster (ADR 0100 decision 1.1a).",
		Args: cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			if grantsOnly && c.durable {
				return errors.New("--grants-only and --durable are exclusive: --grants-only applies no migration")
			}
			if grantsOnly && requireWriter {
				return errors.New("--grants-only and --require-install-writer are exclusive: --grants-only waits for the role")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return migrate(cmd.Context(), c.databaseURL, c.wait, migrateOptions{durable: c.durable, grantsOnly: grantsOnly, requireWriter: requireWriter})
		},
	}
	c.databaseFlag(cmd.Flags())
	fs := cmd.Flags()
	fs.DurationVar(&c.wait, "wait", 5*time.Minute, "how long migrations wait for the database to come up "+
		"(with --grants-only: for the database, the install audit writer role and its tables)")
	c.durableFlag(fs, "also create the DBOS runtime's tables (needed when the API runs with --durable; ADR 0066)")
	fs.BoolVar(&grantsOnly, "grants-only", false, "apply no migration; wait for the install audit writer role and grant it "+
		"its privileges (the chart's post-upgrade hook in CloudNativePG mode)")
	fs.BoolVar(&requireWriter, "require-install-writer", false, "fail when the install audit writer role does not exist, "+
		"instead of warning (the chart passes it on an external database, where nothing creates the role later; ADR 0100 decision 1.1a). It runs before any migration")
	cli.EnvAlias(cmd, "database-url", "DATABASE_URL")
	mustRequire(cmd, "database-url")
	return cmd
}

// mustRequire marks flags required. A required flag may come from the environment: cli.Resolve
// runs before cobra's check.
func mustRequire(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		if err := cmd.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("%s: %v", cmd.Name(), err))
		}
	}
}
