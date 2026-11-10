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

// gatewayIDReadTimeout bounds the ConfigMap read, so a slow API server cannot hold a pass.
const gatewayIDReadTimeout = 10 * time.Second

// GatewayIDSource says which gateway_id the OpenShell gateway runs with.
type GatewayIDSource interface {
	// GatewayID returns the gateway's gateway_id (OpenShell's default when the configuration sets none).
	GatewayID(ctx context.Context) (string, error)
}

// KubeGatewayID reads gateway_id from OpenShell's gateway ConfigMap, the object the install preflight
// reads (internal/preflight).
type KubeGatewayID struct {
	Client    kubernetes.Interface
	Namespace string
	ConfigMap string
}

// GatewayID implements GatewayIDSource.
func (k KubeGatewayID) GatewayID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayIDReadTimeout)
	defer cancel()
	cm, err := k.Client.CoreV1().ConfigMaps(k.Namespace).Get(ctx, k.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read the OpenShell gateway ConfigMap %s/%s: %w", k.Namespace, k.ConfigMap, err)
	}
	toml := cm.Data[preflight.GatewayConfigKey]
	if toml == "" {
		return "", fmt.Errorf("the OpenShell gateway ConfigMap %s/%s has no %s", k.Namespace, k.ConfigMap, preflight.GatewayConfigKey)
	}
	return preflight.GatewayID(toml), nil
}

// checkGatewayID compares openshell.gatewayId (Config.GatewayID) with the gateway's gateway_id once per
// pass, the first pass included. The sandbox admission policies select namespaces labelled
// openshell.ai/gateway-id=<openshell.gatewayId>: the preflight compares the two at install and upgrade
// only, and a gateway whose ID changes later leaves every policy matching nothing, so they fail open
// without notice (old issue 475). A mismatch sets koinon_reconciler_openshell_gateway_id_mismatch to 1
// and logs a warning when it begins; a match clears it. A failed read keeps the last answer: the
// ConfigMap being unreadable says nothing about the ID.
func (r *Reconciler) checkGatewayID(ctx context.Context) {
	if r.cfg.GatewayIDs == nil || r.cfg.GatewayID == "" {
		return
	}
	got, err := r.cfg.GatewayIDs.GatewayID(ctx)
	if err != nil {
		r.log.Warn("could not read the OpenShell gateway_id; the gateway ID check keeps its last result", "err", err)
		return
	}
	r.gwMu.Lock()
	defer r.gwMu.Unlock()
	mismatch := got != r.cfg.GatewayID
	if mismatch && !r.gwMismatch {
		r.log.Error("the OpenShell gateway's gateway_id differs from openshell.gatewayId: the sandbox admission policies match none of its sandbox namespaces and fail open until they agree",
			"gateway_id", got, "openshell.gatewayId", r.cfg.GatewayID)
	}
	if !mismatch && r.gwMismatch {
		r.log.Info("the OpenShell gateway_id matches openshell.gatewayId again", "gateway_id", got)
	}
	r.gwMismatch = mismatch
	if mismatch {
		r.m.GatewayIDMismatch.Set(1)
	} else {
		r.m.GatewayIDMismatch.Set(0)
	}
}
