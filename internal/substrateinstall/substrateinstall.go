// SPDX-License-Identifier: Apache-2.0

// Package substrateinstall is Koinon's installer step for Agent Substrate (ADR 0119 D4, issue #1089).
// Substrate is a pinned cluster prerequisite, not part of the chart: its namespaces are fixed, so it is a
// cluster singleton, and its set-up is imperative (a CA, a Postgres schema, CRDs and then objects that
// need those CRDs), which a chart cannot render deterministically. The step does four things in order:
// it runs the prerequisite preflight (Kubernetes version, node devices, service-account issuer) and stops
// before it changes anything when one fails; it runs the pinned ate-setup with the Koinon profile; it waits
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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/preflight"
)

// PinnedVersion is the Substrate release this Koinon release installs and tests. The preflight accepts
// a later v1.x release (the compatibility promise of v1.0.0) that an operator brought along.
const PinnedVersion = "v1.0.0"

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// Profile is the Koinon profile of ate-setup: what comes from Koinon's values.
type Profile struct {
	// ObjectStoreEndpoint, ObjectStoreBucket and ObjectStoreSecret locate the S3-compatible object store
	// for snapshots. The Secret holds the credential; the installer never reads it.
	ObjectStoreEndpoint string
	ObjectStoreBucket   string
	ObjectStoreSecret   string
	// DatabaseSecret is the Secret with the connection string of ateapi's Postgres database (Koinon's
	// database mode provides it).
	DatabaseSecret string
}

// Validate says what the profile lacks.
func (p Profile) Validate() error {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"object store endpoint", p.ObjectStoreEndpoint}, {"object store bucket", p.ObjectStoreBucket},
		{"object store secret", p.ObjectStoreSecret}, {"database secret", p.DatabaseSecret},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("substrate profile lacks %s", strings.Join(missing, ", "))
	}
	return nil
}

// SetupArgs are the arguments of ate-setup for the Koinon profile: kind-free install, S3 object store
// and Postgres from Koinon's values, and no bundled Prometheus or OpenTelemetry (ADR 0119 D4).
func (p Profile) SetupArgs() []string {
	return []string{
		"install",
		"--object-store=s3",
		"--object-store-endpoint=" + p.ObjectStoreEndpoint,
		"--object-store-bucket=" + p.ObjectStoreBucket,
		"--object-store-secret=" + p.ObjectStoreSecret,
		"--database-secret=" + p.DatabaseSecret,
		"--with-prometheus=false",
		"--with-otel=false",
	}
}

// Runner runs the pinned ate-setup binary.
type Runner interface {
	Run(ctx context.Context, name string, args []string, out io.Writer) error
}

// Installer is the installer step.
type Installer struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// Checker runs the preflight; its Config.Substrate is the driver's configuration.
	Checker *preflight.Checker
	Runner  Runner
	// SetupBinary is the path of the pinned ate-setup (a verified download of PinnedVersion).
	SetupBinary string
	Profile     Profile
	Out         io.Writer
	// PollInterval and WaitTimeout bound the wait for the CRDs (defaults 2s and 2m).
	PollInterval time.Duration
	WaitTimeout  time.Duration
}

// ErrPrerequisites is returned when the prerequisite preflight fails: nothing was installed.
var ErrPrerequisites = errors.New("substrate prerequisites failed")

// Install runs the step. It is safe to run again: ate-setup is idempotent for the pinned release and the
// router change is made only when it is missing.
func (i *Installer) Install(ctx context.Context) error {
	if err := i.Profile.Validate(); err != nil {
		return err
	}
	if failed := preflight.Write(i.Out, i.Checker.SubstratePrerequisites(ctx)); failed > 0 {
		return fmt.Errorf("%w: %d checks; nothing was installed", ErrPrerequisites, failed)
	}
	if err := i.Runner.Run(ctx, i.SetupBinary, i.Profile.SetupArgs(), i.Out); err != nil {
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
	for n, arg := range ctr.Args {
		v, ok := strings.CutPrefix(arg, prefix)
		if !ok {
			continue
		}
		ids := strings.Split(v, ",")
		if slices.Contains(ids, preflight.SubstrateRouterSPIFFEID) {
			return nil
		}
		ctr.Args[n] = prefix + strings.Join(append(ids, preflight.SubstrateRouterSPIFFEID), ",")
		_, err = deployments.Update(ctx, d, metav1.UpdateOptions{})
		return err
	}
	ctr.Args = append(ctr.Args, prefix+preflight.SubstrateRouterSPIFFEID)
	_, err = deployments.Update(ctx, d, metav1.UpdateOptions{})
	return err
}

func orDuration(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
