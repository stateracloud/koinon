// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/secretstore"
)

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
