// SPDX-License-Identifier: Apache-2.0

// Package substrateinstall is Koinon's installer step for Agent Substrate (ADR 0119 D4, issue #1089).
// Substrate is a pinned cluster prerequisite, not part of the chart: its namespaces are fixed, so it is a
// cluster singleton, and its set-up is imperative (a CA, a Postgres schema, CRDs and then objects that
// need those CRDs), which a chart cannot render deterministically. The step does these things in order:
// it exports the pinned commit from a Substrate git repository and builds ate-setup from that export
// (the installer trusts no operator-supplied binary or checkout); it runs the prerequisite checks that
// need no namespace (Kubernetes version, issuer discovery) and then the probe checks (node devices,
// issuer reachability) in the Substrate namespace, stopping before it changes anything else when one
// fails and removing the namespace if it created it; it runs ate-setup deploy ate-system with the Koinon
// profile; it waits until the CRDs are Established, because applying a SandboxConfig right after the
// CRDs raced with the API server's discovery in the lab ("no matches for kind"); and it makes sure the
// router allows the atenet client identity. The install checks of the preflight run last.
package substrateinstall

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/stateracloud/koinon/internal/preflight"
)

// PinnedVersion and PinnedCommit are the Substrate release this Koinon release installs. The pin lives in
// internal/preflight (substratepin.go, ADR 0119 D4) so the preflight and the installer cannot differ.
const (
	PinnedVersion = preflight.SubstratePinnedVersion
	PinnedCommit  = preflight.SubstratePinnedCommit
)

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// Profile is the Koinon profile of ate-setup: what comes from Koinon's values.
type Profile struct {
	// StorageBucket is the object-storage bucket for snapshots (ate-setup --storage-bucket-name).
	StorageBucket string
	// SandboxClasses is ate-setup --sandbox-classes; empty keeps the default (gvisor,microvm).
	SandboxClasses string
	// ImageRepo is the registry path of the release images; empty means the pinned public path.
	ImageRepo string
	// PostgresStorageClass is the StorageClass of the bundled PostgreSQL volume
	// (ate-setup --ateapi-postgres-storage-class); empty uses the cluster default. A cluster without a
	// default StorageClass leaves the bundled Postgres Pending until this is set.
	PostgresStorageClass string
	// SetupCSI is ate-setup --setup-csi (nfs, hostpath, both or none); empty leaves ate-setup's default, none.
	SetupCSI string
	// ExpectedJWTIssuer is ate-setup --ateapi-expected-jwt-issuer: the service-account issuer Substrate's api
	// server trusts. Empty lets ate-setup read it from the API server's discovery document. It must equal the
	// iss of the cluster's tokens, and Substrate v1.0.0 fetches discovery from this URL, so pods must reach it.
	// It cannot make an unroutable cluster issuer work: Kubernetes names the first --service-account-issuer in
	// both the tokens and the discovery document, so that flag is the one to change.
	ExpectedJWTIssuer string
	// GVisorMirror is the base URL of an operator mirror of the gVisor release (layout in GVisorDefaultMirror);
	// empty means the public gVisor bucket. Only the installer host reads it, never a sandbox or a node.
	GVisorMirror string
	// Platform is the kind of cluster. Empty means detect it from the nodes: kind or generic, never gke.
	Platform Platform
	// RustFSStorageClass, RustFSStorageSize and PostgresStorageSize size the in-cluster object store and the
	// bundled PostgreSQL on the generic platform. Empty class means the cluster default StorageClass; empty
	// sizes mean DefaultRustFSStorageSize and DefaultPostgresStorageSize. They are refused on other platforms.
	RustFSStorageClass  string
	RustFSStorageSize   string
	PostgresStorageSize string
	// GCPProjectID and SnapshotTransferGSAName are ate-setup --gcp-project-id and --snapshot-transfer-gsa-name
	// for the gke platform. Empty leaves them to the environment of ate-setup (PROJECT_ID,
	// SNAPSHOT_TRANSFER_GSA_NAME). They are refused on other platforms.
	GCPProjectID            string
	SnapshotTransferGSAName string
}

// csiDrivers are the values of ate-setup --setup-csi (cmd/ate-setup/internal/steps/csi.go ValidateCSIDriver).
var csiDrivers = []string{"nfs", "hostpath", "both", "none"}

// Validate says what the profile lacks.
func (p Profile) Validate() error {
	if p.StorageBucket == "" {
		return errors.New("substrate profile lacks the storage bucket")
	}
	if p.SetupCSI != "" && !slices.Contains(csiDrivers, p.SetupCSI) {
		return fmt.Errorf("substrate profile: unknown CSI driver %q (valid: %s)", p.SetupCSI, strings.Join(csiDrivers, ", "))
	}
	if err := p.validatePlatform(); err != nil {
		return err
	}
	if p.ExpectedJWTIssuer != "" {
		if u, err := url.Parse(p.ExpectedJWTIssuer); err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return fmt.Errorf("substrate profile: the expected JWT issuer %q must be an https URL without query or fragment", p.ExpectedJWTIssuer)
		}
	}
	return nil
}

// UsesGVisor says whether the install has the gvisor sandbox class: the default classes (gvisor,microvm), the
// generic platform (gvisor only) or a list that names gvisor.
func (p Profile) UsesGVisor() bool {
	if p.Platform == PlatformGeneric || p.SandboxClasses == "" {
		return true
	}
	for _, class := range strings.Split(p.SandboxClasses, ",") {
		if strings.TrimSpace(class) == "gvisor" {
			return true
		}
	}
	return false
}

// noDevEnvFlag is passed to ate-setup deploy and delete alike, so a delete undoes the same install.
const noDevEnvFlag = "--no-dev-env"

// egressProviderFlag selects the bundled Kubernetes Secrets provider of the egress gateway; ate-setup
// requires one provider form for deploy ate-system.
const egressProviderFlag = `--credential-provider={"name":"k8s.io"}`

// SetupArgs are the arguments of the pinned ate-setup for the Koinon profile, with pre-built release
// images of the pinned tag (never a build from source), the router allow-list for the atenet client of
// the given namespace, and no developer settings.
func (p Profile) SetupArgs(namespace string) []string {
	repo := p.ImageRepo
	if repo == "" {
		repo = preflight.SubstratePinnedImageRepo
	}
	args := []string{
		"deploy", "ate-system",
		noDevEnvFlag,
		"--namespace=" + namespace,
		"--storage-bucket-name=" + p.StorageBucket,
		egressProviderFlag,
		"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID(namespace),
		"--image-repo=" + repo,
		"--image-tag=" + PinnedVersion,
	}
	if p.SandboxClasses != "" && p.Platform != PlatformGeneric {
		args = append(args, "--sandbox-classes="+p.SandboxClasses)
	}
	if p.PostgresStorageClass != "" {
		args = append(args, "--ateapi-postgres-storage-class="+p.PostgresStorageClass)
	}
	if p.SetupCSI != "" {
		args = append(args, "--setup-csi="+p.SetupCSI)
	}
	if p.ExpectedJWTIssuer != "" {
		args = append(args, "--ateapi-expected-jwt-issuer="+p.ExpectedJWTIssuer)
	}
	switch p.Platform {
	case PlatformKind:
		args = append(args, "--kind")
	case PlatformGeneric:
		// ate-setup's kind route installs an in-cluster object store and leaves out the GKE-only mounts. The
		// microVM class is left out: its asset staging needs docker on the installer host and cluster nodes of the
		// host's architecture (generic.go).
		args = append(args, "--kind", "--sandbox-classes="+genericSandboxClasses)
	case PlatformGKE:
		if p.GCPProjectID != "" {
			args = append(args, "--gcp-project-id="+p.GCPProjectID)
		}
		if p.SnapshotTransferGSAName != "" {
			args = append(args, "--snapshot-transfer-gsa-name="+p.SnapshotTransferGSAName)
		}
	}
	return args
}

// Runner runs commands: ate-setup in the source checkout, and the read-only probes of the pin check.
type Runner interface {
	// Run runs name in dir (empty is the current directory) with env added to the environment of the child,
	// writing its output to out.
	Run(ctx context.Context, dir, name string, args, env []string, out io.Writer) error
	// Output runs name in dir and returns its standard output.
	Output(ctx context.Context, dir, name string, args []string) (string, error)
}

// Installer is the installer step.
type Installer struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// Checker runs the preflight; its Config.Substrate is the driver's configuration.
	Checker *preflight.Checker
	Runner  Runner
	// SourceRepo is a git repository that holds the pinned commit (a clone of Substrate). Substrate
	// releases ship no ate-setup binary and ate-setup reads the manifests of a source tree, so the
	// installer exports PinnedCommit from this repository and builds ate-setup from that export. Nothing
	// else in the repository (its working tree, its config) is trusted.
	SourceRepo string
	// TempDir is the parent of the exported tree; empty is the system temporary directory.
	TempDir string
	// Kubeconfig and Context are both required: they go to the Kubernetes clients and to ate-setup, so
	// the installer never acts on the ambient current context.
	Kubeconfig string
	Context    string
	Profile    Profile
	Out        io.Writer
	// PollInterval and WaitTimeout bound the wait for the CRDs (defaults 2s and 2m).
	PollInterval time.Duration
	WaitTimeout  time.Duration
	// GVisorStore and GVisorFetcher are for the tests. Nil means the cluster object store reached through a
	// port-forward to its rustfs pod, and a plain HTTP GET (gvisorstore.go).
	GVisorStore   func(ctx context.Context) (AssetStore, func(), error)
	GVisorFetcher AssetFetcher
}

// ErrPrerequisites is returned when the prerequisite preflight fails: nothing was installed.
var ErrPrerequisites = errors.New("substrate prerequisites failed")

// ErrPin is returned when the pinned commit is not in the source repository or the built ate-setup is
// not the pinned release.
var ErrPin = errors.New("not the pinned Substrate release")

// gitArgs prefixes a git call so that a repository cannot change what the installer reads: replace refs
// are ignored (an object id then names its own content) and the file system monitor hook does not run.
func gitArgs(repo string, args ...string) []string {
	return append([]string{"--no-replace-objects", "-c", "core.fsmonitor=", "-C", repo}, args...)
}

// Limits of the export: the pinned Substrate tree is far below them, and they stop a hostile archive
// from filling the disk.
const (
	maxExportBytes   = 1 << 30
	maxExportEntries = 100_000
)

// Build is the exported source tree and the ate-setup built from it.
type Build struct {
	dir    string // the exported tree: ate-setup runs here, because it reads manifests from it
	binary string
	tmp    string
}

// Dir is the exported tree.
func (b *Build) Dir() string { return b.dir }

// Binary is the ate-setup built from the tree.
func (b *Build) Binary() string { return b.binary }

// Root is the temporary directory that holds the tree and the binary; Cleanup deletes it.
func (b *Build) Root() string { return b.tmp }

// Cleanup deletes the tree and the binary.
func (b *Build) Cleanup() { _ = os.RemoveAll(b.tmp) }

// exportPin exports PinnedCommit from SourceRepo into dst. Scratch files go into tmp.
func (i *Installer) exportPin(ctx context.Context, tmp, dst string) error {
	// The export comes from a fresh bare clone, not from SourceRepo itself: a local clone copies the
	// objects but not the source's config, hooks, info/attributes or replace refs, so no filter driver
	// of the source runs in git archive and no replace ref substitutes the pinned objects.
	repo := filepath.Join(tmp, "repo.git")
	clone := []string{"--no-replace-objects", "-c", "core.fsmonitor=", "clone", "--quiet", "--bare", "--", i.SourceRepo, repo}
	if err := i.Runner.Run(ctx, "", "git", clone, nil, i.Out); err != nil {
		return fmt.Errorf("%w: clone %s: %w", ErrPin, i.SourceRepo, err)
	}
	got, err := i.Runner.Output(ctx, "", "git", gitArgs(repo, "rev-parse", "--verify", PinnedCommit+"^{commit}"))
	if err != nil {
		return fmt.Errorf("%w: commit %s is not in %s: %w", ErrPin, PinnedCommit, i.SourceRepo, err)
	}
	if strings.TrimSpace(got) != PinnedCommit {
		return fmt.Errorf("%w: %s resolves the pinned commit to %q", ErrPin, i.SourceRepo, strings.TrimSpace(got))
	}
	archive := filepath.Join(tmp, "src.tar")
	if err := i.Runner.Run(ctx, "", "git", gitArgs(repo, "archive", "--format=tar", "--output="+archive, PinnedCommit), nil, i.Out); err != nil {
		return fmt.Errorf("%w: git archive %s: %w", ErrPin, PinnedCommit, err)
	}
	if err := extractTar(archive, dst); err != nil {
		return fmt.Errorf("%w: %w", ErrPin, err)
	}
	return nil
}

// BuildSetup exports the pinned commit from SourceRepo into a fresh temporary directory, builds
// ate-setup there and checks that the binary reports the pinned version and commit. The caller must
// call Cleanup when err is nil; on an error BuildSetup has removed the directory itself.
func (i *Installer) BuildSetup(ctx context.Context) (*Build, error) {
	if i.SourceRepo == "" {
		return nil, fmt.Errorf("%w: no Substrate source repository", ErrPin)
	}
	tmp, err := os.MkdirTemp(i.TempDir, "koinon-substrate-")
	if err != nil {
		return nil, fmt.Errorf("temporary directory: %w", err)
	}
	b := &Build{dir: filepath.Join(tmp, "src"), binary: filepath.Join(tmp, "ate-setup"), tmp: tmp}
	fail := func(err error) (*Build, error) {
		b.Cleanup()
		return nil, err
	}
	if err := i.exportPin(ctx, tmp, b.dir); err != nil {
		return fail(err)
	}
	const versionPkg = "go.ate.dev/substrate/internal/version"
	ldflags := "-X " + versionPkg + ".Version=" + PinnedVersion + " -X " + versionPkg + ".Commit=" + PinnedCommit
	build := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", b.binary, "./cmd/ate-setup"}
	if err := i.Runner.Run(ctx, b.dir, "go", build, nil, i.Out); err != nil {
		return fail(fmt.Errorf("build ate-setup %s: %w", PinnedVersion, err))
	}
	ver, err := i.Runner.Output(ctx, "", b.binary, []string{"--version"})
	if err != nil {
		return fail(fmt.Errorf("%w: ate-setup --version: %w", ErrPin, err))
	}
	if err := checkVersionLine(ver); err != nil {
		return fail(err)
	}
	return b, nil
}

// checkVersionLine requires that the line of ate-setup --version names the pinned version and carries
// exactly the pinned commit: a "-dirty" suffix or any other value is refused.
func checkVersionLine(line string) error {
	var commits []string
	versionOK := false
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "commit="); ok {
			commits = append(commits, v)
		}
		if f == PinnedVersion || strings.HasSuffix(f, "Version="+PinnedVersion) {
			versionOK = true
		}
	}
	if !versionOK || len(commits) != 1 || commits[0] != PinnedCommit {
		return fmt.Errorf("%w: ate-setup reports %q, want %s at commit %s", ErrPin, strings.TrimSpace(line), PinnedVersion, PinnedCommit)
	}
	return nil
}

// extractTar unpacks a tar archive of regular files and directories into dir. It refuses a name that is
// not a local path, a link of any kind, and an archive above maxExportBytes or maxExportEntries. Every
// file operation goes through an os.Root on dir, so the kernel also keeps each write inside dir.
func extractTar(archive, dir string) error {
	f, err := os.Open(archive) //nolint:gosec // the path is inside the temporary directory the installer made
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(f)
	var total int64
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the export: %w", err)
		}
		if entries >= maxExportEntries {
			return fmt.Errorf("the export has more than %d entries", maxExportEntries)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // git archive writes the commit id here
		}
		name, err := localName(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o750)
		case tar.TypeReg:
			if total += h.Size; h.Size < 0 || total > maxExportBytes {
				return fmt.Errorf("the export is larger than %d bytes", int64(maxExportBytes))
			}
			if err = root.MkdirAll(filepath.Dir(name), 0o750); err == nil {
				mode := os.FileMode(0o600)
				if h.Mode&0o100 != 0 {
					mode = 0o700
				}
				err = writeExtracted(root, name, tr, h.Size, mode)
			}
		default:
			err = fmt.Errorf("the export has an entry %q of type %c", h.Name, h.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

// localName turns an archive entry name into a path relative to the extraction root, or refuses it when
// it is absolute or climbs out with "..".
func localName(entry string) (string, error) {
	name := filepath.Clean(filepath.FromSlash(entry))
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("the export has the path %q outside its directory", entry)
	}
	return name, nil
}

// writeExtracted writes exactly size bytes of r to name under root: a header that understates its
// content cannot make the file larger.
func writeExtracted(root *os.Root, name string, r io.Reader, size int64, mode os.FileMode) error {
	out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.CopyN(out, r, size)
	return errors.Join(err, out.Close())
}

// Install runs the step. It is safe to run again: ate-setup is idempotent for the pinned release and the
// router change is made only when it is missing. The pin is checked before anything changes.
//
// The final preflight runs without the NetworkPolicy probe: the release namespace may not exist yet (the
// chart creates it), and the installer creates nothing there. The chart's own preflight runs that probe.
func (i *Installer) Install(ctx context.Context) error {
	if err := i.Profile.Validate(); err != nil {
		return err
	}
	// The issuer probe checks the issuer ate-setup is about to write.
	sub := i.Checker.Config.SubstrateOrDefault()
	sub.ExpectedIssuer = i.Profile.ExpectedJWTIssuer
	i.Checker.Config.Substrate = &sub
	if i.Kubeconfig == "" || i.Context == "" {
		return errors.New("the installer needs a kubeconfig and a context: it does not act on the ambient current context")
	}
	platform, err := i.resolvePlatform(ctx)
	if err != nil {
		return err
	}
	// From here the platform is known: every later step reads it from the profile.
	i.Profile.Platform = platform
	if err := i.Profile.Validate(); err != nil {
		return err
	}
	if err := i.CheckKind(ctx); err != nil {
		return err
	}
	// The installed classes decide whether the gVisor node check applies.
	sub.GVisor = i.Profile.UsesGVisor()
	i.Checker.Config.Substrate = &sub
	if platform == PlatformGeneric {
		// Only the gvisor class is installed, so the node devices of the vm tier are not needed.
		sub.VMTier = false
		i.Checker.Config.Substrate = &sub
		_, _ = fmt.Fprintln(i.Out, "platform generic: the vm tier (microVM) is not installed; the sandbox class is gvisor")
	}
	setup, err := i.BuildSetup(ctx)
	if err != nil {
		return err
	}
	// After a failed ate-setup the build stays: the cleanup command below runs the same pinned binary.
	keepBuild := false
	defer func() {
		if !keepBuild {
			setup.Cleanup()
		}
	}()
	var setupEnv []string
	if platform == PlatformGeneric {
		// File work only, so a pin that no longer fits fails before anything is created.
		if err := i.Profile.ApplyGenericOverlay(setup.Dir(), i.Checker.Config.SubstrateOrDefault().Namespace); err != nil {
			return err
		}
		if setupEnv, err = WriteDockerShim(filepath.Join(setup.Root(), "shim"), os.Getenv("PATH")); err != nil {
			return fmt.Errorf("write the docker shim: %w", err)
		}
	}
	// Checks that need no namespace come first: a cluster that cannot run Substrate fails with nothing created.
	static := i.Checker.SubstrateStaticPrerequisites(ctx)
	if platform == PlatformGeneric {
		static = append(static, i.CheckStorage(ctx)...)
		static = append(static, i.CheckExistingVolumes(ctx)...)
	}
	if failed := preflight.Write(i.Out, static); failed > 0 {
		return fmt.Errorf("%w: %d checks; nothing was installed", ErrPrerequisites, failed)
	}
	// One issuer for the probe and for ate-setup, passed to ate-setup explicitly: there a flag wins over an
	// EXPECTED_JWT_ISSUER in the operator's environment, which would otherwise replace the discovery issuer.
	profile := i.Profile
	if profile.ExpectedJWTIssuer == "" && i.Checker.Issuer != nil {
		issuer, err := i.Checker.SubstrateIssuer(ctx)
		if err != nil {
			return fmt.Errorf("%w: read the service-account issuer: %w; nothing was installed", ErrPrerequisites, err)
		}
		profile.ExpectedJWTIssuer = issuer
		sub.ExpectedIssuer = issuer
		i.Checker.Config.Substrate = &sub
	}
	created, err := i.EnsureNamespace(ctx)
	if err != nil {
		return err
	}
	if err := i.WaitNamespaceReady(ctx); err != nil {
		if created {
			i.removeNamespace(ctx)
		}
		return err
	}
	if failed := preflight.Write(i.Out, i.Checker.SubstrateProbePrerequisites(ctx)); failed > 0 {
		if created {
			i.removeNamespace(ctx)
		}
		return fmt.Errorf("%w: %d checks; nothing was installed", ErrPrerequisites, failed)
	}
	if platform == PlatformGeneric {
		if err := i.EnsureRustFSCredentials(ctx); err != nil {
			if created {
				i.removeNamespace(ctx)
			}
			return err
		}
	}
	args := append(profile.SetupArgs(i.Checker.Config.SubstrateOrDefault().Namespace),
		"--kubeconfig="+i.Kubeconfig, "--context="+i.Context)
	if err := i.Runner.Run(ctx, setup.Dir(), setup.Binary(), args, setupEnv, i.Out); err != nil {
		keepBuild = true
		return fmt.Errorf("ate-setup %s: %w\nate-setup may have left a partial install. Remove it with the same pinned ate-setup, then run the installer again (the build directory %s stays so that command can run; delete it after the cleanup):\n  %s",
			PinnedVersion, err, setup.Root(), cleanupCommand(setup, platform, i.Checker.Config.SubstrateOrDefault().Namespace, i.Kubeconfig, i.Context))
	}
	if _, err := i.EnsureAPIAuthentication(ctx, profile.ExpectedJWTIssuer); err != nil {
		return err
	}
	if err := i.WaitCRDs(ctx); err != nil {
		return err
	}
	if err := i.EnsureRouterSPIFFE(ctx); err != nil {
		return err
	}
	if err := i.EnsureGVisorAssets(ctx); err != nil {
		return err
	}
	if failed := preflight.Write(i.Out, i.Checker.Run(ctx)); failed > 0 {
		return fmt.Errorf("%w: %d checks after the install", preflight.ErrFailed, failed)
	}
	return nil
}

// cleanupCommand is the command that removes what ate-setup deploy ate-system created (ate-setup delete
// ate-system). It runs from the exported tree, as ate-setup reads manifests from it.
// The kind and generic platforms delete with --kind: ate-setup then deletes the objects of the kind overlay,
// which on the generic platform is the overlay the installer corrected in this tree.
func cleanupCommand(b *Build, platform Platform, namespace, kubeconfig, kctx string) string {
	kind := ""
	if platform == PlatformKind || platform == PlatformGeneric {
		kind = " --kind"
	}
	return fmt.Sprintf("cd %s && %s delete ate-system %s%s --namespace=%s --kubeconfig=%s --context=%s",
		shellQuote(b.Dir()), shellQuote(b.Binary()), noDevEnvFlag, kind, shellQuote(namespace), shellQuote(kubeconfig), shellQuote(kctx))
}

// resolvePlatform is the platform of the profile, or the detected one when the profile has none.
func (i *Installer) resolvePlatform(ctx context.Context) (Platform, error) {
	if i.Profile.Platform != "" {
		return i.Profile.Platform, nil
	}
	p, err := i.DetectPlatform(ctx)
	if err != nil {
		return "", err
	}
	if p == PlatformGeneric && i.allProviders(ctx, "gce://") {
		_, _ = fmt.Fprintln(i.Out, "warning: every node is a GKE node, and no --platform was given: the install uses the generic platform (an in-cluster object store). Set --platform gke for GCS.")
	}
	_, _ = fmt.Fprintf(i.Out, "platform %s (detected from the nodes; set --platform to choose)\n", p)
	return p, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// removeNamespace deletes the namespace the installer created for the probes, after a prerequisite failed.
func (i *Installer) removeNamespace(ctx context.Context) {
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := i.Kube.CoreV1().Namespaces().Delete(dctx, ns, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		_, _ = fmt.Fprintf(i.Out, "could not delete the namespace %s that the installer created: %v\n", ns, err)
	}
}

// EnsureNamespace creates Substrate's namespace when it is missing, with Pod Security enforce=privileged
// (the device probe mounts hostPath devices and Substrate's workers are privileged, plan section 4.4),
// and reports whether it created it. The prerequisite probes run pods there before ate-setup, which
// creates the namespace on a fresh cluster. An existing namespace is left as it is.
func (i *Installer) EnsureNamespace(ctx context.Context) (bool, error) {
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	_, err := i.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("read namespace %s: %w", ns, err)
	}
	_, err = i.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: ns,
		// warn and audit too: the device probe mounts hostPath devices, and without them the API server
		// warns about the restricted profile on every probe pod.
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "privileged",
			"pod-security.kubernetes.io/warn":    "privileged",
			"pod-security.kubernetes.io/audit":   "privileged",
		},
	}}, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("create namespace %s: %w", ns, err)
	}
	return true, nil
}

// WaitNamespaceReady waits until the controllers have filled the Substrate namespace: the probe pods use
// the default ServiceAccount and the issuer probe mounts the default ServiceAccount token, whose ca.crt is the kube-root-ca.crt ConfigMap, and both appear
// some time after a namespace is created. Without the wait a probe pod on a fresh cluster is refused or
// stays in FailedMount.
func (i *Installer) WaitNamespaceReady(ctx context.Context) error {
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	ctx, cancel := context.WithTimeout(ctx, orDuration(i.WaitTimeout, 2*time.Minute))
	defer cancel()
	for {
		var missing []string
		if _, err := i.Kube.CoreV1().ServiceAccounts(ns).Get(ctx, "default", metav1.GetOptions{}); apierrors.IsNotFound(err) {
			missing = append(missing, "ServiceAccount default")
		} else if err != nil {
			return fmt.Errorf("read ServiceAccount %s/default: %w", ns, err)
		}
		if _, err := i.Kube.CoreV1().ConfigMaps(ns).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{}); apierrors.IsNotFound(err) {
			missing = append(missing, "ConfigMap kube-root-ca.crt")
		} else if err != nil {
			return fmt.Errorf("read ConfigMap %s/kube-root-ca.crt: %w", ns, err)
		}
		if len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %s not ready in time, missing: %s", ns, strings.Join(missing, ", "))
		case <-time.After(orDuration(i.PollInterval, 2*time.Second)):
		}
	}
}

// WaitCRDs waits until every Substrate CRD has the condition Established=True.
func (i *Installer) WaitCRDs(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, orDuration(i.WaitTimeout, 2*time.Minute))
	defer cancel()
	for {
		pending, err := i.pendingCRDs(ctx)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("CRDs not Established in time: %s", strings.Join(pending, ", "))
		case <-time.After(orDuration(i.PollInterval, 2*time.Second)):
		}
	}
}

func (i *Installer) pendingCRDs(ctx context.Context) ([]string, error) {
	var pending []string
	for _, name := range preflight.SubstrateCRDs {
		crd, err := i.Dynamic.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			pending = append(pending, name)
			continue
		case err != nil:
			return nil, fmt.Errorf("read CRD %s: %w", name, err)
		}
		conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
		established := slices.ContainsFunc(conds, func(c any) bool {
			m, ok := c.(map[string]any)
			return ok && m["type"] == "Established" && m["status"] == "True"
		})
		if !established {
			pending = append(pending, name)
		}
	}
	return pending, nil
}

// routerContainer is the router container of the atenet-router Deployment (Substrate v1.0.0).
const routerContainer = "atenet-router"

// EnsureRouterSPIFFE adds the atenet client identity to the router's --ingress-allowed-spiffe-ids when
// it is missing (phase 0 proof P-B: without it the actor cannot reach the gateway). It reads the flag
// in the container's command and arguments and retries on a conflicting update.
func (i *Installer) EnsureRouterSPIFFE(ctx context.Context) error {
	sub := i.Checker.Config.SubstrateOrDefault()
	deployments := i.Kube.AppsV1().Deployments(sub.Namespace)
	where := sub.Namespace + "/" + sub.RouterDeployment
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := deployments.Get(ctx, sub.RouterDeployment, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read router Deployment %s: %w", where, err)
		}
		n := slices.IndexFunc(d.Spec.Template.Spec.Containers, func(c corev1.Container) bool { return c.Name == routerContainer })
		if n < 0 {
			return fmt.Errorf("router Deployment %s has no container %q", where, routerContainer)
		}
		ctr := &d.Spec.Template.Spec.Containers[n]
		if !addSPIFFEID(ctr, preflight.SubstrateRouterSPIFFEID(sub.Namespace)) {
			return nil
		}
		if _, err := deployments.Update(ctx, d, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update router Deployment %s: %w", where, err)
		}
		return nil
	})
	return err
}

// addSPIFFEID makes the flag of the container allow id, in its command or its arguments, and reports
// whether it changed anything.
func addSPIFFEID(ctr *corev1.Container, id string) bool {
	const prefix = preflight.SubstrateRouterSPIFFEFlag + "="
	for _, list := range []*[]string{&ctr.Command, &ctr.Args} {
		for n, arg := range *list {
			v, ok := strings.CutPrefix(arg, prefix)
			if !ok {
				continue
			}
			ids := strings.Split(v, ",")
			if slices.Contains(ids, id) {
				return false
			}
			(*list)[n] = prefix + strings.Join(append(ids, id), ",")
			return true
		}
	}
	ctr.Args = append(ctr.Args, prefix+id)
	return true
}

func orDuration(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// allProviders reports whether every node has a providerID that starts with prefix.
func (i *Installer) allProviders(ctx context.Context, prefix string) bool {
	nodes, err := i.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return false
	}
	for _, n := range nodes.Items {
		if !strings.HasPrefix(n.Spec.ProviderID, prefix) {
			return false
		}
	}
	return true
}
