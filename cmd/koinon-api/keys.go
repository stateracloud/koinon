// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/keyscli"
)

const keysUsage = `usage: koinon-api keys <command> [flags]

commands:
  status           list the key-encryption keys (KEKs), the active one and the values each wraps
  add [--id ID]    generate a new KEK (not active yet)
  activate ID      make a KEK the one that wraps new data keys
  rewrap           wrap every stored data key under the active KEK (payloads are not decrypted)
  retire ID        remove a KEK; refuses while any value uses it
  reencrypt        seal every value again under a new data key (the compromise path)

Run it in a koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api keys status
`

// runKeys runs "koinon-api keys".
func runKeys(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(out, keysUsage)
		return nil
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("koinon-api keys "+cmd, flag.ContinueOnError)
	var (
		dbURL, kekSecret, kekDir, namespace, kubeconfig, id string
		period                                              time.Duration
	)
	fs.StringVar(&dbURL, "database-url", "", "Postgres connection string (default $DATABASE_URL)")
	fs.StringVar(&kekSecret, "kek-secret", "", "name of the KEK Secret; the key set is read and changed through the Kubernetes API")
	fs.StringVar(&kekDir, "kek-dir", "", "mounted KEK directory, read-only; used when --kek-secret is not set")
	fs.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace of the KEK Secret")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	fs.StringVar(&id, "id", "", "add: the new KEK's ID (default kek-YYYYMMDD-xxxx)")
	fs.DurationVar(&period, "kek-active-period", keyscli.DefaultActivePeriod, "status warns when the active KEK is older than this")
	if err := cli.BindEnv(fs, nil); err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}

	var store keyscli.Store
	switch {
	case kekSecret != "":
		if namespace == "" {
			return errors.New("--namespace (or $POD_NAMESPACE) is required with --kek-secret")
		}
		client, err := keysKubeClient(kubeconfig)
		if err != nil {
			return err
		}
		store = keyscli.KubeStore{Client: client, Namespace: namespace, Name: kekSecret}
	case kekDir != "":
		store = keyscli.DirStore{Dir: kekDir}
	default:
		return errors.New("set --kek-secret or --kek-dir")
	}

	tool := &keyscli.Tool{Store: store, Out: out, ActivePeriod: period}
	needsDB := cmd == "status" || cmd == "rewrap" || cmd == "retire" || cmd == "reencrypt"
	if needsDB {
		if dbURL == "" {
			return errors.New("--database-url is required")
		}
		pool, err := pgxpool.New(ctx, dbURL)
		if err != nil {
			return fmt.Errorf("connect to postgres: %w", err)
		}
		defer pool.Close()
		tool.Pool = pool
	}
	one := func() (string, error) {
		if len(rest) != 1 {
			return "", fmt.Errorf("%s takes one KEK ID", cmd)
		}
		return rest[0], nil
	}
	switch cmd {
	case "status":
		return tool.Status(ctx)
	case "add":
		return tool.Add(ctx, id)
	case "activate":
		kid, err := one()
		if err != nil {
			return err
		}
		return tool.Activate(ctx, kid)
	case "rewrap":
		return tool.Rewrap(ctx)
	case "retire":
		kid, err := one()
		if err != nil {
			return err
		}
		return tool.Retire(ctx, kid)
	case "reencrypt":
		return tool.Reencrypt(ctx)
	}
	_, _ = io.WriteString(out, keysUsage)
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
