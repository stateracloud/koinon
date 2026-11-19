// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// GVisorSandboxConfigName is the SandboxConfig that points at the staged tarball. It is a class default
// (annotation below): Substrate picks the newest default of a class, and this one is made after ate-setup
// applies `gvisor-default`, so the gVisor worker pools use it unless a WorkerPool names a configRef.
const GVisorSandboxConfigName = "koinon-gvisor"

// sandboxClassDefaultAnnotation is SandboxConfigClassDefaultAnnotation of Substrate (api/infra/k8s/ate/v1).
const sandboxClassDefaultAnnotation = "sandboxconfig.ate.dev/is-class-default"

var sandboxConfigGVR = schema.GroupVersionResource{Group: "ate.dev", Version: "v1", Resource: "sandboxconfigs"}

// gvisorVersionName is the version of the SandboxConfig for the pinned release ("gvisor-20261102-0"). It is a DNS
// label. A snapshot restores its memory only on the version it was taken with, so a bump adds a version
// and keeps the old ones.
func gvisorVersionName() string {
	return "gvisor-" + strings.ReplaceAll(GVisorRelease, ".", "-")
}

// GVisorVersion is the version entry of a SandboxConfig: the staged tarballs with the pinned digests, as
// s3://<bucket>/<key> URLs. The digest is the one the installer checked before it wrote the object.
func GVisorVersion(bucket string, assets []StagedAsset) map[string]any {
	byArch := map[string]any{}
	for _, a := range assets {
		byArch[a.Arch] = map[string]any{"gvisor": map[string]any{"url": "s3://" + bucket + "/" + a.Key, "sha256": a.SHA256}}
	}
	return map[string]any{"name": gvisorVersionName(), "state": "Enabled", "assets": byArch}
}

// GVisorSandboxConfig is the SandboxConfig the installer applies for a new cluster.
func GVisorSandboxConfig(bucket string, assets []StagedAsset) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1",
		"kind":       "SandboxConfig",
		"metadata": map[string]any{
			"name":        GVisorSandboxConfigName,
			"annotations": map[string]any{sandboxClassDefaultAnnotation: "true"},
			"labels":      map[string]any{"app.kubernetes.io/part-of": "koinon"},
		},
		"spec": map[string]any{
			"sandboxClass":   "gvisor",
			"defaultVersion": gvisorVersionName(),
			"versions":       []any{GVisorVersion(bucket, assets)},
		},
	}}
}

// ApplySandboxConfig creates the SandboxConfig, or adds the version of the pinned release to an existing
// one and makes it the default. It is idempotent: the same input changes nothing. It refuses (ErrDigestMismatch)
// a config that already has the version name with other assets: a version is never rewritten, because
// snapshots depend on it. It returns whether it changed the cluster.
func ApplySandboxConfig(ctx context.Context, dyn dynamic.Interface, bucket string, assets []StagedAsset) (bool, error) {
	client := dyn.Resource(sandboxConfigGVR)
	want := GVisorSandboxConfig(bucket, assets)
	have, err := client.Get(ctx, GVisorSandboxConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := client.Create(ctx, want, metav1.CreateOptions{}); err != nil {
			return false, fmt.Errorf("create SandboxConfig %s: %w", GVisorSandboxConfigName, err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read SandboxConfig %s: %w", GVisorSandboxConfigName, err)
	}
	versions, _, err := unstructured.NestedSlice(have.Object, "spec", "versions")
	if err != nil {
		return false, fmt.Errorf("SandboxConfig %s: spec.versions: %w", GVisorSandboxConfigName, err)
	}
	newVersion := GVisorVersion(bucket, assets)
	changed := false
	found := false
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok || m["name"] != gvisorVersionName() {
			continue
		}
		found = true
		if !reflect.DeepEqual(m["assets"], newVersion["assets"]) {
			return false, fmt.Errorf("%w: SandboxConfig %s has version %s with other assets than the staged ones. The installer does not rewrite a version. Delete the SandboxConfig if it is wrong, then run the installer again", ErrDigestMismatch, GVisorSandboxConfigName, gvisorVersionName())
		}
	}
	if !found {
		versions = append(versions, newVersion)
		changed = true
	}
	if err := unstructured.SetNestedSlice(have.Object, versions, "spec", "versions"); err != nil {
		return false, err
	}
	if d, _, _ := unstructured.NestedString(have.Object, "spec", "defaultVersion"); d != gvisorVersionName() {
		_ = unstructured.SetNestedField(have.Object, gvisorVersionName(), "spec", "defaultVersion")
		changed = true
	}
	if a := have.GetAnnotations(); a[sandboxClassDefaultAnnotation] != "true" {
		if a == nil {
			a = map[string]string{}
		}
		a[sandboxClassDefaultAnnotation] = "true"
		have.SetAnnotations(a)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if _, err := client.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("update SandboxConfig %s: %w", GVisorSandboxConfigName, err)
	}
	return true, nil
}

// gvisorOrderWarning returns a warning when `gvisor-default` is as new as or newer than the Koinon config.
// Substrate picks the newest class default, so then the pools would use gs://gvisor again. This happens after
// ate-setup is cleaned up and run again while the Koinon config stays. The fix is to delete the Koinon
// SandboxConfig and run the installer again: the version name and the assets stay the same.
func gvisorOrderWarning(ctx context.Context, dyn dynamic.Interface) string {
	client := dyn.Resource(sandboxConfigGVR)
	ours, err := client.Get(ctx, GVisorSandboxConfigName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	theirs, err := client.Get(ctx, "gvisor-default", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	if theirs.GetCreationTimestamp().Time.Before(ours.GetCreationTimestamp().Time) {
		return ""
	}
	if theirs.GetAnnotations()[sandboxClassDefaultAnnotation] != "true" {
		return ""
	}
	return fmt.Sprintf("warning: SandboxConfig gvisor-default is newer than %s, so Substrate may use gs://gvisor again. Delete SandboxConfig %s and run the installer again", GVisorSandboxConfigName, GVisorSandboxConfigName)
}
