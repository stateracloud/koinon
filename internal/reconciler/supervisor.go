// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/preflight"
)

// DefaultMaxRecreatesPerPass is how many sandboxes one reconcile pass may delete to recreate them
// with a changed spec (ADR 0085 decision 9).
const DefaultMaxRecreatesPerPass = 4

// supervisorReadTimeout bounds the ConfigMap read, so a slow API server cannot hold a pass.
const supervisorReadTimeout = 10 * time.Second

// SupervisorSource says which supervisor image the OpenShell gateway starts sandboxes with.
type SupervisorSource interface {
	// SupervisorImage returns the gateway's supervisor_image; empty when it does not set one.
	SupervisorImage(ctx context.Context) (string, error)
}

// KubeSupervisorImage reads supervisor_image from OpenShell's gateway ConfigMap, the object the
// install preflight reads (internal/preflight).
type KubeSupervisorImage struct {
	Client    kubernetes.Interface
	Namespace string
	ConfigMap string
}

// SupervisorImage implements SupervisorSource.
func (k KubeSupervisorImage) SupervisorImage(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, supervisorReadTimeout)
	defer cancel()
	cm, err := k.Client.CoreV1().ConfigMaps(k.Namespace).Get(ctx, k.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read the OpenShell gateway ConfigMap %s/%s: %w", k.Namespace, k.ConfigMap, err)
	}
	toml := cm.Data[preflight.GatewayConfigKey]
	if toml == "" {
		return "", fmt.Errorf("the OpenShell gateway ConfigMap %s/%s has no %s", k.Namespace, k.ConfigMap, preflight.GatewayConfigKey)
	}
	return preflight.SupervisorImage(toml), nil
}

// refreshSupervisor reads the gateway's supervisor image once per pass. A failed read keeps the
// last image read: dropping it would change every sandbox's spec hash and recreate the fleet.
// Before the first successful read the image is unknown, and sandbox work waits (supervisorReady).
func (r *Reconciler) refreshSupervisor(ctx context.Context) {
	if r.cfg.Supervisor == nil {
		return
	}
	img, err := r.cfg.Supervisor.SupervisorImage(ctx)
	r.supMu.Lock()
	defer r.supMu.Unlock()
	if err != nil {
		r.supErr = err
		r.log.Warn("could not read the OpenShell supervisor image; sandboxes keep the last one read", "err", err)
		return
	}
	r.supErr = nil
	if r.supKnown && r.supImage != img {
		r.log.Info("the OpenShell supervisor image changed; sandboxes are recreated, a few per pass", "from", r.supImage, "to", img)
	}
	r.supImage, r.supKnown = img, true
}

// supervisorImage is the image last read; empty when none is configured or known.
func (r *Reconciler) supervisorImage() string {
	r.supMu.Lock()
	defer r.supMu.Unlock()
	return r.supImage
}

// supervisorReady is an error while a source is configured and no read has succeeded yet: the spec
// hash cannot be computed, and a hash without the image would recreate every sandbox once it is known.
func (r *Reconciler) supervisorReady() error {
	if r.cfg.Supervisor == nil {
		return nil
	}
	r.supMu.Lock()
	defer r.supMu.Unlock()
	if r.supKnown {
		return nil
	}
	return fmt.Errorf("the OpenShell supervisor image is not known yet: %w", r.supErr)
}

// takeRecreate spends one of the pass's recreates; false when the cap is used up (ADR 0085
// decision 9: an OpenShell upgrade must not recreate the whole fleet at once).
func (r *Reconciler) takeRecreate() bool {
	limit := r.cfg.MaxRecreatesPerPass
	if limit < 0 {
		return true
	}
	return r.recreates.Add(1) <= int32(limit) //nolint:gosec // a small configured count
}
