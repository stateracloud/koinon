// SPDX-License-Identifier: Apache-2.0

// Package imagecacheca lets atelet trust the certificate of Koinon's image cache (issue #1191).
//
// atelet pulls every actor image through the in-cluster cache (ADR 0120 section 6.9) over TLS. Substrate has no CA
// option, but atelet is a Go program: it reads the system roots from /etc/ssl/certs/ca-certificates.crt and then adds
// every certificate file in the directories of SSL_CERT_DIR (crypto/x509 loadOnDiskRoots). So the installer patch
// (internal/substrateinstall) mounts the ConfigMap ConfigMapName and sets SSL_CERT_DIR to MountPath, and this package
// puts the PUBLIC CA certificate into that ConfigMap. The chart's hook Job calls Publish ("koinon-api imagecache-ca").
//
// A pod reads its trust roots once, at start. So Publish also stamps a hash of the certificates on the pod template of
// the atelet DaemonSet and of the cache Deployment: a rotated CA or leaf rolls both. The stamp is a field Helm and
// kubectl never wrote, so their three-way merge keeps it on the next apply.
package imagecacheca

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMapName is the ConfigMap in the Substrate namespace that holds the CA. The name is fixed: one Substrate
	// install has one atelet, and the installer does not know the release name.
	ConfigMapName = "koinon-imagecache-ca"
	// CAKey is the key of the CA certificate in the ConfigMap and in the TLS Secret.
	CAKey = "ca.crt"
	// MountPath is where atelet mounts the ConfigMap. It is the value of SSL_CERT_DIR.
	MountPath = "/etc/koinon/imagecache-ca"
	// VolumeName is the name of the volume in the atelet pod.
	VolumeName = "koinon-imagecache-ca"
	// CertDirs is the value of SSL_CERT_DIR: the mount, then the default directory of the image, which setting the
	// variable would otherwise replace.
	CertDirs = MountPath + ":/etc/ssl/certs"
	// EnvName is the environment variable that points Go's system roots at MountPath.
	EnvName = "SSL_CERT_DIR"
	// DaemonSetName is the node agent of Substrate. The name is fixed by the pinned manifests.
	DaemonSetName = "atelet"
	// AteletContainer is the container of the atelet pod.
	AteletContainer = "atelet"
	// AteletAnnotation is the pod-template annotation that holds the hash of the CA. A new hash rolls atelet.
	AteletAnnotation = "koinon.dev/imagecache-ca-sha256"
	// CacheAnnotation is the pod-template annotation that holds the hash of the cache certificates. A new hash rolls
	// the cache pod, which reads its certificate at start.
	CacheAnnotation = "koinon.dev/imagecache-tls-sha256"

	tlsCertKey = "tls.crt"
)

// ErrCA is returned for a TLS Secret whose CA cannot be published.
var ErrCA = errors.New("image cache CA")

// Params says which objects to read and change.
type Params struct {
	// Secret is the TLS Secret of the cache in Namespace (the generated one or imageCache.tls.existingSecret).
	Secret string
	// Namespace is the release namespace.
	Namespace string
	// SubstrateNamespace is the namespace of Substrate (ate-system).
	SubstrateNamespace string
	// Deployment is the cache Deployment in Namespace. It is optional: it does not exist on the first install.
	Deployment string
}

// Publish copies the public CA of the cache TLS Secret into the ConfigMap of the Substrate namespace and stamps the
// atelet DaemonSet and the cache Deployment with the hashes of the certificates. It writes only certificates: the
// private key of the Secret is never read into the ConfigMap, and a ca.crt that holds anything but certificates is
// refused. It is safe to run again: unchanged hashes change nothing, so nothing restarts.
func Publish(ctx context.Context, client kubernetes.Interface, p Params, out io.Writer) error {
	secret, err := client.CoreV1().Secrets(p.Namespace).Get(ctx, p.Secret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read Secret %s/%s: %w", p.Namespace, p.Secret, err)
	}
	ca, err := CertificatesOnly(secret.Data[CAKey])
	if err != nil {
		return fmt.Errorf("%w: Secret %s/%s key %s: %w", ErrCA, p.Namespace, p.Secret, CAKey, err)
	}
	leaf := secret.Data[tlsCertKey]
	if len(leaf) == 0 {
		return fmt.Errorf("%w: Secret %s/%s has no %s", ErrCA, p.Namespace, p.Secret, tlsCertKey)
	}
	caSum := Sum(ca)
	tlsSum := Sum(leaf, ca)
	// The installer owns the trust patch (EnsureTrust, every route). The admission policy of PR 1193 lets this Job change
	// only the hash annotation of atelet, so an atelet without the trust would pull with no trust while this Job reports
	// success: fail with the way out instead.
	lacks, err := ateletLacksTrust(ctx, client, p.SubstrateNamespace)
	if err != nil {
		return err
	}
	if lacks {
		return fmt.Errorf("%w: DaemonSet %s/%s does not trust the image cache CA (no volume %s, mount or %s): run koinon-api substrate install, which adds it on every route",
			ErrCA, p.SubstrateNamespace, DaemonSetName, VolumeName, EnvName)
	}
	if err := upsertConfigMap(ctx, client, p.SubstrateNamespace, ca, out); err != nil {
		return err
	}
	// atelet must exist: the installer (koinon-api substrate install) runs before helm install. A missing one is an
	// error with the way out, not a skipped step: the cache would be unreachable for every actor.
	if err := stamp(ctx, out, "DaemonSet "+p.SubstrateNamespace+"/"+DaemonSetName, AteletAnnotation, caSum, true,
		func(ctx context.Context) (map[string]string, error) {
			ds, err := client.AppsV1().DaemonSets(p.SubstrateNamespace).Get(ctx, DaemonSetName, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return ds.Spec.Template.Annotations, nil
		}, func(ctx context.Context, patch []byte) error {
			_, err := client.AppsV1().DaemonSets(p.SubstrateNamespace).Patch(ctx, DaemonSetName, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
			return err
		}); err != nil {
		return fmt.Errorf("%w; run koinon-api substrate install before helm install", err)
	}
	if p.Deployment == "" {
		return nil
	}
	// The Deployment is created after this hook on the first install and reads the Secret when its pod starts.
	return stamp(ctx, out, "Deployment "+p.Namespace+"/"+p.Deployment, CacheAnnotation, tlsSum, false,
		func(ctx context.Context) (map[string]string, error) {
			d, err := client.AppsV1().Deployments(p.Namespace).Get(ctx, p.Deployment, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return d.Spec.Template.Annotations, nil
		}, func(ctx context.Context, patch []byte) error {
			_, err := client.AppsV1().Deployments(p.Namespace).Patch(ctx, p.Deployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
			return err
		})
}

// CertificatesOnly returns the PEM of the certificates in raw, parsed and encoded again. It refuses empty input, a
// block that is not a CERTIFICATE (a private key must never travel with the CA) and a certificate that does not parse.
func CertificatesOnly(raw []byte) ([]byte, error) {
	var out []byte
	rest := raw
	n := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("holds a %q block, want CERTIFICATE only", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("a certificate does not parse: %w", err)
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
		n++
	}
	if n == 0 {
		return nil, errors.New("holds no certificate")
	}
	if len(rest) > 0 && len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("holds data after the last PEM block")
	}
	return out, nil
}

// Sum is the hex SHA-256 of the parts. It hashes certificates only, never a key.
func Sum(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write(p)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func upsertConfigMap(ctx context.Context, client kubernetes.Interface, ns string, ca []byte, out io.Writer) error {
	cms := client.CoreV1().ConfigMaps(ns)
	want := map[string]string{CAKey: string(ca)}
	cm, err := cms.Get(ctx, ConfigMapName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "koinon"}},
			Data:       want,
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create ConfigMap %s/%s: %w", ns, ConfigMapName, err)
		}
		_, _ = fmt.Fprintf(out, "created ConfigMap %s/%s (public CA only)\n", ns, ConfigMapName)
		return nil
	case err != nil:
		return fmt.Errorf("read ConfigMap %s/%s: %w", ns, ConfigMapName, err)
	}
	if len(cm.Data) == 1 && cm.Data[CAKey] == want[CAKey] && len(cm.BinaryData) == 0 {
		_, _ = fmt.Fprintf(out, "ConfigMap %s/%s is current\n", ns, ConfigMapName)
		return nil
	}
	cm.Data, cm.BinaryData = want, nil
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ConfigMap %s/%s: %w", ns, ConfigMapName, err)
	}
	_, _ = fmt.Fprintf(out, "updated ConfigMap %s/%s (public CA only)\n", ns, ConfigMapName)
	return nil
}

// stamp sets the annotation on a pod template when it differs. A missing object is an error when required, else a
// note: the object does not exist yet and its first pod reads the current certificates.
func stamp(ctx context.Context, out io.Writer, what, key, sum string, required bool,
	read func(context.Context) (map[string]string, error), patch func(context.Context, []byte) error,
) error {
	have, err := read(ctx)
	switch {
	case apierrors.IsNotFound(err) && !required:
		_, _ = fmt.Fprintf(out, "%s does not exist yet: its first pod reads the current certificates\n", what)
		return nil
	case err != nil:
		return fmt.Errorf("read %s: %w", what, err)
	}
	if have[key] == sum {
		_, _ = fmt.Fprintf(out, "%s is current\n", what)
		return nil
	}
	tmpl := map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: sum}}}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"template": tmpl}})
	if err != nil {
		return err
	}
	if err := patch(ctx, body); err != nil {
		return fmt.Errorf("patch %s: %w", what, err)
	}
	_, _ = fmt.Fprintf(out, "%s: new certificates, its pods restart\n", what)
	return nil
}

// TrustPodSpec is the part of the atelet pod spec that makes atelet trust the CA: the optional ConfigMap volume, the
// mount and SSL_CERT_DIR on the atelet container. It adds no container, no image, no hostPath and no Secret volume, so
// the admission policy of the Substrate namespace has nothing new to allow. The installer patch (kind and generic) and
// EnsureTrust (every route, the gke route included) both use it, so the two cannot differ.
func TrustPodSpec() map[string]any {
	return map[string]any{
		"volumes": []any{map[string]any{
			"name":      VolumeName,
			"configMap": map[string]any{"name": ConfigMapName, "optional": true},
		}},
		"containers": []any{map[string]any{
			"name":         AteletContainer,
			"env":          []any{map[string]any{"name": EnvName, "value": CertDirs}},
			"volumeMounts": []any{map[string]any{"name": VolumeName, "mountPath": MountPath, "readOnly": true}},
		}},
	}
}

// ateletLacksTrust reports whether the atelet DaemonSet exists without the trust patch. A missing DaemonSet is not
// handled here: the stamp step reports it.
func ateletLacksTrust(ctx context.Context, client kubernetes.Interface, ns string) (bool, error) {
	ds, err := client.AppsV1().DaemonSets(ns).Get(ctx, DaemonSetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read DaemonSet %s/%s: %w", ns, DaemonSetName, err)
	}
	return !trusts(ds), nil
}

// EnsureTrust is the installer's step after ate-setup, on every route (issue #1194). The kind and generic routes already
// carry the patch from the kustomize overlay, so it does nothing there. The gke route of ate-setup uses an overlay
// directory that the installer does not know, so the installer adds the same patch (TrustPodSpec) to the live
// DaemonSet. The installer identity may write DaemonSets in the Substrate namespace; the hook Job may not (it changes
// the hash annotation only).
func EnsureTrust(ctx context.Context, client kubernetes.Interface, ns string, out io.Writer) error {
	ds, err := client.AppsV1().DaemonSets(ns).Get(ctx, DaemonSetName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read DaemonSet %s/%s: %w", ns, DaemonSetName, err)
	}
	if trusts(ds) {
		return nil
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": TrustPodSpec()}}})
	if err != nil {
		return err
	}
	if _, err := client.AppsV1().DaemonSets(ns).Patch(ctx, DaemonSetName, types.StrategicMergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch DaemonSet %s/%s to trust the image cache CA: %w", ns, DaemonSetName, err)
	}
	_, _ = fmt.Fprintf(out, "DaemonSet %s/%s had no image cache CA trust (a route without the overlay patch): added the volume, the mount and %s\n", ns, DaemonSetName, EnvName)
	return nil
}

// trusts reports whether the atelet DaemonSet has the volume of the ConfigMap, its mount and SSL_CERT_DIR on the atelet container.
func trusts(ds *appsv1.DaemonSet) bool {
	hasVolume, hasEnv, hasMount := false, false, false
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == VolumeName && v.ConfigMap != nil && v.ConfigMap.Name == ConfigMapName {
			hasVolume = true
		}
	}
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != AteletContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == EnvName && strings.Contains(e.Value, MountPath) {
				hasEnv = true
			}
		}
		for _, m := range c.VolumeMounts {
			if m.Name == VolumeName && m.MountPath == MountPath {
				hasMount = true
			}
		}
	}
	return hasVolume && hasEnv && hasMount
}
