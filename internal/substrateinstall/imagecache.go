// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"path/filepath"

	"github.com/stateracloud/koinon/internal/imagecacheca"
)

// imageCacheTrustPatch is the kustomize patch that makes atelet trust the CA of Koinon's image cache (issue #1191).
//
// atelet pulls actor images through the cache over TLS with Go's default transport, and Substrate v1.0.1 has no CA
// option. Go reads the system roots from /etc/ssl/certs/ca-certificates.crt of the image and then adds every
// certificate file of the directories in SSL_CERT_DIR (crypto/x509 loadOnDiskRoots; proved by a test in
// internal/imagecacheca). So the patch mounts the ConfigMap imagecacheca.ConfigMapName at imagecacheca.MountPath and
// sets SSL_CERT_DIR to it: the public roots of the image stay, the cache CA is added, TLS stays on, and the pod gets
// no new container and no new image, so the admission policy of the Substrate namespace (digest pins, hostPath only for
// the node agent) has nothing new to allow. The ConfigMap is optional: the installer runs before the chart, and the
// chart's hook Job (koinon-api imagecache-ca) fills it and rolls atelet. The ConfigMap holds the public CA certificate
// only, never a key.
//
// Exit condition: Substrate gets a CA option for its image pulls (not asked upstream; Koinon posts nothing there).
func imageCacheTrustPatch(ns string) koinonPatch {
	return koinonPatch{"koinon-atelet-imagecache-ca.yaml", map[string]any{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"name": imagecacheca.DaemonSetName, "namespace": ns},
		"spec":     map[string]any{"template": map[string]any{"spec": imagecacheca.TrustPodSpec()}},
	}}
}

// ApplyKindOverlay writes the corrections that every kind-route install needs, before ate-setup reads the tree: today
// the image cache trust of atelet. The generic platform has it in ApplyGenericOverlay; the kind platform keeps
// upstream's overlay otherwise.
func (p Profile) ApplyKindOverlay(dir, namespace string) error {
	return appendKindPatches(filepath.Join(dir, filepath.FromSlash(kindDir)), []koinonPatch{imageCacheTrustPatch(namespace)})
}
