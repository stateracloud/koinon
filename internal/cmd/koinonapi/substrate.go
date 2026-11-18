// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"io"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/substrateinstall"
)

// execRunner runs the pinned ate-setup binary.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the operator names the pinned binary
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// newSubstrateCommand is "koinon-api substrate": the Substrate installer step (ADR 0119 D4).
func newSubstrateCommand() *cobra.Command {
	var (
		c       preflightConfig
		profile substrateinstall.Profile
		setup   string
	)
	cmd := &cobra.Command{Use: "substrate", Short: "Install Agent Substrate, Koinon's execution layer (ADR 0119)"}
	install := &cobra.Command{
		Use:   "install",
		Short: "Check the prerequisites, run the pinned ate-setup with the Koinon profile, then run the preflight",
		Long: "Runs the Substrate prerequisite preflight (Kubernetes 1.37 or later, the node devices of the vm tier, a " +
			"service-account issuer that pods can reach) and stops before it installs anything when one fails. " +
			"Then it runs ate-setup " + substrateinstall.PinnedVersion + " with the Koinon profile, waits for the CRDs, " +
			"lets the router accept the atenet client and runs the full preflight. Run it before helm install " +
			"with execution.driver: substrate.",
		Args: cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			c.driver = "substrate"
			return c.validate()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			checker, kube, closeGateway, err := buildChecker(c)
			if err != nil {
				return err
			}
			defer closeGateway()
			return (&substrateinstall.Installer{
				Kube: kube, Dynamic: checker.Dynamic, Checker: checker, Runner: execRunner{},
				SetupBinary: setup, Profile: profile, Out: cmd.OutOrStdout(),
			}).Install(cmd.Context())
		},
	}
	c.flags(install.Flags())
	f := install.Flags()
	f.StringVar(&setup, "ate-setup", "", "path of the pinned ate-setup binary ("+substrateinstall.PinnedVersion+")")
	f.StringVar(&profile.ObjectStoreEndpoint, "object-store-endpoint", "", "S3-compatible object store endpoint for snapshots")
	f.StringVar(&profile.ObjectStoreBucket, "object-store-bucket", "", "object store bucket")
	f.StringVar(&profile.ObjectStoreSecret, "object-store-secret", "", "Secret with the object store credential")
	f.StringVar(&profile.DatabaseSecret, "database-secret", "", "Secret with the connection string of ateapi's Postgres database")
	mustRequire(install, "ate-setup")
	cmd.AddCommand(install)
	return cmd
}
