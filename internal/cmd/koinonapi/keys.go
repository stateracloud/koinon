// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

type keysOptions struct {
	dbURL, kekSecret, kekDir, namespace, kubeconfig, id string
	period                                              time.Duration
	yes                                                 bool
	// recordStarted writes the .started audit event of rewrap and reencrypt.
	recordStarted bool
	// all and agents choose the agents of new-repositories.
	all    bool
	agents []string
	// wrapping is how the KEKs are wrapped; zero is the default local Secret-held KEK.
	wrapping wrapping.Config
}

// prompterFor decides whether a keys command may ask the person (cli.PrompterFor); tests replace it.
var prompterFor = cli.PrompterFor

// oneOrPrompt accepts a missing ID: on a terminal the command asks for it. PreRunE rejects a missing
// ID when it cannot ask, with the error ExactArgs(1) gives.
func oneOrPrompt(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return cobra.ExactArgs(1)(cmd, args)
}

// newKeysCommand is "koinon-api keys", the key-encryption key (KEK) tool.
func newKeysCommand() *cobra.Command {
	var o keysOptions
	keys := &cobra.Command{
		Use:   "keys",
		Short: "Manage the key-encryption keys (KEKs)",
		Long:  "Run it in a koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api keys status",
	}
	pf := keys.PersistentFlags()
	pf.StringVar(&o.dbURL, "database-url", "", "Postgres connection string (add, status, rewrap, retire, reencrypt, forget and new-repositories; every command for the install ID that KMS-wrapped KEKs are bound to)")
	pf.StringVar(&o.kekSecret, "kek-secret", "", "name of the KEK Secret; the key set is read and changed through the Kubernetes API")
	pf.StringVar(&o.kekDir, "kek-dir", "", "mounted KEK directory, read-only; used when --kek-secret is not set")
	pf.StringVar(&o.namespace, "namespace", "", "namespace of the KEK Secret")
	pf.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	pf.DurationVar(&o.period, "kek-active-period", keyscli.DefaultActivePeriod, "status warns when the active KEK is older than this")
	o.wrapping.Flags(pf)
	cli.EnvAlias(keys, "database-url", "DATABASE_URL")
	cli.EnvAlias(keys, "namespace", "POD_NAMESPACE")

	sub := func(use, short string, args cobra.PositionalArgs, needsDB bool) *cobra.Command {
		name, _, _ := strings.Cut(use, " ")
		return &cobra.Command{
			Use: use, Short: short, Args: args,
			PreRunE: func(cmd *cobra.Command, args []string) error {
				// A missing ID is for the person to pick, if there is one to ask (the flags and
				// KOINON_NO_INPUT are resolved by now); otherwise it is the error ExactArgs gives.
				if len(args) == 0 && strings.Contains(use, " ") && prompterFor(cmd) == nil {
					return cobra.ExactArgs(1)(cmd, args)
				}
				if needsDB && o.dbURL == "" {
					return errors.New("--database-url is required")
				}
				return nil
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				return runKeys(cmd.Context(), o, name, args, cmd.OutOrStdout(), prompterFor(cmd))
			},
		}
	}
	confirmed := func(c *cobra.Command) *cobra.Command {
		c.Flags().BoolVar(&o.yes, "yes", false, "on a terminal, do not ask for confirmation")
		return c
	}
	// The per-row loop of rewrap and reencrypt is not atomic, so it may also record its start (ADR 0100
	// decision 3): a run that dies half way then shows in the install audit log.
	started := func(c *cobra.Command) *cobra.Command {
		c.Flags().BoolVar(&o.recordStarted, "record-started", false, "also write a keys."+c.Name()+".started install audit event before the run, so a run that dies half way shows")
		return c
	}
	add := sub("add", "Generate a new KEK (not active yet)", cobra.NoArgs, true)
	newRepos := confirmed(sub("new-repositories", "After a compromise: give agents new home repositories (new master keys) seeded with their newest checkpoint, and drop the older checkpoints", cobra.NoArgs, true))
	newRepos.Flags().BoolVar(&o.all, "all", false, "every agent with a home repository")
	newRepos.Flags().StringSliceVar(&o.agents, "agent", nil, "an agent ID (repeat it, or separate IDs with commas)")
	newRepos.MarkFlagsMutuallyExclusive("all", "agent")
	newRepos.MarkFlagsOneRequired("all", "agent")
	add.Flags().StringVar(&o.id, "id", "", "the new KEK's ID (default kek-YYYYMMDD-xxxx)")
	keys.AddCommand(
		sub("status", "List the KEKs, the active one and the values each wraps", cobra.NoArgs, true),
		add,
		confirmed(sub("activate [ID]", "Make a KEK the one that wraps new data keys (on a terminal, pick it from a list without ID)", oneOrPrompt, true)),
		started(confirmed(sub("rewrap", "Wrap every stored data key under the active KEK (payloads are not decrypted)", cobra.NoArgs, true))),
		confirmed(sub("retire [ID]", "Remove a KEK; refuses while any value uses it (on a terminal, pick it from a list without ID)", oneOrPrompt, true)),
		started(confirmed(sub("reencrypt", "Seal every value again under a new data key (the compromise path)", cobra.NoArgs, true))),
		confirmed(sub("forget [ID]", "Give up on a lost KEK: mark every value it sealed as lost and start new home repositories (on a terminal, pick it from a list without ID)", oneOrPrompt, true)),
		newRepos,
		sub("export", "Print the key set as a Secret manifest, the KEK backup (redirect it into a file; restore with kubectl apply)", cobra.NoArgs, false),
	)
	return keys
}

func runKeys(ctx context.Context, o keysOptions, cmd string, rest []string, out io.Writer, prompt *cli.Prompter) error {
	var store keyscli.Store
	// These commands run in the koinon-api pod, so they are a service: with the default local KEK
	// they add keys as before; with an opted-in KMS, only the key-admin Job adds them.
	// KMS-wrapped KEKs are bound to the install ID in the database (old issue 615), and the bound-only marker
	// is there too: every command that opens them, export too, reads both there, and only when such
	// a KEK is opened. Without a database a KMS KEK stored unbound does not open (fail closed).
	var pool *pgxpool.Pool
	boundOnly := secretstore.UnreadableBoundOnly(errors.New("no --database-url to read the bound-only marker from"))
	if o.dbURL != "" {
		var err error
		if pool, err = pgxpool.New(ctx, o.dbURL); err != nil {
			return fmt.Errorf("connect to postgres: %w", err)
		}
		defer pool.Close()
		boundOnly = installid.BoundOnlySource(pool)
	}
	wrappers, err := wrapping.New(ctx, o.wrapping, wrapping.Service, boundOnly)
	if err != nil {
		return err
	}
	if pool != nil {
		wrappers = wrappers.WithInstallID(installid.Source(pool))
	}
	switch {
	case o.kekSecret != "":
		if o.namespace == "" {
			return errors.New("--namespace (or $POD_NAMESPACE) is required with --kek-secret")
		}
		client, err := keysKubeClient(o.kubeconfig)
		if err != nil {
			return err
		}
		store = keyscli.KubeStore{Client: client, Namespace: o.namespace, Name: o.kekSecret, Wrappers: wrappers}
	case o.kekDir != "":
		store = keyscli.DirStore{Dir: o.kekDir, Wrappers: wrappers}
	default:
		return errors.New("set --kek-secret or --kek-dir")
	}

	tool := &keyscli.Tool{Store: store, Out: out, ActivePeriod: o.period, Prompt: prompt, Yes: o.yes, RecordStarted: o.recordStarted, SecretName: o.kekSecret, Wrappers: wrappers}
	// activate uses the database to record the activation and to show, in its picker, the values each KEK wraps.
	// With the database, every command that changes the key set also records it in the install audit chain
	// (actor cli, "keys <command>").
	if pool != nil && cmd != "export" {
		tool.Pool = pool
	}
	if pool != nil {
		// add refuses an ID the database still carries; a missing table fails closed (not migrated yet).
		tool.UsedID = func(ctx context.Context, id string) (bool, error) { return kekIDUsedYet(ctx, pool, id) }
	}
	switch cmd {
	case "status":
		return tool.Status(ctx)
	case "add":
		return tool.Add(ctx, o.id)
	case "activate":
		return tool.Activate(ctx, firstOrEmpty(rest))
	case "rewrap":
		return tool.Rewrap(ctx)
	case "retire":
		return tool.Retire(ctx, firstOrEmpty(rest))
	case "reencrypt":
		return tool.Reencrypt(ctx)
	case "forget":
		return tool.Forget(ctx, firstOrEmpty(rest))
	case "new-repositories":
		return tool.NewRepositories(ctx, o.all, o.agents)
	case "export":
		return tool.Export(ctx)
	}
	return fmt.Errorf("unknown keys command %q", cmd)
}

func firstOrEmpty(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

func keysKubeClient(kubeconfig string) (kubernetes.Interface, error) {
	var (
		rc  *rest.Config
		err error
	)
	if kubeconfig != "" {
		rc, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	return kubernetes.NewForConfig(rc)
}
