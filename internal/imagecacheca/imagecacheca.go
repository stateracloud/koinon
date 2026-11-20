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
	// A Substrate that was installed before the installer patch (or on a route without it) would pull with no trust
	// while this Job reports success: fail with the way out instead.
	if err := checkPatched(ctx, client, p.SubstrateNamespace); err != nil {
		return err
	}
	if err := upsertConfigMap(ctx, client, p.SubstrateNamespace, ca, out); err != nil {
		return err
	}
	caSum := Sum(ca)
	tlsSum := Sum(leaf, ca)
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
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: sum}}}}})
	if err != nil {
		return err
	}
	if err := patch(ctx, body); err != nil {
		return fmt.Errorf("patch %s: %w", what, err)
	}
	_, _ = fmt.Fprintf(out, "%s: new certificates, its pods restart\n", what)
	return nil
}

// checkPatched fails when the atelet DaemonSet exists without the installer patch: the volume of the ConfigMap and
// SSL_CERT_DIR on the atelet container. A missing DaemonSet is not checked here: the stamp step reports it.
func checkPatched(ctx context.Context, client kubernetes.Interface, ns string) error {
	ds, err := client.AppsV1().DaemonSets(ns).Get(ctx, DaemonSetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	hasVolume, hasEnv := false, false
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
	}
	if !hasVolume || !hasEnv {
		return fmt.Errorf("DaemonSet %s/%s does not trust the image cache CA (volume %s: %t, %s: %t): run koinon-api substrate install on a kind or generic cluster; the gke route has no patch yet",
			ns, DaemonSetName, VolumeName, hasVolume, EnvName, hasEnv)
	}
	return nil
}
