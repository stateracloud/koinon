// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/procoduck/koinon/internal/project"
)

// KeySecret is an agent's agentgateway key as stored in a Kubernetes Secret. Material is the
// only secret field; it must never be logged.
type KeySecret struct {
	Name     string
	KeyID    string
	KeyHash  string
	Material string
	Labels   map[string]string
}

// SecretStore holds key material. It addresses Secrets by name only (no list), which is all
// the reconciler's RBAC allows.
type SecretStore interface {
	// Get returns the Secret, or found=false when it does not exist.
	Get(ctx context.Context, name string) (s KeySecret, found bool, err error)
	// Put creates or replaces the Secret.
	Put(ctx context.Context, s KeySecret) error
	// Delete removes the Secret. A missing Secret is not an error.
	Delete(ctx context.Context, name string) error
}

// Keys of the Secret.
var (
	secretDataKey     = keyCredential
	annotationKeyID   = project.Domain + "/key-id"
	annotationKeyHash = project.Domain + "/key-hash"
)

// KubeSecrets stores key Secrets in one namespace (Koinon's own).
type KubeSecrets struct {
	Client    kubernetes.Interface
	Namespace string
}

var _ SecretStore = KubeSecrets{}

// Get implements SecretStore.
func (k KubeSecrets) Get(ctx context.Context, name string) (KeySecret, bool, error) {
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return KeySecret{}, false, nil
	}
	if err != nil {
		return KeySecret{}, false, fmt.Errorf("get secret %s/%s: %w", k.Namespace, name, err)
	}
	return KeySecret{
		Name:     name,
		KeyID:    s.Annotations[annotationKeyID],
		KeyHash:  s.Annotations[annotationKeyHash],
		Material: string(s.Data[secretDataKey]),
		Labels:   s.Labels,
	}, true, nil
}

// Put implements SecretStore.
func (k KubeSecrets) Put(ctx context.Context, ks KeySecret) error {
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ks.Name,
			Namespace: k.Namespace,
			Labels:    maps.Clone(ks.Labels),
			Annotations: map[string]string{
				annotationKeyID:   ks.KeyID,
				annotationKeyHash: ks.KeyHash,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{secretDataKey: []byte(ks.Material)},
	}
	secrets := k.Client.CoreV1().Secrets(k.Namespace)
	_, err := secrets.Create(ctx, want, metav1.CreateOptions{FieldManager: ManagedByValue})
	if !apierrors.IsAlreadyExists(err) {
		if err != nil {
			return fmt.Errorf("create secret %s/%s: %w", k.Namespace, ks.Name, err)
		}
		return nil
	}
	cur, err := secrets.Get(ctx, ks.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get secret %s/%s: %w", k.Namespace, ks.Name, err)
	}
	want.ResourceVersion = cur.ResourceVersion
	if _, err := secrets.Update(ctx, want, metav1.UpdateOptions{FieldManager: ManagedByValue}); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", k.Namespace, ks.Name, err)
	}
	return nil
}

// Delete implements SecretStore.
func (k KubeSecrets) Delete(ctx context.Context, name string) error {
	err := k.Client.CoreV1().Secrets(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete secret %s/%s: %w", k.Namespace, name, err)
	}
	return nil
}
