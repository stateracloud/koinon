// SPDX-License-Identifier: Apache-2.0

package koinongateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

// keysOptions are the flags of every "keys" command: the database, the Secret and the wrapping key.
type keysOptions struct {
	dbURL      string
	secret     string
	namespace  string
	kubeconfig string
	replicas   int
	wait       time.Duration
	waitLoaded time.Duration
	grace      time.Duration
	wrapping   wrapping.Config
}

// newKeysCommand returns "koinon-gateway keys": the lifecycle of the gateway's recipient key (ADR 0097
// decision 12). The chart runs it as the gateway key-admin Job, with its own identity: the only one that
// may encrypt with the gateway's wrapping key, write the Secret of wrapped keys and change the key table.
func newKeysCommand() *cobra.Command {
	o := &keysOptions{}
	root := &cobra.Command{
		Use:   "keys",
		Short: "The gateway's recipient key: init, rotate, activate, retire (the gateway key-admin Job)",
		Long: "The gateway's recipient key is the key koinon-api seals a stored MCP credential to (an HPKE envelope, ADR 0097 decision 12). " +
			"The private key is wrapped by the gateway's own wrapping key (an OpenBao transit key or an AWS KMS key; on the local backend the Secret holds it unwrapped) " +
			"and stored in a Secret that only the gateway mounts. The public key is a row of gateway_recipient_keys.\n\n" +
			"A key moves pending, active, retiring, retired. A key becomes active only after every gateway process reported it loaded.",
		Args: cobra.NoArgs,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&o.dbURL, "database-url", "", "Postgres connection string of the key-admin login (role "+"koinon_gateway_key_admin"+")")
	pf.StringVar(&o.secret, "secret", "", "name of the Secret that holds the wrapped private keys")
	pf.StringVar(&o.namespace, "namespace", "", "namespace of that Secret")
	pf.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	pf.IntVar(&o.replicas, "replicas", 1, "how many gateway processes must report a pending key loaded before it becomes active")
	pf.DurationVar(&o.wait, "wait", 5*time.Minute, "how long to wait for the database to come up and be migrated")
	o.wrapping.Flags(pf)

	cmds := []*cobra.Command{
		{
			Use:   "init",
			Short: "Make the first key, when there is none, and activate it once the gateways loaded it",
			Long: "Idempotent: with a key already there it only finishes a pending activation. With --wait-loaded it waits for the gateways to load the key; " +
				"without a wait that ends in time it leaves the key pending and says so (run \"keys activate\" later).",
			RunE: func(cmd *cobra.Command, _ []string) error { return o.run(cmd, "init") },
		},
		{
			Use:   "rotate",
			Short: "Add a new key (pending) and activate it once the gateways loaded it; the old key becomes retiring",
			RunE:  func(cmd *cobra.Command, _ []string) error { return o.run(cmd, "rotate") },
		},
		{
			Use:   "activate",
			Short: "Activate the pending key if every gateway process loaded it",
			RunE:  func(cmd *cobra.Command, _ []string) error { return o.run(cmd, "activate") },
		},
		{
			Use:   "retire",
			Short: "Retire each retiring key whose wait is over and that no stored credential names; delete its wrapped private key",
			RunE:  func(cmd *cobra.Command, _ []string) error { return o.run(cmd, "retire") },
		},
		{
			Use:   "status",
			Short: "List the keys, their states and which gateway processes loaded them",
			RunE:  func(cmd *cobra.Command, _ []string) error { return o.run(cmd, "status") },
		},
	}
	for _, c := range cmds {
		c.Args = cobra.NoArgs
		root.AddCommand(c)
	}
	for _, name := range []string{"init", "rotate"} {
		c, _, _ := root.Find([]string{name})
		c.Flags().DurationVar(&o.waitLoaded, "wait-loaded", 0, "how long to wait for every gateway process to load the new key before it is activated; 0 does not wait")
	}
	rc, _, _ := root.Find([]string{"retire"})
	rc.Flags().DurationVar(&o.grace, "grace", gatewaykeys.DefaultRetireGrace, "the wait between a key leaving active and its retirement; it must exceed koinon-api's reload interval")
	cli.EnvAlias(root, "database-url", "DATABASE_URL")
	cli.EnvAlias(root, "namespace", "POD_NAMESPACE")
	root.AddCommand(o.resealCommand())
	return root
}

// resealCommand is "keys reseal": the re-seal Job of a rotation (ADR 0097 12.4). It runs under the gateway's
// identity (only the gateway unwraps its keys) and the re-seal database login, which may read and write only
// the credential column.
func (o *keysOptions) resealCommand() *cobra.Command {
	var keyDir, org string
	var dryRun bool
	c := &cobra.Command{
		Use:   "reseal",
		Short: "Seal every stored MCP credential that names an older gateway key to the active key",
		Long: "Idempotent and resumable: a credential already sealed to the active key is left alone. Each credential is one write that " +
			"changes the row only if it is still the envelope that was read, so a credential the API writes meanwhile is not lost; a rerun moves it. " +
			"When the report shows no credential on the old key, \"keys retire\" retires that key. The report holds counts and key IDs, never a value.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.reseal(cmd, keyDir, org, dryRun)
		},
	}
	c.Flags().StringVar(&keyDir, "recipient-key-dir", "", "the mounted Secret with the wrapped gateway recipient keys")
	c.Flags().StringVar(&org, "org", "", "re-seal only the credentials of this org (a UUID)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "open every credential that would move and report the counts; write nothing")
	return c
}

func (o *keysOptions) reseal(cmd *cobra.Command, keyDir, org string, dryRun bool) error {
	if o.dbURL == "" {
		return errors.New("--database-url is required")
	}
	if keyDir == "" {
		return errors.New("--recipient-key-dir is required")
	}
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	pool, err := waitForPool(ctx, o.dbURL, o.wait)
	if err != nil {
		return err
	}
	defer pool.Close()
	waitCtx, cancel := context.WithTimeout(ctx, o.wait)
	install, err := installid.Wait(waitCtx, pool, 2*time.Second)
	cancel()
	if err != nil {
		return err
	}
	ws, err := wrapping.New(ctx, o.wrapping, wrapping.Service, installid.BoundOnlySource(pool))
	if err != nil {
		return fmt.Errorf("the gateway's key-wrapping key: %w", err)
	}
	l := gatewaykeys.NewLoader(keyDir, pool, ws.WithInstallID(secretstore.StaticInstallID(install)), "reseal", nil)
	l.NoReport = true
	if err := l.Load(ctx); err != nil {
		return fmt.Errorf("load the gateway keys: %w", err)
	}
	rep, err := gatewaykeys.Resealer{DB: pool, Keyring: l.Keyring, Org: org, DryRun: dryRun}.Run(ctx)
	printReseal(out, rep, dryRun)
	return err
}

func printReseal(out io.Writer, rep gatewaykeys.ResealReport, dryRun bool) {
	if dryRun {
		_, _ = fmt.Fprintf(out, "Dry run: %d MCP credential(s) would move to gateway key %s. Nothing was written.\n", rep.WouldMove, rep.KeyID)
	} else {
		_, _ = fmt.Fprintf(out, "%d MCP credential(s) moved to gateway key %s; %d skipped (changed during the run, run it again); %d failed.\n",
			rep.Moved, rep.KeyID, rep.Skipped, rep.Failed)
	}
	from := make([]string, 0, len(rep.From))
	for id := range rep.From {
		from = append(from, id)
	}
	sort.Strings(from)
	for _, id := range from {
		_, _ = fmt.Fprintf(out, "  from %s: %d\n", id, rep.From[id])
	}
	kinds := make([]string, 0, len(rep.Failures))
	for k := range rep.Failures {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		_, _ = fmt.Fprintf(out, "  failed (%s): %d\n", k, rep.Failures[k])
	}
}

func (o *keysOptions) run(cmd *cobra.Command, what string) error {
	switch {
	case o.dbURL == "":
		return errors.New("--database-url is required")
	case o.secret == "" || o.namespace == "":
		return errors.New("--secret and --namespace are required")
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	pool, err := waitForPool(ctx, o.dbURL, o.wait)
	if err != nil {
		return err
	}
	defer pool.Close()
	waitCtx, cancel := context.WithTimeout(ctx, o.wait)
	install, err := installid.Wait(waitCtx, pool, 2*time.Second)
	cancel()
	if err != nil {
		return err
	}
	ws, err := wrapping.New(ctx, o.wrapping, wrapping.KeyAdmin, installid.BoundOnlySource(pool))
	if err != nil {
		return err
	}
	ws = ws.WithInstallID(secretstore.StaticInstallID(install))
	rc, err := o.restConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}
	a := gatewaykeys.Admin{
		DB:       pool,
		Secrets:  gatewaykeys.SecretStore{Client: client, Namespace: o.namespace, Name: o.secret},
		Wrap:     ws,
		Replicas: o.replicas,
		Grace:    o.grace,
		Logf:     func(f string, args ...any) { _, _ = fmt.Fprintf(out, f+"\n", args...) },
	}
	switch what {
	case "init", "rotate":
		return o.addAndActivate(ctx, a, what == "rotate", out)
	case "activate":
		id, err := a.Activate(ctx)
		return reportActivation(out, id, err)
	case "retire":
		retired, kept, err := a.Retire(ctx)
		for _, id := range retired {
			_, _ = fmt.Fprintf(out, "gateway key %s is retired and its wrapped private key is deleted\n", id)
		}
		for id, why := range kept {
			_, _ = fmt.Fprintf(out, "gateway key %s stays retiring: %s\n", id, why)
		}
		return err
	default:
		return status(ctx, pool, out)
	}
}

// addAndActivate is init and rotate. Init adds a key only when the table has none; rotate adds one
// when no key is pending.
func (o *keysOptions) addAndActivate(ctx context.Context, a gatewaykeys.Admin, rotate bool, out io.Writer) error {
	rows, err := gatewaykeys.List(ctx, a.DB)
	if err != nil {
		return err
	}
	if len(rows) == 0 || rotate {
		if _, err := a.Add(ctx); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(o.waitLoaded)
	for {
		id, err := a.Activate(ctx)
		if errors.Is(err, gatewaykeys.ErrNoPending) {
			_, _ = fmt.Fprintln(out, "no pending gateway key")
			return nil
		}
		if errors.Is(err, gatewaykeys.ErrNotLoaded) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				continue
			}
		}
		if errors.Is(err, gatewaykeys.ErrNotLoaded) && o.waitLoaded > 0 {
			// The Job was told to wait for the gateways and they did not load the key in time: fail the
			// Job, so the person sees it. The key stays pending; nothing seals to it.
			return fmt.Errorf("gateway key %s stays pending after %s: %w", id, o.waitLoaded, err)
		}
		return reportActivation(out, id, err)
	}
}

func reportActivation(out io.Writer, id string, err error) error {
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(out, "gateway key %s is active\n", id)
		return nil
	case errors.Is(err, gatewaykeys.ErrNoPending):
		_, _ = fmt.Fprintln(out, "no pending gateway key")
		return nil
	case errors.Is(err, gatewaykeys.ErrNotLoaded):
		// Not a failure of the Job: the gateways load a key within a minute of the Secret changing. Run
		// "keys activate" again; the key stays pending and nothing seals to it until then.
		_, _ = fmt.Fprintf(out, "gateway key %s stays pending: %v\n", id, err)
		return nil
	default:
		return err
	}
}

func status(ctx context.Context, pool *pgxpool.Pool, out io.Writer) error {
	rows, err := gatewaykeys.List(ctx, pool)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no gateway keys")
	}
	for _, r := range rows {
		by, err := gatewaykeys.LoadedBy(ctx, pool, r.KeyID, gatewaykeys.DefaultLoadWindow)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "%s %s %s loaded-by=%d\n", r.KeyID, r.Recipient, r.State, len(by))
	}
	return nil
}

func (o *keysOptions) restConfig() (*rest.Config, error) {
	if o.kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", o.kubeconfig)
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes client config: %w", err)
	}
	return rc, nil
}

// waitForPool connects to the database and waits up to wait for it to answer.
func waitForPool(ctx context.Context, dsn string, wait time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, errors.New("the connection string does not parse")
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if err = pool.Ping(waitCtx); err == nil {
			return pool, nil
		}
		select {
		case <-waitCtx.Done():
			pool.Close()
			return nil, fmt.Errorf("connect to postgres: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}
