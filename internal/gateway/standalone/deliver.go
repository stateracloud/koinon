// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Deliverer writes a rendered config where agentgateway reads it.
type Deliverer interface {
	Deliver(ctx context.Context, config []byte, rev gateway.Revision) error
}

// ConfigMapDeliverer writes the config into one data key of an existing
// ConfigMap, which agentgateway mounts as its config file.
//
// It sends a JSON merge patch that touches only data[Key]. It never applies
// the whole object (server-side apply or update), so labels, annotations and
// other data keys owned by Helm, Argo CD or an operator are left alone, and a
// GitOps tool that ignores differences in this data key stays in sync (in the
// spike the platform ConfigMap belonged to an Argo CD application with
// ignoreDifferences on its data). The ConfigMap itself is created by the chart.
type ConfigMapDeliverer struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
	// Key is the data key, default "config.yaml".
	Key string
	// FieldManager is recorded in managedFields, default "koinon".
	FieldManager string
}

// Deliver implements Deliverer.
func (d ConfigMapDeliverer) Deliver(ctx context.Context, config []byte, _ gateway.Revision) error {
	key := d.Key
	if key == "" {
		key = "config.yaml"
	}
	manager := d.FieldManager
	if manager == "" {
		manager = "koinon"
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{key: string(config)}})
	if err != nil {
		return err
	}
	_, err = d.Client.CoreV1().ConfigMaps(d.Namespace).Patch(ctx, d.Name, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: manager})
	if err != nil {
		return fmt.Errorf("standalone: patch configmap %s/%s: %w", d.Namespace, d.Name, err)
	}
	return nil
}

// FileDeliverer writes the config to a local file by atomic rename, for an
// agentgateway that reads a file directly (local development, tests).
type FileDeliverer struct {
	Path string
}

// Deliver implements Deliverer.
func (d FileDeliverer) Deliver(_ context.Context, config []byte, _ gateway.Revision) error {
	dir, base := filepath.Split(d.Path)
	f, err := os.CreateTemp(dir, "."+base+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(config); err != nil {
		_ = f.Close()
		return err
	}
	// agentgateway may run as another user; the config holds no secrets.
	if err := f.Chmod(0o644); err != nil { //nolint:gosec // see above
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), d.Path)
}
