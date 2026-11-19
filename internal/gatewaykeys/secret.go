// SPDX-License-Identifier: Apache-2.0

package gatewaykeys

import (
	"context"
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// SecretStore is the Secret that holds the wrapped private keys (<release>-gateway-recipient-key): one
// entry per key, named by its key ID, in the form of a wrapped KEK ([secretstore.StoredKEK.Encode]). Only
// the gateway mounts it. Only the key-admin identity writes it (the chart's kek-guard rule).
type SecretStore struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

// Read returns the entries of the Secret and whether it exists.
func (s SecretStore) Read(ctx context.Context) (map[string]secretstore.StoredKEK, bool, error) {
	sec, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	stored, _, err := secretstore.ParseStoredKeys(sec.Data)
	if err != nil {
		return nil, true, fmt.Errorf("the Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return stored, true, nil
}

// Put adds the entry of a key, creating the Secret when it does not exist.
func (s SecretStore) Put(ctx context.Context, keyID string, k secretstore.StoredKEK) error {
	enc, err := k.Encode()
	if err != nil {
		return err
	}
	secrets := s.Client.CoreV1().Secrets(s.Namespace)
	for range 3 {
		cur, err := secrets.Get(ctx, s.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace, Labels: map[string]string{
					"app.kubernetes.io/part-of": "koinon", project.Domain + "/component": "gateway-recipient-key",
				}},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{keyID: enc},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		data := maps.Clone(cur.Data)
		if data == nil {
			data = map[string][]byte{}
		}
		data[keyID] = enc
		cur.Data = data
		if _, err = secrets.Update(ctx, cur, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return errors.New("the Secret changed under the key-admin Job three times; run it again")
}

// Delete removes the entry of a key. A missing entry or Secret is not an error: a rerun finishes the job.
func (s SecretStore) Delete(ctx context.Context, keyID string) error {
	secrets := s.Client.CoreV1().Secrets(s.Namespace)
	for range 3 {
		cur, err := secrets.Get(ctx, s.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, ok := cur.Data[keyID]; !ok {
			return nil
		}
		cur.Data = maps.Clone(cur.Data)
		delete(cur.Data, keyID)
		if _, err = secrets.Update(ctx, cur, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return errors.New("the Secret changed under the key-admin Job three times; run it again")
}
