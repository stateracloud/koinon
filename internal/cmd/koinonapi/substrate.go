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

func (execRunner) Run(ctx context.Context, dir, name string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the operator names the pinned binary
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (execRunner) Output(ctx context.Context, dir, name string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the operator names the pinned binary
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// newSubstrateCommand is "koinon-api substrate": the Substrate installer step (ADR 0119 D4).
func newSubstrateCommand() *cobra.Command {
	var (
		c       preflightConfig
		profile substrateinstall.Profile
		setup   string
		source  string
	)
	cmd := &cobra.Command{Use: "substrate", Short: "Install Agent Substrate, Koinon's execution layer (ADR 0119)"}
	install := &cobra.Command{
		Use:   "install",
		Short: "Check the prerequisites, run the pinned ate-setup with the Koinon profile, then run the preflight",
		Long: "Checks that ate-setup and its source checkout are the pinned release, then runs the Substrate prerequisite preflight (Kubernetes 1.37 or later, the node devices of the vm tier, a " +
			"service-account issuer that pods can reach) and stops before it installs anything when one fails. " +
			"Then it runs ate-setup deploy ate-system (" + substrateinstall.PinnedVersion + ", pre-built images) with the Koinon profile, waits for the CRDs, " +
			"lets the router accept the atenet client and runs the full preflight. Run it before helm install " +
			"with execution.driver: substrate.",
		Args: cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			c.driver = "substrate"
			// The release namespace may not exist before helm install and the installer creates nothing
			// there, so the NetworkPolicy probe (a pod pair in that namespace) is left to the chart's preflight.
			c.netpolProbe = false
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
				SetupBinary: setup, SourceDir: source, Kubeconfig: c.kubeconfig, Profile: profile, Out: cmd.OutOrStdout(),
			}).Install(cmd.Context())
		},
	}
	c.flags(install.Flags())
	f := install.Flags()
	f.StringVar(&setup, "ate-setup", "", "path of ate-setup built from the source checkout ("+substrateinstall.PinnedVersion+")")
	f.StringVar(&source, "substrate-source", "", "clean checkout of the Substrate release tag "+substrateinstall.PinnedVersion+" (ate-setup applies its manifests)")
	f.StringVar(&profile.StorageBucket, "storage-bucket", "", "object-storage bucket for snapshots")
	f.StringVar(&profile.SandboxClasses, "sandbox-classes", "", "ate-setup --sandbox-classes (default gvisor,microvm)")
	f.StringVar(&profile.ImageRepo, "image-repo", "", "registry path of the Substrate release images (default the pinned public path)")
	mustRequire(install, "ate-setup")
	mustRequire(install, "substrate-source")
	mustRequire(install, "storage-bucket")
	cmd.AddCommand(install)
	return cmd
}
