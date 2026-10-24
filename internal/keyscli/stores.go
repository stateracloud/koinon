// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// ErrKEKMissing means the KEK Secret is gone while values encrypted with it exist.
var ErrKEKMissing = errors.New("keyscli: KEK Secret missing")

// ActivatedAnnotation records when the active KEK was activated, as RFC 3339.
var ActivatedAnnotation = project.Domain + "/kek-activated-at"

// KubeStore keeps the key set in a Kubernetes Secret.
type KubeStore struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

// Load implements Store.
func (k KubeStore) Load(ctx context.Context) (KeySet, error) {
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, k.Name, metav1.GetOptions{})
	if err != nil {
		return KeySet{}, fmt.Errorf("read the KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	keys, active, err := secretstore.ParseKeySet(s.Data)
	if err != nil {
		return KeySet{}, fmt.Errorf("KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	ks := KeySet{Keys: keys, Active: active}
	if v := s.Annotations[ActivatedAnnotation]; v != "" {
		ks.ActivatedAt, _ = time.Parse(time.RFC3339, v)
	}
	return ks, nil
}

// Save implements Store.
func (k KubeStore) Save(ctx context.Context, ks KeySet) error {
	secrets := k.Client.CoreV1().Secrets(k.Namespace)
	s, err := secrets.Get(ctx, k.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	data := map[string][]byte{secretstore.ActiveFile: []byte(ks.Active)}
	for id, key := range ks.Keys {
		data[id] = secretstore.EncodeKey(key)
	}
	s.Data, s.StringData = data, nil
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	if !ks.ActivatedAt.IsZero() {
		s.Annotations[ActivatedAnnotation] = ks.ActivatedAt.UTC().Format(time.RFC3339)
	}
	if _, err := secrets.Update(ctx, s, metav1.UpdateOptions{FieldManager: "koinon-api-keys"}); err != nil {
		return fmt.Errorf("update the KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	return nil
}

// DirStore reads the key set from a mounted directory. It cannot save.
type DirStore struct{ Dir string }

// Load implements Store.
func (d DirStore) Load(context.Context) (KeySet, error) {
	keys, active, err := secretstore.ReadDir(d.Dir)
	if err != nil {
		return KeySet{}, err
	}
	return KeySet{Keys: keys, Active: active}, nil
}

// Save implements Store.
func (DirStore) Save(context.Context, KeySet) error {
	return errors.New("the key set is read from a mounted directory and cannot be changed here; set --kek-secret to change it through the Kubernetes API")
}

// EnsureKubeSecret creates the KEK Secret with one new active KEK when it does not exist, and
// reports whether it did. It never overwrites an existing Secret, so a restart, a second replica
// or a re-render of the chart cannot replace the keys (which would make every stored secret
// unreadable). Two replicas starting together race on Create; the loser reads the winner's Secret.
//
// It also refuses to create the Secret when the database already holds envelopes (hasValues): a new
// KEK could not open any of them, so a deleted Secret must be restored from backup, not replaced.
func EnsureKubeSecret(ctx context.Context, client kubernetes.Interface, namespace, name string, now time.Time, hasValues func(context.Context) (bool, error)) (bool, error) {
	secrets := client.CoreV1().Secrets(namespace)
	if _, err := secrets.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("read the KEK Secret %s/%s: %w", namespace, name, err)
	}
	if hasValues != nil {
		used, err := hasValues(ctx)
		if err != nil {
			return false, fmt.Errorf("check for stored secrets before creating the KEK Secret: %w", err)
		}
		if used {
			return false, fmt.Errorf("%w: the KEK Secret %s/%s is missing but the database holds encrypted values that only its keys can open; restore the Secret from your backup (docs/ops/secret-keys.md). Nothing was created", ErrKEKMissing, namespace, name)
		}
	}
	id, err := NewID(now)
	if err != nil {
		return false, err
	}
	key := make([]byte, secretstore.KeySize)
	if _, err := rand.Read(key); err != nil {
		return false, err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels:      map[string]string{project.Domain + "/managed-by": "koinon-api"},
			Annotations: map[string]string{ActivatedAnnotation: now.UTC().Format(time.RFC3339)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{id: secretstore.EncodeKey(key), secretstore.ActiveFile: []byte(id)},
	}, metav1.CreateOptions{FieldManager: "koinon-api"})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create the KEK Secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}
