// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/preflight"
)

// DefaultMaxRecreatesPerPass is how many sandboxes may be deleted at once to recreate them with a
// changed spec, and how many more per ResyncInterval (ADR 0085 decision 9; a token bucket since
// issue 573).
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
// Before the first successful read the image is unknown, and supervisor-driven recreates wait.
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

// supervisorImage is the image last read and whether one was read. Without a source nothing is
// configured: the image is empty and counts as known. Until the first successful read it is unknown,
// and only supervisor-driven recreates wait (supervisorChanged); every other spec change, creating,
// starting, stopping and policy work go on, so an unreadable ConfigMap never blocks the fleet.
func (r *Reconciler) supervisorImage() (img string, known bool) {
	if r.cfg.Supervisor == nil {
		return "", true
	}
	r.supMu.Lock()
	defer r.supMu.Unlock()
	return r.supImage, r.supKnown
}

// supervisorDigest is the label value for a supervisor image: the first 16 hex characters of its
// sha256, so the value is a valid Kubernetes label value whatever the registry path (an image ref
// has '/' and ':' and can pass 63 characters). An empty image (none configured) stays empty.
func supervisorDigest(img string) string {
	if img == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(img))
	return hex.EncodeToString(sum[:8])
}

// supervisorChanged says whether a sandbox runs a supervisor other than the gateway's current one.
// False while the current image is unknown. A sandbox without the label (created before it, or while
// the image was unreadable) counts as changed once an image is configured, so an existing fleet
// converges with one recreate each, under the per-pass cap and the usual gates.
func (r *Reconciler) supervisorChanged(sb execution.Sandbox) bool {
	img, known := r.supervisorImage()
	if !known {
		return false
	}
	want := supervisorDigest(img)
	have, ok := sb.Labels[LabelSupervisorImage]
	if !ok {
		return want != ""
	}
	return have != want
}

// takeRecreate says whether the agent may start a recreate now. The bucket holds
// MaxRecreatesPerPass tokens and gets them back at MaxRecreatesPerPass per ResyncInterval, which is
// the rate of the per-pass cap it replaces (a pass ran at least once per interval). Work items run
// in parallel and outside any pass, so the pass is no unit to count in (ADR 0085 decision 9: an
// OpenShell upgrade must not recreate the whole fleet at once).
//
// An agent keeps the token it took until it has deleted its sandbox (recreateStarted): its home gate
// may wait many passes for a backup, and each of them asks again without spending a second token.
// At most MaxRecreatesPerPass agents hold one at a time, so at most that many sit in the gate and
// delete together. A hold ends when the agent's work item ends without wanting a recreate
// (recreateReleased): the spec went back, the agent was archived or deleted, a run holds it.
func (r *Reconciler) takeRecreate(key agentKey) bool {
	limit := r.cfg.MaxRecreatesPerPass
	if limit < 0 {
		return true
	}
	now := time.Now()
	if r.cfg.Now != nil {
		now = r.cfg.Now()
	}
	interval := r.cfg.ResyncInterval
	if interval <= 0 {
		interval = DefaultResyncInterval
	}
	r.bucketMu.Lock()
	defer r.bucketMu.Unlock()
	if r.bucketAt.IsZero() {
		r.bucketTokens = float64(limit)
	} else if d := now.Sub(r.bucketAt); d > 0 {
		r.bucketTokens = min(float64(limit), r.bucketTokens+d.Seconds()*float64(limit)/interval.Seconds())
	}
	r.bucketAt = now
	if r.recreating[key] {
		return true
	}
	// The bucket bounds the rate; the holds bound how many agents are in the recreate path at once.
	// Without the second bound, N slow intervals would leave about N*limit agents behind their home
	// gates, and they could all delete together.
	if len(r.recreating) >= limit || r.bucketTokens < 1 {
		return false
	}
	r.bucketTokens--
	if r.recreating == nil {
		r.recreating = map[agentKey]bool{}
	}
	r.recreating[key] = true
	return true
}

// recreateStarted ends the agent's hold on its recreate token: its sandbox is deleted.
func (r *Reconciler) recreateStarted(key agentKey) {
	r.bucketMu.Lock()
	defer r.bucketMu.Unlock()
	delete(r.recreating, key)
}

// recreateReleased ends the agent's hold without a recreate and gives its token back: the agent did
// not delete anything, so the allowance it took is unspent. A no-op for an agent without a hold.
func (r *Reconciler) recreateReleased(key agentKey) {
	r.bucketMu.Lock()
	defer r.bucketMu.Unlock()
	if !r.recreating[key] {
		return
	}
	delete(r.recreating, key)
	if limit := r.cfg.MaxRecreatesPerPass; limit > 0 {
		r.bucketTokens = min(float64(limit), r.bucketTokens+1)
	}
}

// releaseRecreatesExcept ends the holds of agents that are not in keep: agents that were archived or
// deleted since they took a hold have no work item that could release it. An agent whose item runs
// (busy) keeps its hold: the item may be between takeRecreate and the delete, and a refunded token
// there would let the delete pass the cap. The item ends the hold itself, or the next pass does.
func (r *Reconciler) releaseRecreatesExcept(keep map[agentKey]bool, busy func(agentKey) bool) {
	r.bucketMu.Lock()
	var gone []agentKey
	for k := range r.recreating {
		if !keep[k] && (busy == nil || !busy(k)) {
			gone = append(gone, k)
		}
	}
	r.bucketMu.Unlock()
	for _, k := range gone {
		r.recreateReleased(k)
	}
}
