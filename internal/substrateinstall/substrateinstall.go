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
}

// Validate says what the profile lacks.
func (p Profile) Validate() error {
	if p.StorageBucket == "" {
		return errors.New("substrate profile lacks the storage bucket")
	}
	return nil
}

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
		"--no-dev-env",
		"--namespace=" + namespace,
		"--storage-bucket-name=" + p.StorageBucket,
		egressProviderFlag,
		"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID(namespace),
		"--image-repo=" + repo,
		"--image-tag=" + PinnedVersion,
	}
	if p.SandboxClasses != "" {
		args = append(args, "--sandbox-classes="+p.SandboxClasses)
	}
	return args
}

// Runner runs commands: ate-setup in the source checkout, and the read-only probes of the pin check.
type Runner interface {
	// Run runs name in dir (empty is the current directory), writing its output to out.
	Run(ctx context.Context, dir, name string, args []string, out io.Writer) error
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
}

// ErrPrerequisites is returned when the prerequisite preflight fails: nothing was installed.
var ErrPrerequisites = errors.New("substrate prerequisites failed")

// ErrPin is returned when the pinned commit is not in the source repository or the built ate-setup is
// not the pinned release.
var ErrPin = errors.New("not the pinned Substrate release")

// gitArgs prefixes a git call so that a repository's configuration cannot run a program (the file
// system monitor hook) while the installer reads it.
func gitArgs(repo string, args ...string) []string {
	return append([]string{"-c", "core.fsmonitor=", "-C", repo}, args...)
}

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

// Cleanup deletes the tree and the binary.
func (b *Build) Cleanup() { _ = os.RemoveAll(b.tmp) }

// BuildSetup exports the pinned commit from SourceRepo into a fresh temporary directory, builds
// ate-setup there and checks that the binary reports the pinned version and commit. The caller must
// call Cleanup when err is nil; on an error BuildSetup has removed the directory itself.
func (i *Installer) BuildSetup(ctx context.Context) (*Build, error) {
	if i.SourceRepo == "" {
		return nil, fmt.Errorf("%w: no Substrate source repository", ErrPin)
	}
	got, err := i.Runner.Output(ctx, "", "git", gitArgs(i.SourceRepo, "rev-parse", "--verify", PinnedCommit+"^{commit}"))
	if err != nil {
		return nil, fmt.Errorf("%w: commit %s is not in %s: %w", ErrPin, PinnedCommit, i.SourceRepo, err)
	}
	if strings.TrimSpace(got) != PinnedCommit {
		return nil, fmt.Errorf("%w: %s resolves the pinned commit to %q", ErrPin, i.SourceRepo, strings.TrimSpace(got))
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
	archive := filepath.Join(tmp, "src.tar")
	if err := i.Runner.Run(ctx, "", "git", gitArgs(i.SourceRepo, "archive", "--format=tar", "--output="+archive, PinnedCommit), i.Out); err != nil {
		return fail(fmt.Errorf("%w: git archive %s: %w", ErrPin, PinnedCommit, err))
	}
	if err := extractTar(archive, b.dir); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrPin, err))
	}
	const versionPkg = "go.ate.dev/substrate/internal/version"
	ldflags := "-X " + versionPkg + ".Version=" + PinnedVersion + " -X " + versionPkg + ".Commit=" + PinnedCommit
	build := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", b.binary, "./cmd/ate-setup"}
	if err := i.Runner.Run(ctx, b.dir, "go", build, i.Out); err != nil {
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

// extractTar unpacks a tar archive of regular files and directories into dir. It refuses a path that
// leaves dir and a link of any kind.
func extractTar(archive, dir string) error {
	f, err := os.Open(archive) //nolint:gosec // the path is inside the temporary directory the installer made
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	root := filepath.Clean(dir)
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the export: %w", err)
		}
		target := filepath.Join(root, filepath.FromSlash(h.Name))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("the export has the path %q outside its directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o750)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(target), 0o750); err == nil {
				mode := os.FileMode(0o600)
				if h.Mode&0o100 != 0 {
					mode = 0o700
				}
				err = writeExtracted(target, tr, mode)
			}
		case tar.TypeXGlobalHeader:
			// git archive writes the commit id here.
		default:
			err = fmt.Errorf("the export has an entry %q of type %c", h.Name, h.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

func writeExtracted(path string, r io.Reader, mode os.FileMode) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // inside the temporary directory; extractTar refuses paths that leave it
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
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
	if i.Kubeconfig == "" || i.Context == "" {
		return errors.New("the installer needs a kubeconfig and a context: it does not act on the ambient current context")
	}
	setup, err := i.BuildSetup(ctx)
	if err != nil {
		return err
	}
	defer setup.Cleanup()
	// Checks that need no namespace come first: a cluster that cannot run Substrate fails with nothing created.
	if failed := preflight.Write(i.Out, i.Checker.SubstrateStaticPrerequisites(ctx)); failed > 0 {
		return fmt.Errorf("%w: %d checks; nothing was installed", ErrPrerequisites, failed)
	}
	created, err := i.EnsureNamespace(ctx)
	if err != nil {
		return err
	}
	if failed := preflight.Write(i.Out, i.Checker.SubstrateProbePrerequisites(ctx)); failed > 0 {
		if created {
			i.removeNamespace(ctx)
		}
		return fmt.Errorf("%w: %d checks; nothing was installed", ErrPrerequisites, failed)
	}
	args := append(i.Profile.SetupArgs(i.Checker.Config.SubstrateOrDefault().Namespace),
		"--kubeconfig="+i.Kubeconfig, "--context="+i.Context)
	if err := i.Runner.Run(ctx, setup.Dir(), setup.Binary(), args, i.Out); err != nil {
		return fmt.Errorf("ate-setup %s: %w", PinnedVersion, err)
	}
	if err := i.WaitCRDs(ctx); err != nil {
		return err
	}
	if err := i.EnsureRouterSPIFFE(ctx); err != nil {
		return err
	}
	if failed := preflight.Write(i.Out, i.Checker.Run(ctx)); failed > 0 {
		return fmt.Errorf("%w: %d checks after the install", preflight.ErrFailed, failed)
	}
	return nil
}

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
		Name:   ns,
		Labels: map[string]string{"pod-security.kubernetes.io/enforce": "privileged"},
	}}, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("create namespace %s: %w", ns, err)
	}
	return true, nil
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
