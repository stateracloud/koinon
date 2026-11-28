// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/substrateauthz"
	"github.com/stateracloud/koinon/internal/substrateinstall"
)

// execRunner runs git, go and the ate-setup that the installer built. Its env is added to the
// environment of every child: the installer sets KUBECONFIG there for ate-setup, so no child reads the
// operator's ambient kubeconfig.
type execRunner struct{ env []string }

func (r execRunner) command(ctx context.Context, dir, name string, args, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name is git, go or the binary the installer built
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), r.env...), env...)
	return cmd
}

func (r execRunner) Run(ctx context.Context, dir, name string, args, env []string, out io.Writer) error {
	cmd := r.command(ctx, dir, name, args, env)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (r execRunner) Output(ctx context.Context, dir, name string, args []string) (string, error) {
	out, err := r.command(ctx, dir, name, args, nil).Output()
	return string(out), err
}

// newSubstrateCommand is "koinon-api substrate": the Substrate installer step (ADR 0119 D4).
func newSubstrateCommand() *cobra.Command {
	var (
		c       preflightConfig
		profile substrateinstall.Profile
		source  string
		// koinonNamespace, koinonRelease and apiTokenFile name the provisioner's ServiceAccount and the token of the
		// grant (ADR 0128).
		koinonNamespace, koinonRelease, apiTokenFile, apiCAFile string
		// platform is the value of --platform; kind is the deprecated --kind.
		platform string
		kind     bool
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
			if !c.substrateGVisor {
				return errors.New("substrate install sets the gVisor check from --sandbox-classes: --substrate-gvisor=false is only for preflight")
			}
			if c.substrateIssuer != "" {
				// The install writes the issuer ate-setup trusts; a second flag for the probe alone could disagree with it.
				return errors.New("substrate install takes the issuer from --expected-jwt-issuer: --substrate-expected-issuer is only for preflight")
			}
			if kind {
				if platform != "" && platform != string(substrateinstall.PlatformKind) {
					return errors.New("--kind means --platform kind and conflicts with --platform " + platform)
				}
				platform = string(substrateinstall.PlatformKind)
			}
			p, err := substrateinstall.ParsePlatform(platform)
			if err != nil {
				return err
			}
			profile.Platform = p
			// The router allow-list gets agentd's identity, the only Koinon pod with one (issue #1446).
			if c.substrateAgentdSA == "" {
				c.substrateAgentdNS = koinonNamespace
				c.substrateAgentdSA = substrateauthz.ComponentServiceAccount(koinonRelease, "agentd")
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
				ProvisionerNamespace: koinonNamespace, ProvisionerServiceAccount: substrateauthz.ComponentServiceAccount(koinonRelease, "provisioner"),
				APITokenFile: apiTokenFile,
				APICAFile:    apiCAFile,
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
	f.StringVar(&platform, "platform", "", "kind of cluster: generic (any Kubernetes cluster: an in-cluster object store, gvisor sandboxes, no docker on this host), gke (GCS and a Google service account) or kind (a local kind cluster). "+
		"Default: kind when every node is a kind node, else generic; gke is never detected")
	f.BoolVar(&kind, "kind", false, "deprecated: use --platform kind")
	_ = f.MarkDeprecated("kind", "use --platform kind")
	f.StringVar(&profile.GVisorMirror, "gvisor-mirror", "", "base URL of a mirror of the gVisor release "+substrateinstall.GVisorRelease+" (<base>/"+substrateinstall.GVisorRelease+"/<x86_64|aarch64>/gvisor.tar.zstd); default "+substrateinstall.GVisorDefaultMirror+". The installer host fetches the tarball and checks its pinned SHA-256")
	f.StringVar(&profile.RustFSStorageClass, "rustfs-storage-class", "", "generic platform: StorageClass of the volume of the in-cluster object store (default: the cluster default class)")
	f.StringVar(&profile.RustFSStorageSize, "rustfs-storage-size", "", "generic platform: size of the object-store volume (default "+substrateinstall.DefaultRustFSStorageSize+")")
	f.StringVar(&profile.PostgresStorageSize, "postgres-storage-size", "", "generic platform: size of the bundled PostgreSQL volume (default "+substrateinstall.DefaultPostgresStorageSize+"); its class is --postgres-storage-class")
	f.StringVar(&profile.GCPProjectID, "gcp-project-id", "", "gke platform: Google Cloud project (ate-setup --gcp-project-id); default PROJECT_ID of the environment")
	f.StringVar(&profile.SnapshotTransferGSAName, "snapshot-transfer-gsa-name", "", "gke platform: Google service account that snapshot transfers run as (ate-setup --snapshot-transfer-gsa-name); default SNAPSHOT_TRANSFER_GSA_NAME of the environment")
	f.BoolVar(&profile.EgressDNSV4Preferred, "egress-dns-v4-preferred", false, "resolve upstream names IPv4-first in the egress gateway (dns_lookup_family V4_PREFERRED instead of ALL). Use it on a cluster without an IPv6 route, where calls can fail at random with 503 \"Network is unreachable\"")
	f.StringVar(&koinonNamespace, "koinon-namespace", "koinon", "namespace of the Koinon release: the installer makes its provisioner an owner in Substrate's global access policy (ADR 0128) and puts agentd's SPIFFE ID (this namespace, <release>-agentd) on the atenet router's allow-list (issue #1446)")
	f.StringVar(&koinonRelease, "koinon-release", "koinon", "name of the Koinon Helm release, which names the ServiceAccounts of the provisioner (<release>-provisioner, or <release>-koinon-provisioner when the release name has no \"koinon\" in it) and of agentd, whose SPIFFE ID goes on the atenet router's allow-list (issue #1446)")
	f.StringVar(&apiCAFile, "api-ca-file", "", "PEM CA that verifies ate-api-server's certificate; default is the ClusterTrustBundle of Substrate's servicedns signer")
	f.StringVar(&apiTokenFile, "api-token-file", "", "token of a principal that holds the global owner role in Substrate, for an install whose bootstrap owners are not ate-client; default is a token minted for ate-client")
	mustRequire(install, "substrate-source")
	mustRequire(install, "kubeconfig")
	mustRequire(install, "context")
	mustRequire(install, "storage-bucket")
	cmd.AddCommand(install)
	return cmd
}
