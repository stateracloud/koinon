// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/audit"
)

// auditSigningSeedKey is the data key of the audit signing Secret that holds the Ed25519 seed (32 bytes).
const auditSigningSeedKey = "seed"

// kubeSigningKeys is the audit.KeySource of koinon-api: the Ed25519 seed lives in a Secret that only
// koinon-api reads (through the Kubernetes API; no other workload mounts it, and the chart gives no other
// Role a rule that names it). The Secret is created on the first use and never overwritten, and the first
// creation is recorded as keys.generated like the other keys koinon-api makes. A Secret deleted on purpose
// (rotation, docs/ops/audit-checkpoints.md) is made again on the next use, with a new key; the new
// public key is then published by the checkpoint job as audit.signing_key.added.
type kubeSigningKeys struct {
	kube      kubernetes.Interface
	namespace string
	name      string
	audits    *keyAudit
}

// SigningKey reads the seed, creating the Secret when it does not exist. The seed is read at every call,
// once per checkpoint, so a rotation reaches every replica with no restart. It is never logged.
func (k *kubeSigningKeys) SigningKey(ctx context.Context, origin string) (*audit.SigningKey, error) {
	sec, err := k.kube.CoreV1().Secrets(k.namespace).Get(ctx, k.name, metav1.GetOptions{})
	var seed []byte
	switch {
	case err == nil:
		seed = sec.Data[auditSigningSeedKey]
	case apierrors.IsNotFound(err):
		_, err = k.audits.ensureGenerated(ctx, "secret", k.name, func(ctx context.Context) (bool, error) {
			s, created, err := ensureRandomKeySecret(ctx, k.kube, k.namespace, k.name, auditSigningSeedKey)
			seed = s
			return created, err
		})
	}
	if err != nil {
		return nil, fmt.Errorf("the audit signing Secret %s: %w", k.name, err)
	}
	defer clear(seed)
	key, err := audit.NewSigningKey(origin, seed)
	if err != nil {
		return nil, fmt.Errorf("the audit signing Secret %s: %w", k.name, err)
	}
	return key, nil
}
