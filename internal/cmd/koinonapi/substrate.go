// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/substrateinstall"
)

// execRunner runs git, go and the ate-setup that the installer built. Its env is added to the
// environment of every child: the installer sets KUBECONFIG there for ate-setup, so no child reads the
// operator's ambient kubeconfig.
type execRunner struct{ env []string }

func (r execRunner) command(ctx context.Context, dir, name string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name is git, go or the binary the installer built
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), r.env...)
	return cmd
}

func (r execRunner) Run(ctx context.Context, dir, name string, args []string, out io.Writer) error {
	cmd := r.command(ctx, dir, name, args)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (r execRunner) Output(ctx context.Context, dir, name string, args []string) (string, error) {
	out, err := r.command(ctx, dir, name, args).Output()
	return string(out), err
}

// newSubstrateCommand is "koinon-api substrate": the Substrate installer step (ADR 0119 D4).
func newSubstrateCommand() *cobra.Command {
	var (
		c       preflightConfig
		profile substrateinstall.Profile
		source  string
	)
	cmd := &cobra.Command{Use: "substrate", Short: "Install Agent Substrate, Koinon's execution layer (ADR 0119)"}
	install := &cobra.Command{
		Use:   "install",
		Short: "Build the pinned ate-setup, check the prerequisites, run it with the Koinon profile, then run the preflight",
		Long: "Exports the pinned Substrate commit from a git repository, builds ate-setup from that export, then runs the Substrate prerequisite preflight (Kubernetes 1.37 or later, the node devices of the vm tier, a " +
			"service-account issuer that Substrate's api server can validate from a pod) and stops before it installs anything when one fails. " +
			"Then it runs ate-setup deploy ate-system (" + substrateinstall.PinnedVersion + ", pre-built images) with the Koinon profile, waits for the CRDs, " +
			"lets the router accept the atenet client and runs the full preflight. Run it before helm install " +
			"with execution.driver: substrate.",
		Args: cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			c.driver = "substrate"
			// The release namespace may not exist before helm install and the installer creates nothing
			// there, so the NetworkPolicy probe (a pod pair in that namespace) is left to the chart's preflight.
			c.netpolProbe = false
			if c.kubeconfig == "" || c.kubeContext == "" {
				return errors.New("substrate install needs --kubeconfig and --context: it does not act on the ambient current context")
			}
			if !c.substrateProbes {
				return errors.New("substrate install runs the probe pods: --substrate-probes=false is only for preflight")
			}
			return c.validate()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			checker, kube, closeGateway, err := buildChecker(c)
			if err != nil {
				return err
			}
			defer closeGateway()
			return (&substrateinstall.Installer{
				Kube: kube, Dynamic: checker.Dynamic, Checker: checker, Runner: execRunner{env: []string{"KUBECONFIG=" + c.kubeconfig}},
				SourceRepo: source, Kubeconfig: c.kubeconfig, Context: c.kubeContext, Profile: profile, Out: cmd.OutOrStdout(),
			}).Install(cmd.Context())
		},
	}
	c.flags(install.Flags())
	f := install.Flags()
	f.StringVar(&source, "substrate-source", "", "git repository (a clone of Substrate) that holds the pinned commit "+substrateinstall.PinnedCommit+" ("+substrateinstall.PinnedVersion+"); the installer exports it and builds ate-setup from the export")
	f.StringVar(&profile.StorageBucket, "storage-bucket", "", "object-storage bucket for snapshots")
	f.StringVar(&profile.SandboxClasses, "sandbox-classes", "", "ate-setup --sandbox-classes (default gvisor,microvm)")
	f.StringVar(&profile.ImageRepo, "image-repo", "", "registry path of the Substrate release images (default the pinned public path)")
	f.StringVar(&profile.PostgresStorageClass, "postgres-storage-class", "", "StorageClass of the bundled PostgreSQL volume (ate-setup --ateapi-postgres-storage-class); needed on a cluster without a default StorageClass")
	f.StringVar(&profile.SetupCSI, "setup-csi", "", "ate-setup --setup-csi: also install a CSI driver (nfs, hostpath, both or none; default none)")
	f.StringVar(&profile.ExpectedJWTIssuer, "expected-jwt-issuer", "", "service-account issuer Substrate's api server trusts (ate-setup --ateapi-expected-jwt-issuer); default is the issuer in the API server's discovery document. "+
		"Only an override: it must equal the iss of the cluster's tokens and does not make an unroutable issuer reachable")
	f.BoolVar(&profile.Kind, "kind", false, "target a local kind cluster (ate-setup --kind)")
	mustRequire(install, "substrate-source")
	mustRequire(install, "kubeconfig")
	mustRequire(install, "context")
	mustRequire(install, "storage-bucket")
	cmd.AddCommand(install)
	return cmd
}
