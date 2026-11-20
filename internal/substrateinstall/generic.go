// SPDX-License-Identifier: Apache-2.0

package substrateinstall

// The generic platform: any Kubernetes cluster that is neither kind nor GKE.
//
// The pinned ate-setup has two routes (cmd/ate-setup/internal/config/load.go applyKindDefaults). The GKE
// route needs GCS and a Google service account. The kind route is mostly "not GCP": an in-cluster rustfs
// object store for snapshots (manifests/ate-install/kind/rustfs.yaml) and an atelet without the
// GKE credential-provider hostPaths, which a read-only-root node such as Talos cannot create (upstream
// agent-substrate/substrate #2669). Upstream plans to make that install the generic default (#1782). Until
// then Koinon uses the kind route and corrects what is only right for a laptop.
//
// ate-setup has no overlay hook at this pin: SystemOverlay picks manifests/ate-install/kind by name and
// ate-setup reads the manifests from the source tree. The installer already owns that tree (it is the
// export of the pinned commit, a temporary copy that nothing else uses), so Koinon's corrections are
// written into the copy before ate-setup runs: kustomize patch files that the kind kustomization lists, and a
// few exact text edits where a patch cannot reach (items of an args list). Every edit names the text it
// expects and fails when the pinned tree does not hold it, so a changed pin cannot be corrected by a
// half-fitting edit. Rendering to files and applying a second time was refused: it would apply the
// upstream objects first (with the laptop sizes and the fixed credentials) and Koinon's after.
//
// Exit condition: when upstream ships a generic install (agent-substrate/substrate #1782) or a chart (#79),
// Koinon moves to it and this file goes.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/preflight"
)

const (
	// genericSandboxClasses is the only sandbox class of the generic platform. The microVM class stages its
	// guest assets with "docker run --network container:<node>" on the installer host, for the host
	// architecture (steps/microvm.go): that works on a kind node only. Staging the assets from inside the
	// cluster is follow-up work.
	genericSandboxClasses = "gvisor"

	// DefaultRustFSStorageSize and DefaultPostgresStorageSize replace the 1Gi of ate-setup's kind route.
	DefaultRustFSStorageSize   = "10Gi"
	DefaultPostgresStorageSize = "20Gi"

	// RustFSSecretName is the Secret of the root credentials of the in-cluster object store. The installer
	// creates it once and keeps it.
	RustFSSecretName = "koinon-rustfs-credentials" //nolint:gosec // the name of a Secret, not a credential
	rustFSAccessKey  = "access-key"
	rustFSSecretKey  = "secret-key"
)

// ErrGeneric is returned when the generic platform cannot be set up: the pinned tree does not hold what a
// correction expects, or the credential Secret does not match.
var ErrGeneric = errors.New("generic platform")

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validateGeneric checks what only the generic platform needs. The bucket name goes to a container
// environment variable, not into a script, but S3 bucket rules keep a typo out of the object store.
func (p Profile) validateGeneric() error {
	if !bucketName.MatchString(p.StorageBucket) || strings.Contains(p.StorageBucket, "..") || net.ParseIP(p.StorageBucket) != nil {
		return fmt.Errorf("substrate profile: the storage bucket %q is not an S3 bucket name (3 to 63 characters: lower-case letters, digits, dots and hyphens)", p.StorageBucket)
	}
	for _, o := range []option{{"--rustfs-storage-size", p.RustFSStorageSize}, {"--postgres-storage-size", p.PostgresStorageSize}} {
		if o.value == "" {
			continue
		}
		if q, err := resource.ParseQuantity(o.value); err != nil || q.Sign() <= 0 {
			return fmt.Errorf("substrate profile: %s %q is not a positive Kubernetes quantity (for example 10Gi)", o.name, o.value)
		}
	}
	if p.SandboxClasses != "" && p.SandboxClasses != genericSandboxClasses {
		return fmt.Errorf("substrate profile: --platform generic installs the sandbox class %s only (got %q): microVM asset staging needs docker on the installer host and works on kind only", genericSandboxClasses, p.SandboxClasses)
	}
	if p.SetupCSI == "hostpath" || p.SetupCSI == "both" {
		return fmt.Errorf("substrate profile: --setup-csi %s reaches into a kind node over docker; use nfs or none on --platform generic", p.SetupCSI)
	}
	return nil
}

func (p Profile) rustFSSize() string {
	return orString(p.RustFSStorageSize, DefaultRustFSStorageSize)
}

func (p Profile) postgresSize() string {
	return orString(p.PostgresStorageSize, DefaultPostgresStorageSize)
}

func orString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// koinonPatch is a kustomize strategic-merge patch file that Koinon adds next to the kind kustomization.
type koinonPatch struct {
	file string
	obj  map[string]any
}

func secretEnv(name, key string) map[string]any {
	// value: null deletes the plain value of the upstream entry, which would conflict with valueFrom.
	return map[string]any{"name": name, "value": nil, "valueFrom": map[string]any{
		"secretKeyRef": map[string]any{"name": RustFSSecretName, "key": key},
	}}
}

func objectStoreEnv() []any {
	return []any{secretEnv("AWS_ACCESS_KEY_ID", rustFSAccessKey), secretEnv("AWS_SECRET_ACCESS_KEY", rustFSSecretKey)}
}

// bucketScript is the command of the bucket-init Job: ate-setup's script with the bucket from the BUCKET
// environment variable instead of the fixed name ate-snapshots. ate-setup sets the snapshot location to
// gs://<bucket>/ for the bucket it is given, so the bucket the object store holds must have that name.
const bucketScript = `set -e
for i in $(seq 1 60); do
  if aws s3api head-bucket --bucket "$BUCKET" 2>/dev/null; then
    echo "bucket $BUCKET already exists"
    exit 0
  fi
  if aws s3api create-bucket --bucket "$BUCKET" 2>/dev/null; then
    echo "bucket $BUCKET created"
    exit 0
  fi
  echo "waiting for rustfs to become available... ($i/60)"
  sleep 2
done
echo "timed out waiting for rustfs"
exit 1
`

func pluginPatch(kind, name, ns string) map[string]any {
	api := "apps/v1"
	return map[string]any{
		"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"initContainers": []any{map[string]any{"name": "objectstore-plugin", "env": objectStoreEnv()}},
		}}},
	}
}

// genericPatches are the kustomize patches of the corrections (b) snapshot bucket, (c) sizes and storage
// class and (d) credentials from a Secret.
func (p Profile) genericPatches(ns string) []koinonPatch {
	pvc := map[string]any{"requests": map[string]any{"storage": p.rustFSSize()}}
	pvcSpec := map[string]any{"resources": pvc}
	if p.RustFSStorageClass != "" {
		pvcSpec["storageClassName"] = p.RustFSStorageClass
	}
	return []koinonPatch{
		{"koinon-rustfs-pvc.yaml", map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{"name": "rustfs-data", "namespace": ns}, "spec": pvcSpec,
		}},
		{"koinon-rustfs-deployment.yaml", map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "rustfs", "namespace": ns},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
				"name": "rustfs", "env": []any{secretEnv("RUSTFS_ACCESS_KEY", rustFSAccessKey), secretEnv("RUSTFS_SECRET_KEY", rustFSSecretKey)},
			}}}}},
		}},
		{"koinon-rustfs-bucket-init.yaml", map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": "rustfs-bucket-init", "namespace": ns},
			// The Job template is immutable: a finished Job goes away, so a run with another bucket or other keys creates a new one.
			"spec": map[string]any{"ttlSecondsAfterFinished": 300, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
				"name": "create-bucket", "env": append(objectStoreEnv(), map[string]any{"name": "BUCKET", "value": p.StorageBucket}),
				"command": []any{"/bin/sh", "-c", bucketScript},
			}}}}},
		}},
		{"koinon-api-server-objectstore.yaml", pluginPatch("Deployment", "ate-api-server", ns)},
		{"koinon-atelet-objectstore.yaml", pluginPatch("DaemonSet", "atelet", ns)},
	}
}

const kindDir = "manifests/ate-install/kind"

// ApplyGenericOverlay writes Koinon's corrections into the exported tree at dir, before ate-setup reads it.
// The corrections are:
//
//	(a) no microVM staging: the install is gvisor only (SetupArgs passes --sandbox-classes=gvisor);
//	(b) the object store holds the Koinon snapshot bucket, not ate-snapshots;
//	(c) the object store and PostgreSQL volumes have Koinon sizes and the named or default StorageClass;
//	(d) the object store root credentials come from a Secret, not from the fixed rustfsadmin;
//	(e) atelet logs at info (upstream's kind overlay sets debug) and without the local registry replacement;
//	(f) atelet trusts the CA of the image cache (imageCacheTrustPatch).
func (p Profile) ApplyGenericOverlay(dir, namespace string) error {
	kind := filepath.Join(dir, filepath.FromSlash(kindDir))
	// (f) atelet trusts the image cache CA (imagecache.go) is part of every kind-route install, not only generic.
	if err := appendKindPatches(kind, append(p.genericPatches(namespace), imageCacheTrustPatch(namespace))); err != nil {
		return err
	}
	// Edits that a patch cannot make: atelet args are a list of strings, and the PostgreSQL size is a JSON
	// patch value in the kind overlay (the StorageClass is ate-setup's own flag).
	edits := []struct{ file, old, new string }{
		{"atelet/kustomization.yaml", "              - --localhost-registry-replacement=kind-registry:5000\n", ""},
		{"atelet/kustomization.yaml", "--log-level=debug", "--log-level=info"},
		{"postgres/kustomization.yaml", "value: 1Gi", "value: " + p.postgresSize()},
	}
	for _, e := range edits {
		if err := replaceOnce(filepath.Join(kind, filepath.FromSlash(e.file)), e.old, e.new); err != nil {
			return err
		}
	}
	return nil
}

// appendKindPatches writes the patch files into the kind overlay and lists them in its kustomization. The kustomization
// must have no patches of its own: a pin that adds some no longer fits Koinon's corrections.
func appendKindPatches(kind string, patches []koinonPatch) error {
	kustomization := filepath.Join(kind, "kustomization.yaml")
	raw, err := os.ReadFile(kustomization) //nolint:gosec // inside the exported tree the installer made
	if err != nil {
		return fmt.Errorf("%w: read the kind kustomization of the pinned tree: %w", ErrGeneric, err)
	}
	if strings.Contains(string(raw), "\npatches:") {
		return fmt.Errorf("%w: %s already has patches: the pinned tree changed and Koinon's corrections no longer fit", ErrGeneric, kindDir+"/kustomization.yaml")
	}
	var list strings.Builder
	list.WriteString("\n# Koinon corrections (internal/substrateinstall).\npatches:\n")
	for _, pt := range patches {
		out, err := yaml.Marshal(pt.obj)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(kind, pt.file), out, 0o600); err != nil {
			return err
		}
		list.WriteString("  - path: " + pt.file + "\n")
	}
	return os.WriteFile(kustomization, append(raw, list.String()...), 0o600) //nolint:gosec // inside the exported tree the installer made
}

// replaceOnce replaces the one occurrence of old in the file, and fails when there is not exactly one.
func replaceOnce(path, old, replacement string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // inside the exported tree the installer made
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGeneric, err)
	}
	if n := strings.Count(string(raw), old); n != 1 {
		return fmt.Errorf("%w: %s holds %d copies of %q, want 1: the pinned tree changed and Koinon's corrections no longer fit", ErrGeneric, filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), n, strings.TrimSpace(old))
	}
	return os.WriteFile(path, []byte(strings.Replace(string(raw), old, replacement, 1)), 0o600) //nolint:gosec // inside the exported tree the installer made
}

// dockerShim stands in for docker while ate-setup runs on the generic platform. ate-setup --kind starts
// every deploy with "docker info" (config/kindcheck.go CheckKindCluster), which makes the installer host
// need docker for a cluster that has none. With the gvisor class only and no hostpath CSI nothing else
// uses docker, so the shim answers that one call and fails any other call loudly, so a docker call that
// a later pin adds cannot go unseen. Exit condition: upstream #1782 (a generic install that has no such check).
const dockerShim = `#!/bin/sh
# Koinon installer, generic platform: ate-setup --kind asks "docker info" before it deploys; this cluster is not kind.
if [ "$1" = "info" ]; then exit 0; fi
echo "koinon: the generic platform does not use docker, but ate-setup ran: docker $*" >&2
exit 127
`

// WriteDockerShim writes the shim into dir and returns the environment entry that puts it first in PATH.
func WriteDockerShim(dir, path string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(dockerShim), 0o700); err != nil { //nolint:gosec // an executable in the installer's temporary directory
		return nil, err
	}
	return []string{"PATH=" + dir + string(os.PathListSeparator) + path}, nil
}

const secretAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomString(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(secretAlphabet)))
	for k := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[k] = secretAlphabet[v.Int64()]
	}
	return string(b), nil
}

// EnsureRustFSCredentials makes sure the Secret of the object store credentials exists in the Substrate
// namespace. It creates the Secret with random keys when it is missing and keeps an existing one as it is:
// a Secret that lacks a key or holds an empty one is an error that names the Secret, never a silent
// rewrite. The keys are never printed or logged.
func (i *Installer) EnsureRustFSCredentials(ctx context.Context) error {
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	secrets := i.Kube.CoreV1().Secrets(ns)
	check := func(s *corev1.Secret) error {
		var bad []string
		for _, k := range []string{rustFSAccessKey, rustFSSecretKey} {
			if len(s.Data[k]) == 0 {
				bad = append(bad, k)
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("%w: Secret %s/%s exists but has no value for %s. The installer does not change a Secret it did not write. Delete the Secret and the rustfs-data claim to start the object store again, or add the keys yourself; nothing was installed",
				ErrGeneric, ns, RustFSSecretName, strings.Join(bad, " and "))
		}
		return nil
	}
	existing, err := secrets.Get(ctx, RustFSSecretName, metav1.GetOptions{})
	switch {
	case err == nil:
		return check(existing)
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("read Secret %s/%s: %w", ns, RustFSSecretName, err)
	}
	access, err := randomString(20)
	if err != nil {
		return err
	}
	secret, err := randomString(40)
	if err != nil {
		return err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: RustFSSecretName, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "koinon"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{rustFSAccessKey: []byte(access), rustFSSecretKey: []byte(secret)},
	}, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		// Another installer run wrote it first: keep that one.
		existing, err := secrets.Get(ctx, RustFSSecretName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read Secret %s/%s: %w", ns, RustFSSecretName, err)
		}
		return check(existing)
	case err != nil:
		return fmt.Errorf("create Secret %s/%s: %w", ns, RustFSSecretName, err)
	}
	return nil
}

const defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// CheckStorage is the generic platform's storage preflight: the volumes of the object store and of
// PostgreSQL need a StorageClass. A named class must exist; a volume without a named class needs a default
// class, or its claim stays Pending.
func (i *Installer) CheckStorage(ctx context.Context) []preflight.Result {
	const check = "Storage class"
	list, err := i.Kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return []preflight.Result{{
			Check: check, Status: preflight.Fail,
			Message: fmt.Sprintf("cannot list StorageClasses: %v", err),
			Fix:     "grant the installer the storageclasses list rule, or check the connection to the cluster",
		}}
	}
	var names []string
	hasDefault := false
	for _, c := range list.Items {
		names = append(names, c.Name)
		if c.Annotations[defaultClassAnnotation] == "true" || c.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
			hasDefault = true
		}
	}
	slices.Sort(names)
	var missing []string
	needDefault := i.Profile.RustFSStorageClass == "" || i.Profile.PostgresStorageClass == ""
	for _, n := range []string{i.Profile.RustFSStorageClass, i.Profile.PostgresStorageClass} {
		if n != "" && !slices.Contains(names, n) {
			missing = append(missing, n)
		}
	}
	switch {
	case len(missing) > 0:
		return []preflight.Result{{
			Check: check, Status: preflight.Fail,
			Message: fmt.Sprintf("StorageClass %s not found (the cluster has: %s)", strings.Join(missing, ", "), orString(strings.Join(names, ", "), "none")),
			Fix:     "name a StorageClass that exists with --rustfs-storage-class and --postgres-storage-class",
		}}
	case needDefault && !hasDefault:
		return []preflight.Result{{
			Check: check, Status: preflight.Fail,
			Message: fmt.Sprintf("the cluster has no default StorageClass (the cluster has: %s), so the volume of the object store or of PostgreSQL would stay Pending", orString(strings.Join(names, ", "), "none")),
			Fix:     "mark a StorageClass as default (annotation " + defaultClassAnnotation + "=true), or name classes with --rustfs-storage-class and --postgres-storage-class",
		}}
	}
	return []preflight.Result{{Check: check, Status: preflight.Pass, Message: fmt.Sprintf("found: %s", strings.Join(names, ", "))}}
}

// CheckExistingVolumes refuses a change that Kubernetes would reject halfway through the install: the
// class of an existing rustfs-data claim, a smaller size for it, and the size or class of the PostgreSQL
// volume claim template (immutable on a StatefulSet). It reads only and runs before anything is created.
func (i *Installer) CheckExistingVolumes(ctx context.Context) []preflight.Result {
	const check = "Existing volumes"
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	var problems []string
	if pvc, err := i.Kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "rustfs-data", metav1.GetOptions{}); err == nil {
		if c := i.Profile.RustFSStorageClass; c != "" && pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != c {
			problems = append(problems, fmt.Sprintf("claim rustfs-data has class %s, not %s", *pvc.Spec.StorageClassName, c))
		}
		want := resource.MustParse(i.Profile.rustFSSize())
		if have, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok && want.Cmp(have) < 0 {
			problems = append(problems, fmt.Sprintf("claim rustfs-data is %s, and a claim cannot shrink to %s", have.String(), want.String()))
		}
	}
	if sts, err := i.Kube.AppsV1().StatefulSets(ns).Get(ctx, "postgres", metav1.GetOptions{}); err == nil && len(sts.Spec.VolumeClaimTemplates) > 0 {
		t := sts.Spec.VolumeClaimTemplates[0]
		want := resource.MustParse(i.Profile.postgresSize())
		if have, ok := t.Spec.Resources.Requests[corev1.ResourceStorage]; ok && want.Cmp(have) != 0 {
			problems = append(problems, fmt.Sprintf("StatefulSet postgres has a %s volume, not %s (its volume claim template cannot change)", have.String(), want.String()))
		}
		if c := i.Profile.PostgresStorageClass; c != "" && t.Spec.StorageClassName != nil && *t.Spec.StorageClassName != c {
			problems = append(problems, fmt.Sprintf("StatefulSet postgres has class %s, not %s", *t.Spec.StorageClassName, c))
		}
	}
	if len(problems) == 0 {
		return []preflight.Result{{Check: check, Status: preflight.Pass, Message: "no existing volume conflicts with the options"}}
	}
	return []preflight.Result{{
		Check: check, Status: preflight.Fail, Message: strings.Join(problems, "; "),
		Fix: "use the options of the first install, or remove the install (ate-setup delete ate-system) and its volumes first",
	}}
}
