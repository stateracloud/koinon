// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/preflight"
	"github.com/stateracloud/koinon/internal/substrateinstall"
)

// The fixtures hold the lines of the pinned kind overlay (substrate 20778a6d, manifests/ate-install/kind)
// that the corrections read; the rest of those files is left out.
const (
	kindKustomization = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ./atelet\n  - rustfs.yaml\n"
	ateletOverlay     = "patches:\n  - patch: |-\n      args:\n              - --localhost-registry-replacement=kind-registry:5000\n              - --grpc-server-cred-bundle=/run/x.pem\n              - --log-level=debug\n"
	postgresOverlay   = "patches:\n  - patch: |-\n      - op: replace\n        path: /spec/volumeClaimTemplates/0/spec/resources/requests/storage\n        value: 1Gi\n"
)

func pinnedKindFiles() map[string]string {
	const kind = "manifests/ate-install/kind/"
	return map[string]string{
		kind + "kustomization.yaml":          kindKustomization,
		kind + "atelet/kustomization.yaml":   ateletOverlay,
		kind + "postgres/kustomization.yaml": postgresOverlay,
		"cmd/ate-setup/main.go":              "main",
		egressPath:                           egressFixture,
	}
}

func writeFilesTar(path string, files map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return err
		}
	}
	return errors.Join(tw.Close(), f.Close())
}

func genericProfile() substrateinstall.Profile {
	return substrateinstall.Profile{StorageBucket: "koinon-snaps", Platform: substrateinstall.PlatformGeneric}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func patchObject(t *testing.T, dir, file string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, dir, "manifests/ate-install/kind/"+file)), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestParsePlatform(t *testing.T) {
	g := NewWithT(t)
	for _, p := range []substrateinstall.Platform{"generic", "gke", "kind"} {
		got, err := substrateinstall.ParsePlatform(string(p))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(p))
	}
	got, err := substrateinstall.ParsePlatform("")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeEmpty(), "empty means detect")
	_, err = substrateinstall.ParsePlatform("talos")
	g.Expect(err).To(MatchError(ContainSubstring(`unknown platform "talos" (valid: generic, gke, kind)`)))
}

func TestDetectsKindOrGenericAndNeverGKE(t *testing.T) {
	g := NewWithT(t)
	detect := func(nodes ...runtime.Object) (substrateinstall.Platform, error) {
		i, _, _ := setup("37", true, nodes)
		return i.DetectPlatform(t.Context())
	}
	p, err := detect(kindNode("a", "kind://docker/lab/a"), kindNode("b", "kind://docker/lab/b"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p).To(Equal(substrateinstall.PlatformKind))
	p, err = detect(kindNode("a", "kind://docker/lab/a"), kindNode("b", "talos://omni/b"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p).To(Equal(substrateinstall.PlatformGeneric), "one node that is not kind makes the cluster generic")
	p, err = detect(kindNode("a", "gce://project/zone/a"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p).To(Equal(substrateinstall.PlatformGeneric), "a GKE node is generic until the operator asks for gke")
	_, err = detect()
	g.Expect(err).To(MatchError(substrateinstall.ErrPlatform))
}

func TestInstallDetectsThePlatformWhenNoneIsGiven(t *testing.T) {
	g := NewWithT(t)
	i, r, _ := setup("37", true, []runtime.Object{kindNode("a", "talos://omni/a"), defaultClass("local")})
	i.Profile.Platform = ""
	r.tarFiles = pinnedKindFiles()
	var out strings.Builder
	i.Out = &out
	_ = i.Install(t.Context())
	g.Expect(out.String()).To(ContainSubstring("platform generic (detected from the nodes"))
	g.Expect(strings.Join(r.setupCalls()[0], " ")).To(ContainSubstring("--kind"))
}

func TestSetupArgsPerPlatform(t *testing.T) {
	g := NewWithT(t)
	generic := genericProfile()
	generic.SandboxClasses = "gvisor"
	args := generic.SetupArgs("ate-system")
	g.Expect(args).To(ContainElements("--kind", "--sandbox-classes=gvisor"))
	count := 0
	for _, a := range args {
		if strings.HasPrefix(a, "--sandbox-classes") {
			count++
		}
	}
	g.Expect(count).To(Equal(1), "the class is passed once")
	g.Expect(genericProfile().SetupArgs("ate-system")).To(ContainElement("--sandbox-classes=gvisor"), "no microVM class, even when none is asked for")

	kind := substrateinstall.Profile{StorageBucket: "b", Platform: substrateinstall.PlatformKind, SandboxClasses: "gvisor,microvm"}.SetupArgs("ate-system")
	g.Expect(kind).To(ContainElements("--kind", "--sandbox-classes=gvisor,microvm"))

	gke := substrateinstall.Profile{StorageBucket: "b", Platform: substrateinstall.PlatformGKE, GCPProjectID: "proj", SnapshotTransferGSAName: "xfer"}.SetupArgs("ate-system")
	g.Expect(gke).To(ContainElements("--gcp-project-id=proj", "--snapshot-transfer-gsa-name=xfer"))
	g.Expect(gke).NotTo(ContainElement("--kind"))
	plain := substrateinstall.Profile{StorageBucket: "b", Platform: substrateinstall.PlatformGKE}.SetupArgs("ate-system")
	for _, a := range plain {
		g.Expect(a).NotTo(Or(HavePrefix("--gcp-"), HavePrefix("--snapshot-transfer"), Equal("--kind")))
	}
}

func TestValidatesOptionsPerPlatform(t *testing.T) {
	g := NewWithT(t)
	bad := func(mod func(*substrateinstall.Profile), want string) {
		t.Helper()
		p := genericProfile()
		mod(&p)
		g.Expect(p.Validate()).To(MatchError(ContainSubstring(want)))
	}
	g.Expect(genericProfile().Validate()).To(Succeed())
	bad(func(p *substrateinstall.Profile) { p.Platform = "talos" }, "unknown platform")
	bad(func(p *substrateinstall.Profile) { p.StorageBucket = "Bad_Bucket" }, "not an S3 bucket name")
	bad(func(p *substrateinstall.Profile) { p.RustFSStorageSize = "lots" }, "--rustfs-storage-size")
	bad(func(p *substrateinstall.Profile) { p.PostgresStorageSize = "-1Gi" }, "--postgres-storage-size")
	bad(func(p *substrateinstall.Profile) { p.SandboxClasses = "gvisor,microvm" }, "microVM asset staging")
	bad(func(p *substrateinstall.Profile) { p.SetupCSI = "hostpath" }, "use nfs or none")
	bad(func(p *substrateinstall.Profile) { p.SetupCSI = "both" }, "use nfs or none")
	bad(func(p *substrateinstall.Profile) { p.GCPProjectID = "proj" }, "--gcp-project-id is only for --platform gke")
	g.Expect(func() error {
		p := genericProfile()
		p.SetupCSI, p.SandboxClasses = "nfs", "gvisor"
		return p.Validate()
	}()).To(Succeed())
	// Options of one platform are refused on the others; with the platform still to detect they pass until it is known.
	for _, pl := range []substrateinstall.Platform{substrateinstall.PlatformGKE, substrateinstall.PlatformKind} {
		p := substrateinstall.Profile{StorageBucket: "b", Platform: pl, RustFSStorageClass: "fast"}
		g.Expect(p.Validate()).To(MatchError(ContainSubstring("--rustfs-storage-class is only for --platform generic")))
	}
	g.Expect(substrateinstall.Profile{StorageBucket: "b", RustFSStorageSize: "5Gi"}.Validate()).To(Succeed())
	g.Expect(substrateinstall.Profile{StorageBucket: "b", Platform: substrateinstall.PlatformKind, SnapshotTransferGSAName: "x"}.Validate()).
		To(MatchError(ContainSubstring("--snapshot-transfer-gsa-name is only for --platform gke")))
}

func TestOverlayCorrectsTheKindRoute(t *testing.T) {
	g := NewWithT(t)
	dir := writeTree(t, pinnedKindFiles())
	p := genericProfile()
	p.RustFSStorageClass = "fast"
	g.Expect(p.ApplyGenericOverlay(dir, "ate-system")).To(Succeed())

	k := readFile(t, dir, "manifests/ate-install/kind/kustomization.yaml")
	g.Expect(k).To(HavePrefix(kindKustomization), "the upstream resources stay as they are")
	g.Expect(k).To(ContainSubstring("patches:\n  - path: koinon-rustfs-pvc.yaml"))

	// (c) size and class of the object-store volume, defaults larger than 1Gi.
	pvc := patchObject(t, dir, "koinon-rustfs-pvc.yaml")
	spec := pvc["spec"].(map[string]any)
	g.Expect(spec["storageClassName"]).To(Equal("fast"))
	g.Expect(spec["resources"].(map[string]any)["requests"].(map[string]any)["storage"]).To(Equal(substrateinstall.DefaultRustFSStorageSize))

	// (d) the credentials come from the Secret in every place that held rustfsadmin, and the plain value is deleted.
	for _, f := range []string{"koinon-rustfs-deployment.yaml", "koinon-rustfs-bucket-init.yaml", "koinon-api-server-objectstore.yaml", "koinon-atelet-objectstore.yaml"} {
		text := readFile(t, dir, "manifests/ate-install/kind/"+f)
		g.Expect(text).To(And(ContainSubstring("name: "+substrateinstall.RustFSSecretName), ContainSubstring("secretKeyRef"), ContainSubstring("value: null")), f)
		g.Expect(text).NotTo(ContainSubstring("rustfsadmin"), f)
	}

	// (b) the bucket-init Job creates the Koinon bucket, taken from the environment, not from the script text.
	job := readFile(t, dir, "manifests/ate-install/kind/koinon-rustfs-bucket-init.yaml")
	g.Expect(job).To(And(ContainSubstring("name: BUCKET"), ContainSubstring("value: koinon-snaps"), ContainSubstring(`--bucket "$BUCKET"`)))
	g.Expect(job).NotTo(ContainSubstring("ate-snapshots"))

	// (e) atelet logs at info and keeps no local registry replacement.
	atelet := readFile(t, dir, "manifests/ate-install/kind/atelet/kustomization.yaml")
	g.Expect(atelet).To(And(ContainSubstring("--log-level=info"), ContainSubstring("--grpc-server-cred-bundle"), Not(ContainSubstring("--log-level=debug")), Not(ContainSubstring("localhost-registry"))))

	// (c) the PostgreSQL volume size.
	g.Expect(readFile(t, dir, "manifests/ate-install/kind/postgres/kustomization.yaml")).To(And(ContainSubstring("value: "+substrateinstall.DefaultPostgresStorageSize), Not(ContainSubstring("1Gi"))))
}

// assertCacheTrustPatch checks the patch of issue #1191: a ConfigMap volume and a plain env var on the atelet
// container, and nothing the admission policy of the Substrate namespace restricts (no container, no image, no
// hostPath, no Secret volume).
func assertCacheTrustPatch(t *testing.T, dir string) {
	t.Helper()
	g := NewWithT(t)
	obj := patchObject(t, dir, "koinon-atelet-imagecache-ca.yaml")
	g.Expect(obj["kind"]).To(Equal("DaemonSet"))
	g.Expect(obj["metadata"].(map[string]any)["name"]).To(Equal("atelet"))
	podSpec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	g.Expect(podSpec).To(HaveLen(2), "only volumes and containers: no init container, no host setting")
	vols := podSpec["volumes"].([]any)
	g.Expect(vols).To(HaveLen(1))
	vol := vols[0].(map[string]any)
	g.Expect(vol).To(HaveLen(2), "a name and a ConfigMap, no hostPath and no Secret")
	cm := vol["configMap"].(map[string]any)
	g.Expect(cm["name"]).To(Equal("koinon-imagecache-ca"))
	g.Expect(cm["optional"]).To(BeTrue(), "the installer runs before the chart: atelet must start without the ConfigMap")
	ctrs := podSpec["containers"].([]any)
	g.Expect(ctrs).To(HaveLen(1))
	c := ctrs[0].(map[string]any)
	g.Expect(c["name"]).To(Equal("atelet"))
	g.Expect(c).NotTo(HaveKey("image"), "the atelet image stays the digest-pinned one")
	g.Expect(c["env"]).To(Equal([]any{map[string]any{"name": "SSL_CERT_DIR", "value": "/etc/koinon/imagecache-ca:/etc/ssl/certs"}}))
	g.Expect(c["volumeMounts"]).To(Equal([]any{map[string]any{"name": "koinon-imagecache-ca", "mountPath": "/etc/koinon/imagecache-ca", "readOnly": true}}))
}

func TestOverlayMakesAteletTrustTheImageCacheCA(t *testing.T) {
	g := NewWithT(t)
	for name, apply := range map[string]func(dir string) error{
		"generic": func(dir string) error { return genericProfile().ApplyGenericOverlay(dir, "ate-system") },
		"kind": func(dir string) error {
			return substrateinstall.Profile{StorageBucket: "b"}.ApplyKindOverlay(dir, "ate-system")
		},
	} {
		dir := writeTree(t, pinnedKindFiles())
		g.Expect(apply(dir)).To(Succeed(), name)
		g.Expect(readFile(t, dir, "manifests/ate-install/kind/kustomization.yaml")).To(ContainSubstring("  - path: koinon-atelet-imagecache-ca.yaml\n"), name)
		assertCacheTrustPatch(t, dir)
	}
}

// TestEveryRouteTrustsTheImageCacheCA is the per-route spec of issue #1194. A new platform fails here until it names how
// atelet gets the trust: the installer overlay (kind, generic) or the installer step after ate-setup, imagecacheca.EnsureTrust (every route, and the only
// one for gke, whose overlay directory in the pinned tree the installer does not know).
func TestEveryRouteTrustsTheImageCacheCA(t *testing.T) {
	g := NewWithT(t)
	overlay := map[substrateinstall.Platform]func(dir string) error{
		substrateinstall.PlatformGeneric: func(dir string) error { return genericProfile().ApplyGenericOverlay(dir, "ate-system") },
		substrateinstall.PlatformKind: func(dir string) error {
			return substrateinstall.Profile{StorageBucket: "b"}.ApplyKindOverlay(dir, "ate-system")
		},
		substrateinstall.PlatformGKE: nil, // no overlay: the installer adds the patch after ate-setup (internal/imagecacheca EnsureTrust, after ate-setup)
	}
	for _, platform := range substrateinstall.Platforms {
		apply, known := overlay[platform]
		g.Expect(known).To(BeTrue(), "platform %s must say how atelet trusts the image cache CA", platform)
		if apply == nil {
			continue
		}
		dir := writeTree(t, pinnedKindFiles())
		g.Expect(apply(dir)).To(Succeed(), string(platform))
		assertCacheTrustPatch(t, dir)
	}
}

func TestKindOverlayChangesNothingElse(t *testing.T) {
	g := NewWithT(t)
	files := pinnedKindFiles()
	dir := writeTree(t, files)
	g.Expect(substrateinstall.Profile{StorageBucket: "b"}.ApplyKindOverlay(dir, "ate-system")).To(Succeed())
	k := readFile(t, dir, "manifests/ate-install/kind/kustomization.yaml")
	g.Expect(k).To(HavePrefix(kindKustomization))
	g.Expect(strings.Count(k, "  - path: ")).To(Equal(1), "only the cache trust patch")
	g.Expect(readFile(t, dir, "manifests/ate-install/kind/atelet/kustomization.yaml")).To(Equal(files["manifests/ate-install/kind/atelet/kustomization.yaml"]))
	g.Expect(substrateinstall.Profile{StorageBucket: "b"}.ApplyKindOverlay(writeTree(t, map[string]string{"cmd/ate-setup/main.go": "main"}), "ate-system")).To(MatchError(substrateinstall.ErrGeneric))
	g.Expect(substrateinstall.Profile{StorageBucket: "b"}.ApplyKindOverlay(writeTree(t, map[string]string{
		"manifests/ate-install/kind/kustomization.yaml": kindKustomization + "patches:\n  - path: x.yaml\n",
	}), "ate-system")).To(MatchError(substrateinstall.ErrGeneric), "a pin with patches of its own no longer fits")
}

func TestOverlayUsesTheSizesAndClassOfTheProfile(t *testing.T) {
	g := NewWithT(t)
	dir := writeTree(t, pinnedKindFiles())
	p := genericProfile()
	p.RustFSStorageSize, p.PostgresStorageSize = "50Gi", "200Gi"
	g.Expect(p.ApplyGenericOverlay(dir, "ate-system")).To(Succeed())
	spec := patchObject(t, dir, "koinon-rustfs-pvc.yaml")["spec"].(map[string]any)
	g.Expect(spec["resources"].(map[string]any)["requests"].(map[string]any)["storage"]).To(Equal("50Gi"))
	g.Expect(spec).NotTo(HaveKey("storageClassName"), "no class named: the cluster default applies")
	g.Expect(readFile(t, dir, "manifests/ate-install/kind/postgres/kustomization.yaml")).To(ContainSubstring("value: 200Gi"))
}

func TestOverlayFailsWhenThePinnedTreeIsNotWhatItExpects(t *testing.T) {
	g := NewWithT(t)
	for name, mod := range map[string]func(map[string]string){
		"no debug log level": func(f map[string]string) { f["manifests/ate-install/kind/atelet/kustomization.yaml"] = "args: []\n" },
		"two sizes": func(f map[string]string) {
			f["manifests/ate-install/kind/postgres/kustomization.yaml"] += "value: 1Gi\n"
		},
		"patches already there": func(f map[string]string) {
			f["manifests/ate-install/kind/kustomization.yaml"] += "patches:\n  - path: x.yaml\n"
		},
		"no kind kustomization": func(f map[string]string) { delete(f, "manifests/ate-install/kind/kustomization.yaml") },
	} {
		files := pinnedKindFiles()
		mod(files)
		err := genericProfile().ApplyGenericOverlay(writeTree(t, files), "ate-system")
		g.Expect(err).To(MatchError(substrateinstall.ErrGeneric), name)
	}
}

func TestDockerShimAnswersInfoAndRefusesEverythingElse(t *testing.T) {
	g := NewWithT(t)
	dir := filepath.Join(t.TempDir(), "shim")
	env, err := substrateinstall.WriteDockerShim(dir, "/usr/bin:/bin")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(env).To(Equal([]string{"PATH=" + dir + ":/usr/bin:/bin"}), "the shim comes before any docker on the host")
	shim := readFile(t, dir, "docker")
	g.Expect(shim).To(And(ContainSubstring(`"$1" = "info"`), ContainSubstring("exit 127")))
	st, err := os.Stat(filepath.Join(dir, "docker"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(st.Mode().Perm() & 0o100).NotTo(BeZero())
}

func TestCredentialsSecretIsCreatedOnceAndKept(t *testing.T) {
	g := NewWithT(t)
	i, _, client := setup("37", true, nil)
	i.Profile = genericProfile()
	g.Expect(i.EnsureRustFSCredentials(t.Context())).To(Succeed())
	s, err := client.CoreV1().Secrets("ate-system").Get(t.Context(), substrateinstall.RustFSSecretName, metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(s.Data["access-key"]).To(HaveLen(20))
	g.Expect(s.Data["secret-key"]).To(HaveLen(40))
	g.Expect(string(s.Data["access-key"])).NotTo(Equal("rustfsadmin"))

	// A second run keeps the Secret: nothing is rewritten.
	before := s.Data["secret-key"]
	g.Expect(i.EnsureRustFSCredentials(t.Context())).To(Succeed())
	s, _ = client.CoreV1().Secrets("ate-system").Get(t.Context(), substrateinstall.RustFSSecretName, metav1.GetOptions{})
	g.Expect(s.Data["secret-key"]).To(Equal(before))
}

func TestCredentialsSecretThatDoesNotMatchFailsLoudAndIsNotChanged(t *testing.T) {
	g := NewWithT(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: substrateinstall.RustFSSecretName, Namespace: "ate-system"},
		Data:       map[string][]byte{"access-key": []byte("operatorvalue")},
	}
	i, _, client := setup("37", true, []runtime.Object{existing})
	i.Profile = genericProfile()
	err := i.EnsureRustFSCredentials(t.Context())
	g.Expect(err).To(MatchError(substrateinstall.ErrGeneric))
	g.Expect(err.Error()).To(And(ContainSubstring("secret-key"), ContainSubstring(substrateinstall.RustFSSecretName), ContainSubstring("nothing was installed")))
	g.Expect(err.Error()).NotTo(ContainSubstring("operatorvalue"), "a value is never printed")
	s, _ := client.CoreV1().Secrets("ate-system").Get(t.Context(), substrateinstall.RustFSSecretName, metav1.GetOptions{})
	g.Expect(s.Data).To(Equal(existing.Data))
}

func defaultClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}}
}

func plainClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestStoragePreflight(t *testing.T) {
	g := NewWithT(t)
	check := func(p substrateinstall.Profile, classes ...runtime.Object) preflight.Result {
		i, _, _ := setup("37", true, classes)
		i.Profile = p
		results := i.CheckStorage(t.Context())
		g.Expect(results).To(HaveLen(1))
		return results[0]
	}
	r := check(genericProfile(), defaultClass("local"))
	g.Expect(r.Status).To(Equal(preflight.Pass))

	r = check(genericProfile(), plainClass("local"))
	g.Expect(r.Status).To(Equal(preflight.Fail))
	g.Expect(r.Message).To(And(ContainSubstring("no default StorageClass"), ContainSubstring("local")))
	g.Expect(r.Fix).To(And(ContainSubstring("is-default-class"), ContainSubstring("--rustfs-storage-class")))

	r = check(genericProfile())
	g.Expect(r.Status).To(Equal(preflight.Fail))
	g.Expect(r.Message).To(ContainSubstring("none"))

	// Both volumes name a class: no default is needed.
	named := genericProfile()
	named.RustFSStorageClass, named.PostgresStorageClass = "fast", "fast"
	g.Expect(check(named, plainClass("fast")).Status).To(Equal(preflight.Pass))

	// A named class that does not exist fails, even with a default.
	named.PostgresStorageClass = "missing"
	r = check(named, plainClass("fast"), defaultClass("local"))
	g.Expect(r.Status).To(Equal(preflight.Fail))
	g.Expect(r.Message).To(ContainSubstring("StorageClass missing not found"))

	// One volume names its class and the other needs the default.
	half := genericProfile()
	half.RustFSStorageClass = "fast"
	g.Expect(check(half, plainClass("fast")).Status).To(Equal(preflight.Fail))
	g.Expect(check(half, plainClass("fast"), defaultClass("local")).Status).To(Equal(preflight.Pass))
}

func TestGenericInstallRunsAteSetupWithTheCorrectedTreeAndNoDocker(t *testing.T) {
	g := NewWithT(t)
	var crds []runtime.Object
	for _, n := range preflight.SubstrateCRDs {
		crds = append(crds, crd(n, true))
	}
	i, r, client := setup("37", true, []runtime.Object{defaultClass("local"), router(), atelet()}, crds...)
	i.Profile = genericProfile()
	r.tarFiles = pinnedKindFiles()
	var out strings.Builder
	i.Out = &out
	store, fetch := stageEnv(t)
	i.GVisorFetcher = fetch
	i.GVisorStore = func(context.Context) (substrateinstall.AssetStore, func(), error) { return store, func() {}, nil }
	var sawTree, sawEgress string
	r.onSetup = func(dir string) {
		sawTree = readFile(t, dir, "manifests/ate-install/kind/atelet/kustomization.yaml")
		sawEgress = readFile(t, dir, egressPath)
	}
	// The full preflight after the install fails (no Substrate api server in the fake cluster); the install ran.
	g.Expect(i.Install(t.Context())).To(MatchError(preflight.ErrFailed))

	checkEgressTLSCache(g, envoyClusters(g, sawEgress)) // issue #1486: ate-setup reads the corrected egress manifest
	calls := r.setupCalls()
	g.Expect(calls).To(HaveLen(1))
	g.Expect(calls[0]).To(ContainElements("--kind", "--sandbox-classes=gvisor", "--storage-bucket-name=koinon-snaps"))
	g.Expect(sawTree).To(ContainSubstring("--log-level=info"), "ate-setup read the corrected tree")
	g.Expect(r.envs[len(r.envs)-1]).To(ContainElement(HavePrefix("PATH=")))
	g.Expect(out.String()).To(ContainSubstring("the vm tier (microVM) is not installed"))
	g.Expect(out.String()).NotTo(ContainSubstring("devices"), "no node-device probe without the vm tier")
	_, err := client.CoreV1().Secrets("ate-system").Get(t.Context(), substrateinstall.RustFSSecretName, metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	// The gVisor tarballs are in the store and the SandboxConfig points at them, before the final preflight.
	g.Expect(store.puts).To(HaveLen(2))
	cfg, err := i.Dynamic.Resource(schema.GroupVersionResource{Group: "ate.dev", Version: "v1", Resource: "sandboxconfigs"}).Get(t.Context(), substrateinstall.GVisorSandboxConfigName, metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(toJSON(t, cfg)).To(ContainSubstring("s3://koinon-snaps/gvisor-assets/amd64/"))
}

func TestGenericInstallStopsBeforeBuildingWhenThereIsNoStorageClass(t *testing.T) {
	g := NewWithT(t)
	i, r, client := setup("37", true, nil)
	i.Profile = genericProfile()
	r.tarFiles = pinnedKindFiles()
	err := i.Install(t.Context())
	g.Expect(err).To(MatchError(substrateinstall.ErrPrerequisites))
	g.Expect(r.setupCalls()).To(BeEmpty())
	_, getErr := client.CoreV1().Namespaces().Get(t.Context(), "ate-system", metav1.GetOptions{})
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "nothing was created")
}

func TestGenericCleanupCommandDeletesTheKindOverlay(t *testing.T) {
	g := NewWithT(t)
	i, r, _ := setup("37", true, []runtime.Object{defaultClass("local")})
	i.Profile = genericProfile()
	r.tarFiles = pinnedKindFiles()
	r.err = errors.New("exit status 1")
	err := i.Install(t.Context())
	g.Expect(err).To(MatchError(ContainSubstring("delete ate-system --no-dev-env --kind --namespace='ate-system'")))
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(r.built)) })
}

func TestExistingVolumesThatWouldChangeAreRefused(t *testing.T) {
	g := NewWithT(t)
	class := "old"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "rustfs-data", Namespace: "ate-system"},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class, Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		}},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "ate-system"},
		Spec: appsv1.StatefulSetSpec{VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		}}}},
	}
	run := func(p substrateinstall.Profile, objs ...runtime.Object) preflight.Result {
		i, _, _ := setup("37", true, objs)
		i.Profile = p
		return i.CheckExistingVolumes(t.Context())[0]
	}
	g.Expect(run(genericProfile()).Status).To(Equal(preflight.Pass), "a first install has nothing to conflict with")
	g.Expect(run(genericProfile(), pvc).Status).To(Equal(preflight.Pass))
	p := genericProfile()
	p.RustFSStorageClass, p.RustFSStorageSize = "new", "5Gi"
	r := run(p, pvc)
	g.Expect(r.Status).To(Equal(preflight.Fail))
	g.Expect(r.Message).To(And(ContainSubstring("class old, not new"), ContainSubstring("cannot shrink")))
	r = run(genericProfile(), sts)
	g.Expect(r.Status).To(Equal(preflight.Fail))
	g.Expect(r.Message).To(ContainSubstring("postgres has a 1Gi volume, not 20Gi"))
	p = genericProfile()
	p.PostgresStorageSize = "1Gi"
	g.Expect(run(p, sts).Status).To(Equal(preflight.Pass))
}

func TestBucketNamesThatS3RefusesAreRefused(t *testing.T) {
	g := NewWithT(t)
	for _, b := range []string{"a..b", "192.168.1.1", "ab"} {
		p := genericProfile()
		p.StorageBucket = b
		g.Expect(p.Validate()).To(HaveOccurred(), b)
	}
}

func TestWarnsWhenEveryNodeIsGKEAndNoPlatformWasGiven(t *testing.T) {
	g := NewWithT(t)
	i, r, _ := setup("37", true, []runtime.Object{kindNode("a", "gce://p/z/a"), defaultClass("x")})
	i.Profile.Platform = ""
	r.tarFiles = pinnedKindFiles()
	var out strings.Builder
	i.Out = &out
	_ = i.Install(t.Context())
	g.Expect(out.String()).To(ContainSubstring("Set --platform gke for GCS"))
}
