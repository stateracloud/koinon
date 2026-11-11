// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/reconciler"
)

// unmarkedConfig is what "koinon-reconciler unmarked list" needs: the database (to know which orgs and
// agents exist), the OpenShell gateway (to list its objects), the install mark (to tell this
// install's objects from another's) and the configured flavors (whose profiles the reconciler claims). The Deployment already gives the reconciler all of them, so
// `kubectl exec deploy/<reconciler> -- koinon-reconciler unmarked list` needs nothing more.
type unmarkedConfig struct {
	databaseURL      string
	openshellAddress string
	openshellTLSDir  string
	installMark      string
	flavors          map[store.AgentFlavor]reconciler.Flavor
	json             bool
}

// newUnmarkedCommand is "koinon-reconciler unmarked" (issue 511, ADR 0106). list is
// its work, so a test can read the settings it resolved.
func newUnmarkedCommand(v *viper.Viper, list func(context.Context, unmarkedConfig, io.Writer) error) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unmarked",
		Short: "Inspect the OpenShell objects that carry no install mark",
		Long: "The gauge koinon_openshell_unmarked_objects counts the OpenShell objects in this install's workspaces " +
			"that carry no install mark (ADR 0106). These commands say which ones.",
	}
	var c unmarkedConfig
	ls := &cobra.Command{
		Use:   "list",
		Short: "List the unmarked objects with the reason each is still unmarked",
		Long: "Lists the sandboxes, providers and provider profiles in this install's workspaces that carry no install mark, " +
			"one per line: workspace, kind, name, reason. It only reads; it changes nothing on the gateway and never lists an " +
			"object that carries another install's mark.\n\n" +
			"Reasons:\n" +
			"  " + reconciler.ReasonPendingClaim + "      an active agent's object, or the profile of a configured flavor no agent of the " +
			"org has; its next reconcile claims it (a sandbox is recreated, a provider recreated once no sandbox attaches it, " +
			"a profile marked)\n" +
			"  " + reconciler.ReasonDraftAgent + "        a draft agent's object, or a flavor profile only draft and archived agents use, " +
			"that the reconciler claims on its next pass; if it is still listed after a pass, the agent's key Secret no longer " +
			"matches its recorded key: activate or archive the agent\n" +
			"  " + reconciler.ReasonArchivedAgent + "     an archived agent's object, or a flavor profile only archived agents use; the " +
			"profile is marked by the reconcile pass of the org, the agent's provider and sandbox are deleted by its cleanup\n" +
			"  " + reconciler.ReasonWaitsForAgent + "    the reconciler cannot claim it while the agent keeps its status: a draft agent's " +
			"stopped sandbox and the providers it attaches (claimed when the agent is activated, removed when it is archived), an " +
			"archived agent's retained sandbox (claimed when the agent is unarchived, or delete it by hand when its home is no " +
			"longer wanted)\n" +
			"  " + reconciler.ReasonOrgNotInDB + "       in a workspace no org of this database owns (left by an earlier install or an e2e " +
			"run on the same gateway); nothing of this install claims it. On a gateway shared with another install it can be that " +
			"install's, which has not claimed its objects yet: check with it before deleting\n" +
			"  " + reconciler.ReasonNotReconcilerMade + "  not made by the reconciler, or not known to this database; nothing claims it\n\n" +
			"Run it in the reconciler pod, which has the settings: kubectl exec deploy/<release>-reconciler -- koinon-reconciler unmarked list",
		Args: cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.ValidateRequiredFlags(); err != nil {
				return err
			}
			images, err := flavorImages(v)
			if err != nil {
				return err
			}
			c.flavors = flavorsOf(images)
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return list(cmd.Context(), c, cmd.OutOrStdout())
		},
	}
	fs := ls.Flags()
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string (default $DATABASE_URL)")
	fs.StringVar(&c.openshellAddress, "openshell-address", "", "OpenShell gateway host:port")
	fs.StringVar(&c.openshellTLSDir, "openshell-tls-dir", "", "directory with tls.crt, tls.key and ca.crt for mTLS; empty connects without TLS")
	fs.StringVar(&c.installMark, "install-mark", "", "this install's random mark (the chart's koinon-install-mark Secret)")
	fs.BoolVar(&c.json, "json", false, "print a JSON array of {workspace, kind, name, reason} instead of text")
	cli.EnvAlias(ls, "database-url", "DATABASE_URL")
	cli.ConfigKey(ls, "openshell-address", configKeys["openshell-address"])
	cli.ConfigKey(ls, "openshell-tls-dir", configKeys["openshell-tls-dir"])
	for _, n := range []string{"database-url", "openshell-address", "install-mark"} {
		if err := ls.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("%s: %v", ls.Name(), err))
		}
	}
	cmd.AddCommand(ls)
	return cmd
}

// listUnmarked is the work of "koinon-reconciler unmarked list".
func listUnmarked(ctx context.Context, c unmarkedConfig, out io.Writer) error {
	pool, err := pgxpool.New(ctx, c.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()
	backend, err := cli.OpenShell{Address: c.openshellAddress, TLSDir: c.openshellTLSDir}.Dial()
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()
	backend.WithOwner(execution.OwnerID(c.installMark))

	objs, err := reconciler.ListUnmarked(ctx, store.New(pool), backend, c.flavors)
	if err != nil {
		return err
	}
	return printUnmarked(out, objs, c.json)
}

// printUnmarked writes the list: one tab-aligned line per object (workspace, kind, name, reason), or a
// JSON array (an empty one is [], never null).
func printUnmarked(out io.Writer, objs []reconciler.UnmarkedObject, asJSON bool) error {
	if asJSON {
		if objs == nil {
			objs = []reconciler.UnmarkedObject{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(objs)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, o := range objs {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", o.Workspace, o.Kind, o.Name, o.Reason); err != nil {
			return err
		}
	}
	return w.Flush()
}
