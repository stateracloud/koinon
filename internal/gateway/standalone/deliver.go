// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Deliverer writes a rendered config where agentgateway reads it.
type Deliverer interface {
	Deliver(ctx context.Context, config []byte, rev gateway.Revision) error
}

// ConfigMapDeliverer writes the config into one data key of a ConfigMap, which agentgateway mounts
// as its config file.
//
// It sends a JSON merge patch that touches only data[Key]. It never applies the whole object
// (server-side apply or update), so labels, annotations and other data keys owned by an operator
// are left alone, and a GitOps tool that ignores differences in this data key stays in sync.
//
// The chart does not render the ConfigMap: a render cannot read the live object (helm template,
// Argo CD, Flux), so rendering it would reset the config on every sync. When the patch finds no
// ConfigMap, the deliverer creates it with the full config. With Owner set, the new ConfigMap has
// an ownerReference to that Deployment (not the controller, no blockOwnerDeletion), so uninstalling
// the release garbage-collects it. The gateway pod waits for the ConfigMap, so the first delivery is
// what lets it start.
type ConfigMapDeliverer struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
	// Key is the data key, default "config.yaml".
	Key string
	// FieldManager is recorded in managedFields, default "koinon".
	FieldManager string
	// Labels are put on a ConfigMap this deliverer creates. Nothing is relabelled later.
	Labels map[string]string
	// OwnerDeployment is the name of the gateway Deployment that owns a created ConfigMap. Empty:
	// the ConfigMap has no owner.
	OwnerDeployment string
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
	configMaps := d.Client.CoreV1().ConfigMaps(d.Namespace)
	_, err = configMaps.Patch(ctx, d.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: manager})
	if apierrors.IsNotFound(err) {
		err = d.create(ctx, key, manager, config)
		if apierrors.IsAlreadyExists(err) {
			// Created by someone else between the patch and the create.
			_, err = configMaps.Patch(ctx, d.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: manager})
		}
	}
	if err != nil {
		return fmt.Errorf("standalone: write configmap %s/%s: %w", d.Namespace, d.Name, err)
	}
	return nil
}

func (d ConfigMapDeliverer) create(ctx context.Context, key, manager string, config []byte) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace, Labels: d.Labels},
		Data:       map[string]string{key: string(config)},
	}
	if d.OwnerDeployment != "" {
		dep, err := d.Client.AppsV1().Deployments(d.Namespace).Get(ctx, d.OwnerDeployment, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read the owner Deployment %s: %w", d.OwnerDeployment, err)
		}
		cm.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID,
			Controller: ptr.To(false), BlockOwnerDeletion: ptr.To(false),
		}}
	}
	_, err := d.Client.CoreV1().ConfigMaps(d.Namespace).Create(ctx, cm, metav1.CreateOptions{FieldManager: manager})
	return err
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
