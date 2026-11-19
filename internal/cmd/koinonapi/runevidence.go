// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/runevidence"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
	"github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// newRunEvidenceCommand is "koinon-api run-evidence" (lab run evidence, #926).
func newRunEvidenceCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "run-evidence",
		Short: "Export one run's evidence for a restore drill",
		Long: "On a running install use the chart's run-evidence Job (runEvidence.export in the chart values): it needs no exec into a pod. " +
			"Elsewhere run it with --kek-dir (a mounted key-encryption keys Secret) or --kek-secret (read through the Kubernetes API).\n\n" +
			"The export is JSON Lines: the run (with its sandbox enforcement mode), its traces (one per turn), its events, the leak " +
			"guard's decisions and the outbound bytes per external sink. It carries no argument values, no secrets and no host names.",
	}

	var dbURL, org, run, out string
	var s3 checkpoint.S3
	var kekDir, kekSecret, namespace, kubeconfig string
	var wrapCfg wrapping.Config
	var noOrgSecrets bool
	export := &cobra.Command{
		Use:   "export",
		Short: "Write a run's evidence to a directory or an s3://bucket/prefix",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			orgID, err := parseUUID("--org", org)
			if err != nil {
				return err
			}
			runID, err := parseUUID("--run", run)
			if err != nil {
				return err
			}
			sink, err := runevidence.ParsePrefix(out, s3)
			if err != nil {
				return err
			}
			if dbURL == "" {
				return errors.New(`required flag(s) "database-url" not set`)
			}
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			hosts, err := runevidence.NewHostMasker()
			if err != nil {
				return err
			}
			secrets, err := exportSecrets(cmd.Context(), pool, orgID, kekSource{dir: kekDir, secret: kekSecret, namespace: namespace, kubeconfig: kubeconfig}, wrapCfg, noOrgSecrets)
			if err != nil {
				return err
			}
			e := runevidence.Exporter{Q: store.New(pool), Scrubber: runevidence.NewScrubber(secrets), Hosts: hosts}
			sum, err := e.Export(cmd.Context(), orgID, runID, sink)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "exported %+v to %s\n", sum, out)
			return err
		},
	}
	export.Flags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	export.Flags().StringVar(&org, "org", "", "the run's org ID")
	export.Flags().StringVar(&run, "run", "", "the run's ID")
	export.Flags().StringVar(&out, "out", "", "a directory, or s3://bucket/prefix (one prefix per export)")
	export.Flags().StringVar(&s3.Endpoint, "s3-endpoint", "", "S3 endpoint (host:port or URL) for an s3:// --out")
	export.Flags().StringVar(&s3.Region, "s3-region", "", "S3 region")
	export.Flags().StringVar(&s3.AccessKeyID, "s3-access-key-id", "", "S3 access key ID")
	export.Flags().StringVar(&s3.SecretAccessKey, "s3-secret-access-key", "", "S3 secret access key (use the environment, not the command line)")
	export.Flags().StringVar(&kekDir, "kek-dir", "", "directory with the mounted key-encryption keys Secret, as koinon-api reads it; the export scrubs the org's stored credentials")
	export.Flags().StringVar(&kekSecret, "kek-secret", "", "name of the key-encryption keys Secret; the export reads it through the Kubernetes API (the chart's run-evidence Job)")
	export.Flags().StringVar(&namespace, "namespace", "", "namespace of the --kek-secret Secret")
	export.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	export.Flags().BoolVar(&noOrgSecrets, "no-org-secrets", false, "export without the org's stored credentials in the scrubber (only for an org that stores none)")
	wrapCfg.Flags(export.Flags())
	cli.EnvAlias(export, "database-url", "DATABASE_URL")
	cli.EnvAlias(export, "namespace", "POD_NAMESPACE")
	cli.EnvAlias(export, "s3-access-key-id", "AWS_ACCESS_KEY_ID")
	cli.EnvAlias(export, "s3-secret-access-key", "AWS_SECRET_ACCESS_KEY")
	_ = export.MarkFlagRequired("org")
	_ = export.MarkFlagRequired("run")
	_ = export.MarkFlagRequired("out")

	var dir, declarations string
	toCorpus := &cobra.Command{
		Use:   "to-corpus",
		Short: "Convert an export directory into leak guard replay corpus lines on stdout (#51)",
		Long: "Reads run_events.jsonl and decisions.jsonl of an export directory and writes corpus lines (the format at the top of " +
			"internal/mcpguard/corpus_test.go). --declarations is a file of org, server and tool lines from the org's admin API; a " +
			"server it does not declare is written with no legs and named on stderr.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ev, err := os.Open(filepath.Join(dir, runevidence.FileEvents)) //nolint:gosec // the operator's own path
			if err != nil {
				return err
			}
			defer func() { _ = ev.Close() }()
			dec, err := os.Open(filepath.Join(dir, runevidence.FileDecisions)) //nolint:gosec // the operator's own path
			if err != nil {
				return err
			}
			defer func() { _ = dec.Close() }()
			d, ok := harness.Lookup(harness.OpenCode)
			if !ok {
				return errors.New("the OpenCode harness is not known")
			}
			opts := runevidence.CorpusOptions{MCPToolPrefix: d.MCPToolPrefix}
			if declarations != "" {
				b, err := os.ReadFile(declarations) //nolint:gosec // the operator's own path
				if err != nil {
					return err
				}
				for _, l := range bytes.Split(b, []byte("\n")) {
					if len(bytes.TrimSpace(l)) > 0 {
						opts.Declarations = append(opts.Declarations, json.RawMessage(l))
					}
				}
			}
			rep, err := runevidence.Corpus(ev, dec, cmd.OutOrStdout(), opts)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "calls=%d feeds=%d unpaired_calls=%d unpaired_decisions=%d undeclared_servers=%v\n",
				rep.Calls, rep.Feeds, rep.UnpairedCalls, rep.UnpairedDecisions, rep.UndeclaredServers)
			return err
		},
	}
	toCorpus.Flags().StringVar(&dir, "dir", "", "an export directory")
	toCorpus.Flags().StringVar(&declarations, "declarations", "", "a file of org, server and tool corpus lines")
	_ = toCorpus.MarkFlagRequired("dir")

	var deny []string
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Check an export directory for host names the operator lists",
		Long: "Exits non-zero when any file of the export holds one of the --deny strings (the lab's host names, domains and IP addresses). " +
			"Run gitleaks over the same directory for secrets.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var bad []string
			for _, f := range runevidence.Files {
				b, err := os.ReadFile(filepath.Join(dir, f)) //nolint:gosec // the operator's own path
				if err != nil {
					return err
				}
				for _, d := range deny {
					if d != "" && strings.Contains(strings.ToLower(string(b)), strings.ToLower(d)) {
						bad = append(bad, f+" holds "+d)
					}
				}
			}
			if len(bad) > 0 {
				return fmt.Errorf("the export names hosts: %s", strings.Join(bad, "; "))
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "ok: none of the listed names is in the export")
			return err
		},
	}
	verify.Flags().StringVar(&dir, "dir", "", "an export directory")
	verify.Flags().StringSliceVar(&deny, "deny", nil, "a host name, domain or address that must not appear (repeatable)")
	_ = verify.MarkFlagRequired("dir")

	root.AddCommand(export, toCorpus, verify)
	return root
}

func parseUUID(flag, v string) (pgtype.UUID, error) {
	u, err := uuid.Parse(v)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s: %w", flag, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

// kekSource says where the export reads the key-encryption keys: a mounted directory or, in the
// chart's run-evidence Job, the Secret itself through the Kubernetes API (read-only, as key-admin
// reads it).
type kekSource struct {
	dir, secret, namespace, kubeconfig string
	// client is the Kubernetes client of --kek-secret; nil builds one from kubeconfig or the pod. Tests set it.
	client kubernetes.Interface
}

// exportSecrets is the org's secret set for the export's scrubber, built as the trace router builds
// it (router.PGSecrets: the org's secret store and provider records, opened with the KEKs). It fails
// when a stored envelope cannot be opened: the set would miss a value, and an export that cannot
// remove a credential it cannot read must not be written. Without --kek-dir or --kek-secret the
// export refuses unless --no-org-secrets says the org stores none.
func exportSecrets(ctx context.Context, pool *pgxpool.Pool, org pgtype.UUID, kek kekSource, wc wrapping.Config, none bool) (*scrub.Secrets, error) {
	if kek.dir != "" && kek.secret != "" {
		return nil, errors.New("set --kek-dir or --kek-secret, not both")
	}
	if kek.dir == "" && kek.secret == "" {
		if !none {
			return nil, errors.New("--kek-dir or --kek-secret is required, so the export can scrub the org's stored credentials (or --no-org-secrets for an org that stores none)")
		}
		return scrub.NewSecrets(), nil
	}
	wrappers, err := wrapping.New(ctx, wc, wrapping.Service, installid.BoundOnlySource(pool))
	if err != nil {
		return nil, err
	}
	keyring, err := loadKEKs(ctx, kek, wrappers.WithInstallID(installid.Source(pool)))
	if err != nil {
		return nil, err
	}
	return router.NewPGSecrets(pool, keyring, slog.Default()).Secrets(ctx, org)
}

// loadKEKs opens the key-encryption keys from the mounted directory or, with --kek-secret, from the
// Secret through the Kubernetes API. It only reads.
func loadKEKs(ctx context.Context, kek kekSource, wrappers secretstore.Wrappers) (*secretstore.Keyring, error) {
	if kek.secret == "" {
		ring, err := secretstore.LoadDir(ctx, kek.dir, wrappers)
		if err != nil {
			return nil, fmt.Errorf("load the key-encryption keys: %w", err)
		}
		return ring, nil
	}
	if kek.namespace == "" {
		return nil, errors.New("--namespace (or $POD_NAMESPACE) is required with --kek-secret")
	}
	client := kek.client
	if client == nil {
		var err error
		if client, err = keysKubeClient(kek.kubeconfig); err != nil {
			return nil, err
		}
	}
	ks, err := keyscli.KubeStore{Client: client, Namespace: kek.namespace, Name: kek.secret, Wrappers: wrappers}.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the key-encryption keys: %w", err)
	}
	ring, err := secretstore.NewKeyring(ks.Keys, ks.Active)
	if err != nil {
		return nil, fmt.Errorf("load the key-encryption keys: %w", err)
	}
	return ring, nil
}
