// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/substrateinstall"
)

// fakeBody stands for the tarball: the pin of the tests is its SHA-256 (PinGVisorForTest).
var fakeBody = []byte("koinon_test_not-a-real-tarball")

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
	// downErr is returned by every call: the object store is down.
	downErr error
	// putErr is returned by Put only.
	putErr error
}

func newStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (s *fakeStore) Digest(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downErr != nil {
		return "", false, s.downErr
	}
	b, ok := s.objects[key]
	if !ok {
		return "", false, nil
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), true, nil
}

func (s *fakeStore) Put(_ context.Context, key string, r io.Reader, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downErr != nil {
		return s.downErr
	}
	if s.putErr != nil {
		return s.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size differs from the bytes")
	}
	s.objects[key] = b
	s.puts = append(s.puts, key)
	return nil
}

type fakeFetcher struct {
	body  []byte
	err   error
	calls []string
}

func (f *fakeFetcher) Open(_ context.Context, url string) (io.ReadCloser, error) {
	f.calls = append(f.calls, url)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func stageEnv(t *testing.T) (*fakeStore, *fakeFetcher) {
	t.Helper()
	substrateinstall.PinGVisorForTest(t, fakeBody)
	return newStore(), &fakeFetcher{body: fakeBody}
}

func TestFirstStageUploadsEveryArchitectureAfterTheDigestCheck(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	res, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Uploaded).To(ConsistOf("amd64", "arm64"))
	g.Expect(res.Present).To(BeEmpty())
	g.Expect(store.puts).To(ConsistOf(substrateinstall.GVisorKey("amd64"), substrateinstall.GVisorKey("arm64")))
	g.Expect(fetch.calls).To(ConsistOf(
		"https://storage.googleapis.com/gvisor/releases/release/"+substrateinstall.GVisorRelease+"/x86_64/gvisor.tar.zstd",
		"https://storage.googleapis.com/gvisor/releases/release/"+substrateinstall.GVisorRelease+"/aarch64/gvisor.tar.zstd"))
	// The key has the digest, and no segment "atespaces": atelet's plugin refuses assets under a snapshot location.
	for _, a := range res.Assets {
		g.Expect(a.Key).To(Equal("gvisor-assets/" + a.Arch + "/" + a.SHA256 + ".tar.zst"))
		g.Expect(a.Key).To(HaveSuffix(".tar.zst"), "atelet chooses the archive format from the suffix")
		g.Expect(strings.Split(a.Key, "/")).NotTo(ContainElement("atespaces"))
	}
}

func TestRepeatStageFetchesAndWritesNothing(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	_, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).NotTo(HaveOccurred())
	store.puts, fetch.calls = nil, nil
	res, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Present).To(ConsistOf("amd64", "arm64"))
	g.Expect(res.Uploaded).To(BeEmpty())
	g.Expect(store.puts).To(BeEmpty())
	g.Expect(fetch.calls).To(BeEmpty(), "an object with the pinned digest is not fetched again")
}

func TestAKeyStagedWithoutTheSuffixIsLeftAloneAndTheNewKeyIsStaged(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	newKey := substrateinstall.GVisorKey("amd64")
	oldKey := strings.TrimSuffix(newKey, ".tar.zst")
	g.Expect(oldKey).NotTo(Equal(newKey))
	store.objects[oldKey] = fakeBody // staged by the code before the fix
	res, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Uploaded).To(ConsistOf("amd64", "arm64"))
	g.Expect(store.objects).To(HaveKey(newKey))
	g.Expect(store.objects).To(HaveKey(oldKey), "the installer does not delete the old object")
	g.Expect(store.puts).NotTo(ContainElement(oldKey))
	// A second run is a no-op.
	store.puts, fetch.calls = nil, nil
	res, err = substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Present).To(ConsistOf("amd64", "arm64"))
	g.Expect(store.puts).To(BeEmpty())
	g.Expect(fetch.calls).To(BeEmpty())
}

func TestAnObjectWithAnotherDigestIsAnErrorAndNeverOverwritten(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	key := substrateinstall.GVisorKey("amd64")
	store.objects[key] = []byte("not the tarball")
	_, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).To(MatchError(substrateinstall.ErrDigestMismatch))
	g.Expect(err).To(MatchError(ContainSubstring(key)))
	g.Expect(store.puts).To(BeEmpty())
	g.Expect(fetch.calls).To(BeEmpty())
	g.Expect(store.objects[key]).To(Equal([]byte("not the tarball")))
}

func TestADownloadWithAnotherDigestIsNotWrittenToTheStore(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	fetch.body = []byte("a changed mirror")
	_, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "https://mirror.example.com/gvisor/?token=koinon_test_not-a-real-token", io.Discard)
	g.Expect(err).To(MatchError(substrateinstall.ErrDigestMismatch))
	g.Expect(err.Error()).NotTo(ContainSubstring("koinon_test_not-a-real-token"))
	g.Expect(store.puts).To(BeEmpty())
	g.Expect(store.objects).To(BeEmpty())
}

func TestAMirrorSuppliesTheTarballFromItsOwnBase(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	_, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "https://mirror.example.com/gvisor/", io.Discard)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch.calls).To(ConsistOf(
		"https://mirror.example.com/gvisor/"+substrateinstall.GVisorRelease+"/x86_64/gvisor.tar.zstd",
		"https://mirror.example.com/gvisor/"+substrateinstall.GVisorRelease+"/aarch64/gvisor.tar.zstd"))
}

func TestObjectStoreDownIsAnErrorBeforeAnyFetch(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	store.downErr = errors.New("connection refused")
	_, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).To(MatchError(ContainSubstring("connection refused")))
	g.Expect(fetch.calls).To(BeEmpty())
}

func TestAFailedWriteIsAnError(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	store.putErr = errors.New("bucket is read-only")
	_, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).To(MatchError(ContainSubstring("bucket is read-only")))
}

func TestAnUnreachableSourceNamesTheMirrorFlag(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	fetch.err = errors.New("no route to host")
	_, err := substrateinstall.StageGVisor(t.Context(), store, fetch, "", io.Discard)
	g.Expect(err).To(MatchError(And(ContainSubstring("no route to host"), ContainSubstring("--gvisor-mirror"))))
	g.Expect(store.puts).To(BeEmpty())
}

var sandboxConfigGVR = schema.GroupVersionResource{Group: "ate.dev", Version: "v1", Resource: "sandboxconfigs"}

func sandboxConfigClient(objs ...runtime.Object) (*dynamicfake.FakeDynamicClient, func(t *testing.T) *unstructured.Unstructured) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{sandboxConfigGVR: "SandboxConfigList"}, objs...)
	return dyn, func(t *testing.T) *unstructured.Unstructured {
		t.Helper()
		u, err := dyn.Resource(sandboxConfigGVR).Get(t.Context(), substrateinstall.GVisorSandboxConfigName, metav1.GetOptions{})
		NewWithT(t).Expect(err).NotTo(HaveOccurred())
		return u
	}
}

func stagedAssets() []substrateinstall.StagedAsset {
	return []substrateinstall.StagedAsset{
		{Arch: "amd64", Key: "gvisor-assets/amd64/" + strings.Repeat("a", 64) + ".tar.zst", SHA256: strings.Repeat("a", 64)},
		{Arch: "arm64", Key: "gvisor-assets/arm64/" + strings.Repeat("b", 64) + ".tar.zst", SHA256: strings.Repeat("b", 64)},
	}
}

func TestTheSandboxConfigHasAnS3URLAndTheSameDigestForEachArchitecture(t *testing.T) {
	g := NewWithT(t)
	dyn, get := sandboxConfigClient()
	changed, err := substrateinstall.ApplySandboxConfig(t.Context(), dyn, "snapshots", stagedAssets())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	cfg := get(t)
	g.Expect(cfg.GetAnnotations()).To(HaveKeyWithValue("sandboxconfig.ate.dev/is-class-default", "true"))
	class, _, _ := unstructured.NestedString(cfg.Object, "spec", "sandboxClass")
	g.Expect(class).To(Equal("gvisor"))
	versions, _, _ := unstructured.NestedSlice(cfg.Object, "spec", "versions")
	g.Expect(versions).To(HaveLen(1))
	v := versions[0].(map[string]any)
	def, _, _ := unstructured.NestedString(cfg.Object, "spec", "defaultVersion")
	g.Expect(def).To(Equal(v["name"]))
	g.Expect(v["name"]).To(MatchRegexp(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`))
	g.Expect(v["state"]).To(Equal("Enabled"))
	assets := v["assets"].(map[string]any)
	for arch, ch := range map[string]string{"amd64": "a", "arm64": "b"} {
		a := assets[arch].(map[string]any)["gvisor"].(map[string]any)
		g.Expect(a["url"]).To(Equal("s3://snapshots/gvisor-assets/" + arch + "/" + strings.Repeat(ch, 64) + ".tar.zst"))
		g.Expect(a["url"]).To(HaveSuffix(".tar.zst"))
		g.Expect(a["sha256"]).To(Equal(strings.Repeat(ch, 64)))
	}
	// Nothing in the SandboxConfig points at the internet.
	g.Expect(cfg.Object).NotTo(HaveKey("status"))
	g.Expect(strings.Contains(strings.ToLower(toJSON(t, cfg)), "gs://")).To(BeFalse())
}

func toJSON(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	b, err := u.MarshalJSON()
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	return string(b)
}

func TestApplyingTheSameSandboxConfigTwiceChangesNothing(t *testing.T) {
	g := NewWithT(t)
	dyn, _ := sandboxConfigClient()
	_, err := substrateinstall.ApplySandboxConfig(t.Context(), dyn, "snapshots", stagedAssets())
	g.Expect(err).NotTo(HaveOccurred())
	dyn.ClearActions()
	changed, err := substrateinstall.ApplySandboxConfig(t.Context(), dyn, "snapshots", stagedAssets())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeFalse())
	for _, a := range dyn.Actions() {
		g.Expect(a.GetVerb()).To(BeElementOf("get"), "a repeat run only reads")
	}
}

func TestANewReleaseAddsAVersionAndKeepsTheOldOne(t *testing.T) {
	g := NewWithT(t)
	old := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1", "kind": "SandboxConfig",
		"metadata": map[string]any{"name": substrateinstall.GVisorSandboxConfigName},
		"spec": map[string]any{"sandboxClass": "gvisor", "defaultVersion": "gvisor-20260101-0", "versions": []any{
			map[string]any{"name": "gvisor-20260101-0", "state": "Enabled", "assets": map[string]any{}},
		}},
	}}
	dyn, get := sandboxConfigClient(old)
	changed, err := substrateinstall.ApplySandboxConfig(t.Context(), dyn, "snapshots", stagedAssets())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	cfg := get(t)
	versions, _, _ := unstructured.NestedSlice(cfg.Object, "spec", "versions")
	g.Expect(versions).To(HaveLen(2))
	def, _, _ := unstructured.NestedString(cfg.Object, "spec", "defaultVersion")
	g.Expect(def).To(Equal(versions[1].(map[string]any)["name"]))
	g.Expect(cfg.GetAnnotations()).To(HaveKeyWithValue("sandboxconfig.ate.dev/is-class-default", "true"))
}

func TestAVersionWithOtherAssetsIsNeverRewritten(t *testing.T) {
	g := NewWithT(t)
	dyn, _ := sandboxConfigClient()
	_, err := substrateinstall.ApplySandboxConfig(t.Context(), dyn, "snapshots", stagedAssets())
	g.Expect(err).NotTo(HaveOccurred())
	other := stagedAssets()
	other[0].SHA256 = strings.Repeat("c", 64)
	dyn.ClearActions()
	_, err = substrateinstall.ApplySandboxConfig(t.Context(), dyn, "snapshots", other)
	g.Expect(err).To(MatchError(substrateinstall.ErrDigestMismatch))
	for _, a := range dyn.Actions() {
		g.Expect(a.GetVerb()).NotTo(BeElementOf("update", "create", "patch"))
	}
}

func TestSandboxConfigWriteErrorsAreReported(t *testing.T) {
	g := NewWithT(t)
	dyn, _ := sandboxConfigClient()
	dyn.PrependReactor("create", "sandboxconfigs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(`no matches for kind "SandboxConfig"`)
	})
	_, err := substrateinstall.ApplySandboxConfig(t.Context(), dyn, "snapshots", stagedAssets())
	g.Expect(err).To(MatchError(ContainSubstring("no matches for kind")))
}

// The Go pin and hack/versions.env are two copies of one release: they must agree.
func TestThePinMatchesVersionsEnv(t *testing.T) {
	g := NewWithT(t)
	raw, err := os.ReadFile("../../hack/versions.env")
	g.Expect(err).NotTo(HaveOccurred())
	val := func(name string) string {
		m := regexp.MustCompile(`(?m)^` + name + `=(.+)$`).FindSubmatch(raw)
		g.Expect(m).NotTo(BeNil(), name)
		return string(m[1])
	}
	g.Expect(val("GVISOR_VERSION")).To(Equal(substrateinstall.GVisorRelease))
	g.Expect(val("GVISOR_SHA256_AMD64")).To(Equal(substrateinstall.GVisorSHA256AMD64))
	g.Expect(val("GVISOR_SHA256_ARM64")).To(Equal(substrateinstall.GVisorSHA256ARM64))
}

func TestInstallerStagesOnGenericAndSkipsGKE(t *testing.T) {
	g := NewWithT(t)
	store, fetch := stageEnv(t)
	opened := 0
	for platform, want := range map[substrateinstall.Platform]int{substrateinstall.PlatformGKE: 0, substrateinstall.PlatformGeneric: 1} {
		opened = 0
		i, _, _ := setup("37", true, nil)
		i.Profile.Platform = platform
		i.GVisorFetcher = fetch
		i.GVisorStore = func(context.Context) (substrateinstall.AssetStore, func(), error) {
			opened++
			return store, func() {}, nil
		}
		dyn, get := sandboxConfigClient()
		i.Dynamic = dyn
		g.Expect(i.EnsureGVisorAssets(t.Context())).To(Succeed())
		g.Expect(opened).To(Equal(want), string(platform))
		if want == 1 {
			cfg := get(t)
			g.Expect(toJSON(t, cfg)).To(ContainSubstring("s3://snapshots/gvisor-assets/amd64/"))
			// The URL of the SandboxConfig is the staged key, with the suffix atelet needs.
			for _, arch := range []string{"amd64", "arm64"} {
				g.Expect(toJSON(t, cfg)).To(ContainSubstring(`"s3://snapshots/` + substrateinstall.GVisorKey(arch) + `"`))
			}
		}
	}
}

func TestInstallerSkipsStagingWithoutTheGVisorClass(t *testing.T) {
	g := NewWithT(t)
	i, _, _ := setup("37", true, nil)
	i.Profile.Platform = substrateinstall.PlatformKind
	i.Profile.SandboxClasses = "microvm"
	i.GVisorStore = func(context.Context) (substrateinstall.AssetStore, func(), error) {
		return nil, nil, errors.New("must not open")
	}
	g.Expect(i.EnsureGVisorAssets(t.Context())).To(Succeed())
}

func TestInstallerReportsAnObjectStoreThatIsDown(t *testing.T) {
	g := NewWithT(t)
	i, _, _ := setup("37", true, nil)
	i.Profile.Platform = substrateinstall.PlatformGeneric
	i.GVisorStore = func(context.Context) (substrateinstall.AssetStore, func(), error) {
		return nil, nil, errors.New("no running rustfs pod")
	}
	g.Expect(i.EnsureGVisorAssets(t.Context())).To(MatchError(And(ContainSubstring("stage gvisor"), ContainSubstring("no running rustfs pod"))))
}
