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
	// Wrappers unwrap the stored KEKs and wrap new ones; the zero value is the local Secret-held
	// KEK (secretstore.Local).
	Wrappers secretstore.Wrappers
}

// Load implements Store. When the KEKs cannot be unwrapped it returns the error with the stored
// KEKs and the active ID (no keys), so the caller can probe the wrapping key.
func (k KubeStore) Load(ctx context.Context) (KeySet, error) {
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, k.Name, metav1.GetOptions{})
	if err != nil {
		return KeySet{}, fmt.Errorf("read the KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	ks, err := openKeySet(ctx, k.Wrappers, s.Data)
	return k.finishLoad(s, ks, err)
}

// LoadWithoutActive is Load for recovering from a lost active KEK (issue #255): the Secret's
// active entry may be missing or name a KEK the Secret no longer holds, which Load refuses. Every
// KEK the Secret does hold must still unwrap. The result's Active is the entry as stored and may
// not be in Keys; only the key-admin Job uses it, to put a new active KEK there.
func (k KubeStore) LoadWithoutActive(ctx context.Context) (KeySet, error) {
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, k.Name, metav1.GetOptions{})
	if err != nil {
		return KeySet{}, fmt.Errorf("read the KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	stored, active, err := secretstore.ParseStoredKeys(s.Data)
	if err != nil {
		return KeySet{}, fmt.Errorf("KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	keys, err := k.Wrappers.UnwrapAll(ctx, stored)
	ks := KeySet{Stored: stored, Active: active}
	if err == nil {
		ks.Keys = keys
	}
	return k.finishLoad(s, ks, err)
}

// finishLoad adds the activation time of s to ks and names the Secret in err.
func (k KubeStore) finishLoad(s *corev1.Secret, ks KeySet, err error) (KeySet, error) {
	if v := s.Annotations[ActivatedAnnotation]; v != "" {
		ks.ActivatedAt, _ = time.Parse(time.RFC3339, v)
	}
	if err != nil {
		return ks, fmt.Errorf("KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	return ks, nil
}

// openKeySet parses the Secret's data and unwraps its KEKs.
func openKeySet(ctx context.Context, ws secretstore.Wrappers, data map[string][]byte) (KeySet, error) {
	stored, active, err := secretstore.ParseKeySet(data)
	if err != nil {
		return KeySet{}, err
	}
	keys, err := ws.UnwrapAll(ctx, stored)
	if err != nil {
		// Stored and Active come back with the error, so a caller can still probe the wrapping key.
		return KeySet{Stored: stored, Active: active}, err
	}
	return KeySet{Keys: keys, Stored: stored, Active: active}, nil
}

// encodeKeySet is the Secret data of ks: the stored form of each KEK it was loaded with, and new
// KEKs wrapped by the primary wrapper. A loaded KEK is never wrapped again, so changing the key set
// needs encrypt rights on the wrapping key only when a KEK is added.
func encodeKeySet(ctx context.Context, ws secretstore.Wrappers, ks KeySet) (map[string][]byte, error) {
	data := map[string][]byte{secretstore.ActiveFile: []byte(ks.Active)}
	for id, key := range ks.Keys {
		s, ok := ks.Stored[id]
		if !ok {
			var err error
			if s, err = ws.Wrap(ctx, id, key); err != nil {
				return nil, err
			}
		}
		v, err := s.Encode()
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", id, err)
		}
		data[id] = v
	}
	return data, nil
}

// Save implements Store.
func (k KubeStore) Save(ctx context.Context, ks KeySet) error {
	secrets := k.Client.CoreV1().Secrets(k.Namespace)
	s, err := secrets.Get(ctx, k.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the KEK Secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	data, err := encodeKeySet(ctx, k.Wrappers, ks)
	if err != nil {
		return err
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
type DirStore struct {
	Dir string
	// Wrappers unwrap the stored KEKs; the zero value reads local KEKs only.
	Wrappers secretstore.Wrappers
}

// Load implements Store.
func (d DirStore) Load(ctx context.Context) (KeySet, error) {
	stored, active, err := secretstore.ReadDir(d.Dir)
	if err != nil {
		return KeySet{}, err
	}
	keys, err := d.Wrappers.UnwrapAll(ctx, stored)
	if err != nil {
		return KeySet{Stored: stored, Active: active}, fmt.Errorf("%s: %w", d.Dir, err)
	}
	return KeySet{Keys: keys, Stored: stored, Active: active}, nil
}

// Save implements Store.
func (DirStore) Save(context.Context, KeySet) error {
	return errors.New("the key set is read from a mounted directory and cannot be changed here; set --kek-secret to change it through the Kubernetes API")
}

// Ensure creates the KEK Secret with one new active KEK, wrapped by the primary wrapper, when it
// does not exist, and reports whether it did. It never overwrites an existing Secret, so a restart,
// a second replica or a re-render of the chart cannot replace the keys (which would make every
// stored secret unreadable). Two replicas starting together race on Create; the loser reads the
// winner's Secret.
//
// It also refuses to create the Secret when the database already holds envelopes (hasValues): a new
// KEK could not open any of them, so a deleted Secret must be restored from backup, not replaced.
func (k KubeStore) Ensure(ctx context.Context, now time.Time, hasValues func(context.Context) (bool, error)) (bool, error) {
	return k.EnsureWith(ctx, now, hasValues, nil)
}

// EnsureWith is [KubeStore.Ensure] that calls beforeCreate, when it is not nil, once it has decided
// to create the Secret and before it does: the key-admin Job and koinon-api set the install's
// bound-only marker there (old issue 615), so the first KEK of an install never exists without it. When
// beforeCreate fails, nothing is created and the next run decides again.
func (k KubeStore) EnsureWith(ctx context.Context, now time.Time, hasValues func(context.Context) (bool, error), beforeCreate func(context.Context) error) (bool, error) {
	client, namespace, name := k.Client, k.Namespace, k.Name
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
			return false, fmt.Errorf("%w: the KEK Secret %s/%s is missing but the database holds encrypted values that only its keys can open; restore the Secret from your backup ("+project.SecretKeysDocURL+"). Nothing was created", ErrKEKMissing, namespace, name)
		}
	}
	if beforeCreate != nil {
		if err := beforeCreate(ctx); err != nil {
			return false, fmt.Errorf("before creating the KEK Secret %s/%s: %w", namespace, name, err)
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
	defer clear(key)
	data, err := encodeKeySet(ctx, k.Wrappers, KeySet{Keys: map[string][]byte{id: key}, Active: id})
	if err != nil {
		return false, err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels:      map[string]string{project.Domain + "/managed-by": "koinon-api"},
			Annotations: map[string]string{ActivatedAnnotation: now.UTC().Format(time.RFC3339)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}, metav1.CreateOptions{FieldManager: "koinon-api"})
	if apierrors.IsAlreadyExists(err) {
		// Someone else created it meanwhile: it is used as found. If that is an old Secret with KEKs
		// stored unbound (GitOps or a backup restore on a new install), the marker set above makes the
		// next load refuse them with secretstore.ErrBoundOnly, whose message says to delete the Secret
		// and rerun the key-admin Job (old issue 615).
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create the KEK Secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}
