// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/substrateauthz"
)

// provisionConfig is the setting of "koinon-reconciler provision" (ADR 0128).
type provisionConfig struct {
	listen, kubeconfig, audience                  string
	controlAddress, caFile, serverName, tokenFile string
	callers, editors, viewers                     []string
}

// newProvisionCommand is the provisioner: the one Koinon component with the global owner role in Substrate. It
// has no database, no gateway and no key; it only creates org atespaces for the reconciler.
func newProvisionCommand() *cobra.Command {
	var c provisionConfig
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Create the Substrate atespace of an org and write its access policy for the reconciler (ADR 0128)",
		Args:  cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.ValidateRequiredFlags()
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return provision(cmd.Context(), c) },
	}
	fs := cmd.Flags()
	fs.StringVar(&c.listen, "listen", ":8086", "address of the HTTP interface (POST /v1/atespaces, GET /healthz)")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig for the TokenReview client; empty uses the pod's ServiceAccount")
	fs.StringVar(&c.audience, "token-audience", "koinon-provisioner", "audience of the ServiceAccount tokens that callers send")
	fs.StringVar(&c.controlAddress, "substrate-control-address", "", "Agent Substrate's control API (ateapi) host:port")
	fs.StringVar(&c.caFile, "substrate-ca-file", "", "CA file that signed ateapi's certificate; empty uses the system roots")
	fs.StringVar(&c.serverName, "substrate-server-name", "", "name checked against ateapi's certificate")
	fs.StringVar(&c.tokenFile, "substrate-token-file", "", "projected ServiceAccount token (ateapi audience) of the provisioner")
	fs.StringSliceVar(&c.callers, "caller", nil, "username (system:serviceaccount:<ns>:<name>) that may ask; the reconciler")
	fs.StringSliceVar(&c.editors, "editor", nil, "policy member (user:system:serviceaccount:<ns>:<name>) that gets editor in each org atespace")
	fs.StringSliceVar(&c.viewers, "viewer", nil, "policy member that gets viewer in each org atespace")
	for _, n := range []string{"substrate-control-address", "substrate-token-file", "caller", "editor"} {
		if err := cmd.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("%s: %v", cmd.Name(), err))
		}
	}
	return cmd
}

func provision(ctx context.Context, c provisionConfig) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	kube, err := kubeClient(c.kubeconfig)
	if err != nil {
		return err
	}
	ctl, closer, err := substrate.DialControl(substrate.Endpoint{
		ControlAddr: c.controlAddress, CAFile: c.caFile, ServerName: c.serverName, BearerTokenFile: c.tokenFile,
	})
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	srv := &substrateauthz.Server{
		Ensurer: &substrateauthz.Provisioner{Control: ctl, Editors: c.editors, Viewers: c.viewers},
		Auth:    substrateauthz.TokenReviewer{Kube: kube, Audience: c.audience},
		Callers: c.callers,
		Log:     log,
	}
	hs := &http.Server{Addr: c.listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("koinon-reconciler provision: serving", "addr", c.listen, "callers", c.callers)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
