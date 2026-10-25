// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
)

// Store is the Kubernetes API as the driver uses it: server-side apply, list by label, delete and
// get, for the kinds in gvrs.
type Store interface {
	Apply(ctx context.Context, o Object) (*unstructured.Unstructured, error)
	Get(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error)
	List(ctx context.Context, kind, namespace, selector string) ([]*unstructured.Unstructured, error)
	Delete(ctx context.Context, kind, namespace, name string) error
}

// DynamicStore is a Store over a dynamic client.
type DynamicStore struct {
	Client dynamic.Interface
	// FieldManager is recorded in managedFields. Default "koinon-reconciler".
	FieldManager string
}

var _ Store = DynamicStore{}

// Apply implements Store with a forced server-side apply, so Koinon owns every field it renders and
// a field someone else added (a label, a status) is left alone.
func (d DynamicStore) Apply(ctx context.Context, o Object) (*unstructured.Unstructured, error) {
	raw, err := json.Marshal(o.U.Object)
	if err != nil {
		return nil, err
	}
	mgr := d.FieldManager
	if mgr == "" {
		mgr = defaultManagedBy
	}
	out, err := d.Client.Resource(gvrs[o.U.GetKind()]).Namespace(o.U.GetNamespace()).Patch(ctx, o.U.GetName(),
		types.ApplyPatchType, raw, metav1.PatchOptions{FieldManager: mgr, Force: ptr.To(true)})
	if err != nil {
		return nil, fmt.Errorf("apply %s: %w", o.Ref(), err)
	}
	return out, nil
}

// Get implements Store.
func (d DynamicStore) Get(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error) {
	return d.Client.Resource(gvrs[kind]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// List implements Store.
func (d DynamicStore) List(ctx context.Context, kind, namespace, selector string) ([]*unstructured.Unstructured, error) {
	l, err := d.Client.Resource(gvrs[kind]).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, len(l.Items))
	for i := range l.Items {
		out[i] = &l.Items[i]
	}
	return out, nil
}

// Delete implements Store; a missing object is not an error.
func (d DynamicStore) Delete(ctx context.Context, kind, namespace, name string) error {
	err := d.Client.Resource(gvrs[kind]).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
