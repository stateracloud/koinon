// SPDX-License-Identifier: Apache-2.0

// Package substrateinstall is Koinon's installer step for Agent Substrate (ADR 0119 D4, issue #1089).
// Substrate is a pinned cluster prerequisite, not part of the chart: its namespaces are fixed, so it is a
// cluster singleton, and its set-up is imperative (a CA, a Postgres schema, CRDs and then objects that
// need those CRDs), which a chart cannot render deterministically. The step does four things in order:
// it checks that ate-setup and its source checkout are the pinned release and runs the prerequisite
// preflight (Kubernetes version, node devices, service-account issuer), stopping before it changes
// anything when one fails; it runs ate-setup deploy ate-system with the Koinon profile; it waits
// until the CRDs are Established, because applying a SandboxConfig right after the CRDs raced with the
// API server's discovery in the lab ("no matches for kind"); and it makes sure the router allows the
// atenet client identity. The install checks of the preflight run last.
package substrateinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// SetupBinary is the path of ate-setup built from SourceDir, and SourceDir a clean checkout of the
	// release tag: Substrate releases ship no ate-setup binary and ate-setup reads the manifests there.
	SetupBinary string
	SourceDir   string
	// Kubeconfig and Context are passed to ate-setup; both may be empty (in-cluster or default rules).
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

// ErrPin is returned when the ate-setup binary or the source checkout is not the pinned release.
var ErrPin = errors.New("not the pinned Substrate release")

// VerifyPin refuses a binary or a checkout that is not the pinned release: ate-setup must report the
// pinned version and commit, and SourceDir must be a clean checkout of the pinned commit (its manifests
// are what ate-setup applies). Substrate publishes no ate-setup binary, so there is no digest to compare.
func (i *Installer) VerifyPin(ctx context.Context) error {
	ver, err := i.Runner.Output(ctx, "", i.SetupBinary, []string{"--version"})
	if err != nil {
		return fmt.Errorf("%w: %s --version: %w", ErrPin, i.SetupBinary, err)
	}
	if !strings.Contains(ver, "Version="+PinnedVersion+" ") || !strings.Contains(ver, "commit="+PinnedCommit) {
		return fmt.Errorf("%w: ate-setup reports %q, want %s at commit %s", ErrPin, strings.TrimSpace(ver), PinnedVersion, PinnedCommit)
	}
	head, err := i.Runner.Output(ctx, i.SourceDir, "git", []string{"rev-parse", "HEAD"})
	if err != nil {
		return fmt.Errorf("%w: git rev-parse in %s: %w", ErrPin, i.SourceDir, err)
	}
	if strings.TrimSpace(head) != PinnedCommit {
		return fmt.Errorf("%w: %s is at %s, want %s", ErrPin, i.SourceDir, strings.TrimSpace(head), PinnedCommit)
	}
	dirty, err := i.Runner.Output(ctx, i.SourceDir, "git", []string{"status", "--porcelain"})
	if err != nil {
		return fmt.Errorf("%w: git status in %s: %w", ErrPin, i.SourceDir, err)
	}
	if strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("%w: %s has uncommitted changes", ErrPin, i.SourceDir)
	}
	return nil
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
	if err := i.VerifyPin(ctx); err != nil {
		return err
	}
	if err := i.EnsureNamespace(ctx); err != nil {
		return err
	}
	if failed := preflight.Write(i.Out, i.Checker.SubstratePrerequisites(ctx)); failed > 0 {
		return fmt.Errorf("%w: %d checks; nothing was installed", ErrPrerequisites, failed)
	}
	args := i.Profile.SetupArgs(i.Checker.Config.SubstrateOrDefault().Namespace)
	if i.Kubeconfig != "" {
		args = append(args, "--kubeconfig="+i.Kubeconfig)
	}
	if i.Context != "" {
		args = append(args, "--context="+i.Context)
	}
	if err := i.Runner.Run(ctx, i.SourceDir, i.SetupBinary, args, i.Out); err != nil {
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

// EnsureNamespace creates Substrate's namespace when it is missing, with Pod Security enforce=privileged
// (the device probe mounts hostPath devices and Substrate's workers are privileged, plan section 4.4).
// The prerequisite probes run pods there before ate-setup, which creates the namespace on a fresh
// cluster. An existing namespace is left as it is.
func (i *Installer) EnsureNamespace(ctx context.Context) error {
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	_, err := i.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("read namespace %s: %w", ns, err)
	}
	_, err = i.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: map[string]string{"pod-security.kubernetes.io/enforce": "privileged"},
	}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}
	return nil
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

// EnsureRouterSPIFFE adds the atenet client identity to the router's --ingress-allowed-spiffe-ids when
// it is missing (phase 0 proof P-B: without it the actor cannot reach the gateway).
func (i *Installer) EnsureRouterSPIFFE(ctx context.Context) error {
	sub := i.Checker.Config.SubstrateOrDefault()
	deployments := i.Kube.AppsV1().Deployments(sub.Namespace)
	d, err := deployments.Get(ctx, sub.RouterDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read router Deployment %s/%s: %w", sub.Namespace, sub.RouterDeployment, err)
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("router Deployment %s/%s has no container", sub.Namespace, sub.RouterDeployment)
	}
	ctr := &d.Spec.Template.Spec.Containers[0]
	const prefix = preflight.SubstrateRouterSPIFFEFlag + "="
	id := preflight.SubstrateRouterSPIFFEID(sub.Namespace)
	for n, arg := range ctr.Args {
		v, ok := strings.CutPrefix(arg, prefix)
		if !ok {
			continue
		}
		ids := strings.Split(v, ",")
		if slices.Contains(ids, id) {
			return nil
		}
		ctr.Args[n] = prefix + strings.Join(append(ids, id), ",")
		_, err = deployments.Update(ctx, d, metav1.UpdateOptions{})
		return err
	}
	ctr.Args = append(ctr.Args, prefix+id)
	_, err = deployments.Update(ctx, d, metav1.UpdateOptions{})
	return err
}

func orDuration(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
