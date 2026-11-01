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
	"github.com/stateracloud/koinon/internal/keyscli"
)

type keysOptions struct {
	dbURL, kekSecret, kekDir, namespace, kubeconfig, id string
	period                                              time.Duration
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
	pf.StringVar(&o.dbURL, "database-url", "", "Postgres connection string (status, rewrap, retire and reencrypt)")
	pf.StringVar(&o.kekSecret, "kek-secret", "", "name of the KEK Secret; the key set is read and changed through the Kubernetes API")
	pf.StringVar(&o.kekDir, "kek-dir", "", "mounted KEK directory, read-only; used when --kek-secret is not set")
	pf.StringVar(&o.namespace, "namespace", "", "namespace of the KEK Secret")
	pf.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	pf.DurationVar(&o.period, "kek-active-period", keyscli.DefaultActivePeriod, "status warns when the active KEK is older than this")
	cli.EnvAlias(keys, "database-url", "DATABASE_URL")
	cli.EnvAlias(keys, "namespace", "POD_NAMESPACE")

	sub := func(use, short string, args cobra.PositionalArgs, needsDB bool) *cobra.Command {
		name, _, _ := strings.Cut(use, " ")
		return &cobra.Command{
			Use: use, Short: short, Args: args,
			PreRunE: func(*cobra.Command, []string) error {
				if needsDB && o.dbURL == "" {
					return errors.New("--database-url is required")
				}
				return nil
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				return runKeys(cmd.Context(), o, name, args, cmd.OutOrStdout())
			},
		}
	}
	add := sub("add", "Generate a new KEK (not active yet)", cobra.NoArgs, false)
	add.Flags().StringVar(&o.id, "id", "", "the new KEK's ID (default kek-YYYYMMDD-xxxx)")
	keys.AddCommand(
		sub("status", "List the KEKs, the active one and the values each wraps", cobra.NoArgs, true),
		add,
		sub("activate ID", "Make a KEK the one that wraps new data keys", cobra.ExactArgs(1), false),
		sub("rewrap", "Wrap every stored data key under the active KEK (payloads are not decrypted)", cobra.NoArgs, true),
		sub("retire ID", "Remove a KEK; refuses while any value uses it", cobra.ExactArgs(1), true),
		sub("reencrypt", "Seal every value again under a new data key (the compromise path)", cobra.NoArgs, true),
	)
	return keys
}

func runKeys(ctx context.Context, o keysOptions, cmd string, rest []string, out io.Writer) error {
	var store keyscli.Store
	switch {
	case o.kekSecret != "":
		if o.namespace == "" {
			return errors.New("--namespace (or $POD_NAMESPACE) is required with --kek-secret")
		}
		client, err := keysKubeClient(o.kubeconfig)
		if err != nil {
			return err
		}
		store = keyscli.KubeStore{Client: client, Namespace: o.namespace, Name: o.kekSecret}
	case o.kekDir != "":
		store = keyscli.DirStore{Dir: o.kekDir}
	default:
		return errors.New("set --kek-secret or --kek-dir")
	}

	tool := &keyscli.Tool{Store: store, Out: out, ActivePeriod: o.period}
	if o.dbURL != "" && cmd != "add" && cmd != "activate" {
		pool, err := pgxpool.New(ctx, o.dbURL)
		if err != nil {
			return fmt.Errorf("connect to postgres: %w", err)
		}
		defer pool.Close()
		tool.Pool = pool
	}
	switch cmd {
	case "status":
		return tool.Status(ctx)
	case "add":
		return tool.Add(ctx, o.id)
	case "activate":
		return tool.Activate(ctx, rest[0])
	case "rewrap":
		return tool.Rewrap(ctx)
	case "retire":
		return tool.Retire(ctx, rest[0])
	case "reencrypt":
		return tool.Reencrypt(ctx)
	}
	return fmt.Errorf("unknown keys command %q", cmd)
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
