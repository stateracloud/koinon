// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/preflight"
	"github.com/stateracloud/koinon/internal/substrateinstall"
)

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// runner is a fake of git, go and ate-setup. It records the calls, writes the export that "git archive"
// would write and the binary that "go build" would build, and reports what the test sets.
type runner struct {
	calls [][]string
	envs  [][]string
	err   error
	// What the probes report: the ate-setup --version line, and what the repository resolves the pinned commit to.
	version, resolved string
	// tarEntries are the export's entries; nil is one file.
	tarEntries []tar.Header
	// tarFiles, when set, are the files of the export with their content.
	tarFiles map[string]string
	// onSetup is called with the directory ate-setup runs in.
	onSetup func(dir string)
	dirs    []string
	// ranIn is the directory ate-setup ran in, and existed is whether it existed then.
	ranIn   string
	existed bool
	// built is the binary path "go build" wrote.
	built string
	// cloneErr is what "git fetch" of the pinned commit returns.
	cloneErr error
}

func (r *runner) Run(_ context.Context, dir, name string, args, env []string, _ io.Writer) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	r.envs = append(r.envs, env)
	r.dirs = append(r.dirs, dir)
	switch {
	case name == "git":
		if slices.Contains(args, "init") {
			return nil
		}
		if slices.Contains(args, "fetch") {
			return r.cloneErr
		}
		for _, a := range args {
			if out, ok := strings.CutPrefix(a, "--output="); ok {
				if r.tarFiles != nil {
					return writeFilesTar(out, r.tarFiles)
				}
				if r.tarEntries == nil {
					// The default export is one source file and the egress manifest the installer edits on every platform.
					return writeFilesTar(out, map[string]string{"cmd/ate-setup/main.go": "main", egressPath: egressFixture})
				}
				return writeTar(out, r.tarEntries)
			}
		}
	case name == "go":
		for n, a := range args {
			if a == "-o" {
				r.built = args[n+1]
				return os.WriteFile(r.built, []byte("binary"), 0o700)
			}
		}
	case strings.HasSuffix(name, "ate-setup"):
		r.ranIn = dir
		if r.onSetup != nil {
			r.onSetup(dir)
		}
		_, err := os.Stat(dir)
		r.existed = err == nil
	}
	return r.err
}

func (r *runner) Output(_ context.Context, _, name string, args []string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "git" {
		return r.resolved + "\n", nil
	}
	return r.version, nil
}

func (r *runner) setupCalls() [][]string {
	var out [][]string
	for _, c := range r.calls {
		if strings.HasSuffix(c[0], "ate-setup") && len(c) > 1 && c[1] == "deploy" {
			out = append(out, c)
		}
	}
	return out
}

func writeTar(path string, entries []tar.Header) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	if entries == nil {
		entries = []tar.Header{{Name: "cmd/ate-setup/main.go", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}}
	}
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg && h.Size > 4 {
			// A header that claims a huge file: the extractor must refuse it before it reads the content,
			// so the archive ends here.
			if err := tw.WriteHeader(&h); err != nil {
				return err
			}
			return f.Close()
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = 4
		}
		if err := tw.WriteHeader(&h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte("main")); err != nil {
				return err
			}
		}
	}
	return errors.Join(tw.Close(), f.Close())
}

type issuer struct{ url string }

func (i issuer) Issuer(context.Context) (string, error) { return i.url, nil }

type probe struct{ reachable bool }

// ProbeIssuer answers as a cluster whose tokens carry the probed issuer.
func (p probe) ProbeIssuer(_ context.Context, iss string) (preflight.IssuerResult, error) {
	return preflight.IssuerResult{Reachable: p.reachable, TokenIssuer: iss, Detail: "x"}, nil
}

func crd(name string, established bool) *unstructured.Unstructured {
	status := "False"
	if established {
		status = "True"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1alpha1", "served": true, "storage": true}}},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": status}}},
	}}
}

func router(args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "atenet-router", Namespace: "ate-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "envoy"}, {Name: "atenet-router", Args: args}},
		}}},
	}
}

func setup(minor string, reachable bool, kube []runtime.Object, crds ...runtime.Object) (*substrateinstall.Installer, *runner, *fake.Clientset) {
	client := fake.NewClientset(kube...)
	// Like the controllers of a real cluster, fill a new namespace with the default ServiceAccount and
	// the cluster CA ConfigMap. The tracker, not the clientset: a reactor that calls the clientset deadlocks.
	client.PrependReactor("create", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		fillNamespace(client, a.(k8stesting.CreateAction).GetObject().(*corev1.Namespace).Name)
		return false, nil, nil
	})
	client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: minor, GitVersion: "v1." + minor}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crds...)
	r := &runner{
		version:  "ate-setup version " + substrateinstall.PinnedVersion + " commit=" + substrateinstall.PinnedCommit + " built=unknown darwin/arm64",
		resolved: substrateinstall.PinnedCommit,
	}
	checker := &preflight.Checker{
		Kube: client, Dynamic: dyn, CallTimeout: time.Second,
		Issuer: issuer{"https://issuer.example.com"}, IssuerProbe: probe{reachable},
		Config: preflight.Config{Substrate: &preflight.SubstrateConfig{}},
	}
	return &substrateinstall.Installer{
		Kube: client, Dynamic: dyn, Checker: checker, Runner: r, SourceRepo: "/src/substrate",
		Kubeconfig: "/etc/kube/config", Context: "lab",
		Profile:      substrateinstall.Profile{StorageBucket: "snapshots", Platform: substrateinstall.PlatformGKE},
		Out:          io.Discard,
		PollInterval: time.Millisecond, WaitTimeout: 50 * time.Millisecond,
		ProvisionerNamespace: "koinon", ProvisionerServiceAccount: "koinon-provisioner",
		Control: grantCluster(nil).dial,
	}, r, client
}

func fillNamespace(client *fake.Clientset, ns string) {
	_ = client.Tracker().Add(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns}})
	_ = client.Tracker().Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: ns}})
}

// On a fresh cluster the default ServiceAccount and kube-root-ca.crt appear after the namespace. The
// probe pods need both, so the installer waits for them, and gives up cleanly when they do not come.
func TestWaitsForTheNamespaceToBeFilledBeforeTheProbes(t *testing.T) {
	g := NewWithT(t)
	i, r, client := setup("37", true, nil)
	client.ReactionChain = client.ReactionChain[1:] // drop the filling reactor that setup prepended
	err := i.Install(t.Context())
	g.Expect(err).To(MatchError(And(ContainSubstring("not ready in time"), ContainSubstring("ServiceAccount default"), ContainSubstring("ConfigMap kube-root-ca.crt"))))
	g.Expect(r.setupCalls()).To(BeEmpty())
	nss, err := client.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nss.Items).To(BeEmpty(), "the namespace the installer created is removed")
}

func TestStopsBeforeInstallingWhenAPrerequisiteFails(t *testing.T) {
	g := NewWithT(t)
	for name, tc := range map[string]struct {
		minor     string
		reachable bool
	}{"kubernetes below 1.37": {"36", true}, "unreachable issuer": {"37", false}} {
		i, r, client := setup(tc.minor, tc.reachable, nil)
		err := i.Install(t.Context())
		g.Expect(err).To(MatchError(substrateinstall.ErrPrerequisites), name)
		g.Expect(r.setupCalls()).To(BeEmpty(), name)
		// Whatever the installer created for its probes is gone again, and ate-system was not created: no namespace remains.
		nss, err := client.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(nss.Items).To(BeEmpty(), name)
	}
}

func TestLeavesAnExistingNamespaceAloneWhenAPrerequisiteFails(t *testing.T) {
	g := NewWithT(t)
	for _, name := range []string{"ate-system", preflight.DefaultSubstrateProbeNamespace} {
		i, _, client := setup("37", false, []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}})
		fillNamespace(client, name)
		g.Expect(i.Install(t.Context())).To(MatchError(substrateinstall.ErrPrerequisites), name)
		_, err := client.CoreV1().Namespaces().Get(t.Context(), name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred(), name)
	}
}

// A probe namespace that is a Substrate namespace is refused before the installer creates anything (issue #1176).
func TestRefusesAProbeNamespaceThatIsASubstrateNamespace(t *testing.T) {
	g := NewWithT(t)
	for _, ns := range []string{"ate-system", "ate-workers"} {
		i, r, client := setup("37", true, nil)
		i.Checker.Config.Substrate.ProbeNamespace = ns
		g.Expect(i.Install(t.Context())).To(MatchError(ContainSubstring("is a Substrate namespace")), ns)
		g.Expect(r.setupCalls()).To(BeEmpty(), ns)
		nss, err := client.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(nss.Items).To(BeEmpty(), ns)
	}
}

func TestRunsSetupThenWaitsForCRDs(t *testing.T) {
	g := NewWithT(t)
	var crds []runtime.Object
	for _, n := range preflight.SubstrateCRDs {
		crds = append(crds, crd(n, true))
	}
	i, r, client := setup("37", true, []runtime.Object{router(), atelet()}, crds...)
	// The CRDs are present but Substrate's api server is not: the full preflight after the install fails, setup ran once.
	err := i.Install(t.Context())
	g.Expect(err).To(MatchError(preflight.ErrFailed))
	calls := r.setupCalls()
	g.Expect(calls).To(HaveLen(1))
	g.Expect(calls[0][0]).To(Equal(r.built))
	g.Expect(r.built).To(HaveSuffix("ate-setup"))
	g.Expect(strings.Join(calls[0], " ")).To(And(
		ContainSubstring("deploy ate-system"), ContainSubstring("--storage-bucket-name=snapshots"),
		ContainSubstring("--image-tag="+substrateinstall.PinnedVersion),
		ContainSubstring("--kubeconfig=/etc/kube/config"), ContainSubstring("--context=lab"),
		ContainSubstring("--ingress-allowed-spiffe-ids="+preflight.SubstrateRouterSPIFFEID("ate-system")),
		// The probed discovery issuer goes to ate-setup as a flag, so an EXPECTED_JWT_ISSUER in the environment cannot replace it.
		ContainSubstring("--ateapi-expected-jwt-issuer=https://issuer.example.com")))
	// The installer adds the image cache trust to atelet on every route (the gke route has no overlay patch, issue #1194).
	ds, err := client.AppsV1().DaemonSets("ate-system").Get(t.Context(), "atelet", metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ds.Spec.Template.Spec.Volumes).To(ContainElement(And(HaveField("Name", "koinon-imagecache-ca"), HaveField("ConfigMap.Optional", HaveValue(BeTrue())))))
	g.Expect(ds.Spec.Template.Spec.Containers[0].Env).To(ContainElement(HaveField("Name", "SSL_CERT_DIR")))
	g.Expect(ds.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(HaveField("MountPath", "/etc/koinon/imagecache-ca")))
	// ate-setup ran in the exported tree, and the tree is removed afterwards.
	g.Expect(r.existed).To(BeTrue())
	g.Expect(filepath.Base(r.ranIn)).To(Equal("src"))
	_, statErr := os.Stat(filepath.Dir(r.ranIn))
	g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "the temporary directory is deleted")
}

func TestBuildsAteSetupFromTheExportOfThePinnedCommit(t *testing.T) {
	g := NewWithT(t)
	i, r, _ := setup("37", true, nil)
	b, err := i.BuildSetup(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	defer b.Cleanup()
	// The source repository only supplies the commit, fetched by its id into a fresh bare repository: its
	// config, attributes and replace refs never reach the export.
	g.Expect(r.calls[0][:6]).To(Equal([]string{"git", "--no-replace-objects", "-c", "core.fsmonitor=", "init", "--quiet"}))
	g.Expect(r.calls[0]).To(ContainElement("--bare"))
	clone := r.calls[0][len(r.calls[0])-1]
	g.Expect(filepath.Base(clone)).To(Equal("repo.git"))
	g.Expect(r.calls[1]).To(Equal([]string{"git", "--no-replace-objects", "-c", "core.fsmonitor=", "-C", clone, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--", "/src/substrate", substrateinstall.PinnedCommit}))
	g.Expect(r.calls[2]).To(Equal([]string{"git", "--no-replace-objects", "-c", "core.fsmonitor=", "-C", clone, "rev-parse", "--verify", substrateinstall.PinnedCommit + "^{commit}"}))
	var archive, build []string
	for _, c := range r.calls {
		switch {
		case c[0] == "git" && slices.Contains(c, "archive"):
			archive = c
		case c[0] == "go":
			build = c
		}
	}
	g.Expect(archive[:6]).To(Equal([]string{"git", "--no-replace-objects", "-c", "core.fsmonitor=", "-C", clone}))
	g.Expect(archive).To(ContainElement(substrateinstall.PinnedCommit))
	g.Expect(build).To(And(ContainElement("-trimpath"), ContainElement("./cmd/ate-setup")))
	g.Expect(strings.Join(build, " ")).To(ContainSubstring("internal/version.Commit=" + substrateinstall.PinnedCommit))
	_, err = os.Stat(filepath.Join(b.Dir(), "cmd", "ate-setup", "main.go"))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestRefusesAnUnpinnedBuildOrRepository(t *testing.T) {
	g := NewWithT(t)
	for name, mutate := range map[string]func(*runner){
		"other version": func(r *runner) { r.version = strings.Replace(r.version, substrateinstall.PinnedVersion, "v1.0.2", 1) },
		"other commit": func(r *runner) {
			r.version = strings.Replace(r.version, "commit="+substrateinstall.PinnedCommit, "commit=deadbeef", 1)
		},
		"dirty build": func(r *runner) {
			r.version = strings.Replace(r.version, "commit="+substrateinstall.PinnedCommit, "commit="+substrateinstall.PinnedCommit+"-dirty", 1)
		},
		"no commit":              func(r *runner) { r.version = "ate-setup version " + substrateinstall.PinnedVersion },
		"two commits":            func(r *runner) { r.version += " commit=" + substrateinstall.PinnedCommit },
		"commit not in the repo": func(r *runner) { r.resolved = "deadbeef" },
		"link in the export": func(r *runner) {
			r.tarEntries = []tar.Header{{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}
		},
		"path out of the export": func(r *runner) {
			r.tarEntries = []tar.Header{{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}}
		},
		"nested path out of the export": func(r *runner) {
			r.tarEntries = []tar.Header{{Name: "cmd/../../evil", Typeflag: tar.TypeReg, Mode: 0o644}}
		},
		"absolute path in the export": func(r *runner) {
			r.tarEntries = []tar.Header{{Name: "/tmp/evil", Typeflag: tar.TypeReg, Mode: 0o644}}
		},
		"export above the size limit": func(r *runner) {
			r.tarEntries = []tar.Header{{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1<<30 + 1}}
		},
		"clone fails": func(r *runner) { r.cloneErr = errors.New("clone failed") },
	} {
		tmp := t.TempDir()
		i, r, _ := setup("37", true, nil)
		i.TempDir = tmp
		mutate(r)
		g.Expect(i.Install(t.Context())).To(MatchError(substrateinstall.ErrPin), name)
		g.Expect(r.setupCalls()).To(BeEmpty(), name)
		left, err := os.ReadDir(tmp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(left).To(BeEmpty(), name+": the temporary directory is deleted")
	}
}

func TestNeedsAKubeconfigAndAContext(t *testing.T) {
	g := NewWithT(t)
	for name, mutate := range map[string]func(*substrateinstall.Installer){
		"no kubeconfig": func(i *substrateinstall.Installer) { i.Kubeconfig = "" },
		"no context":    func(i *substrateinstall.Installer) { i.Context = "" },
	} {
		i, r, _ := setup("37", true, nil)
		mutate(i)
		g.Expect(i.Install(t.Context())).To(MatchError(ContainSubstring("kubeconfig and a context")), name)
		g.Expect(r.calls).To(BeEmpty(), name)
	}
}

func TestThePinIsComplete(t *testing.T) {
	g := NewWithT(t)
	g.Expect(substrateinstall.PinnedVersion).To(MatchRegexp(`^v[0-9]+\.[0-9]+\.[0-9]+$`))
	g.Expect(substrateinstall.PinnedCommit).To(MatchRegexp(`^[0-9a-f]{40}$`))
}

// The SPIFFE identity follows the configured Substrate namespace.
func TestSPIFFEIDFollowsTheNamespace(t *testing.T) {
	g := NewWithT(t)
	g.Expect(preflight.SubstrateRouterSPIFFEID("substrate-prod")).To(Equal("spiffe://cluster.local/ns/substrate-prod/sa/atenet-client"))
	args := substrateinstall.Profile{StorageBucket: "b"}.SetupArgs("substrate-prod")
	g.Expect(args).To(ContainElements("--namespace=substrate-prod",
		"--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/substrate-prod/sa/atenet-client"))
}

// agentd's pod certificate is the only Koinon identity on the router allow-list (issue #1446).
func TestAgentdIdentityIsOnTheAllowList(t *testing.T) {
	g := NewWithT(t)
	id := preflight.SubstrateAgentdSPIFFEID("koinon", "koinon-agentd")
	g.Expect(id).To(Equal("spiffe://cluster.local/ns/koinon/sa/koinon-agentd"))
	args := substrateinstall.Profile{StorageBucket: "b"}.SetupArgs("ate-system", id)
	g.Expect(args).To(ContainElement("--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID("ate-system") + "," + id))

	i, _, client := setup("37", true, nil)
	i.Checker.Config.Substrate.AgentdNamespace, i.Checker.Config.Substrate.AgentdServiceAccount = "koinon", "koinon-agentd"
	_, err := client.AppsV1().Deployments("ate-system").Create(t.Context(), router(), metav1.CreateOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(Succeed())
	g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(Succeed(), "idempotent")
	d, err := client.AppsV1().Deployments("ate-system").Get(t.Context(), "atenet-router", metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	got := strings.Join(d.Spec.Template.Spec.Containers[1].Args, " ")
	g.Expect(got).To(And(ContainSubstring(id), ContainSubstring("sa/atenet-client")))
	g.Expect(strings.Count(got, id)).To(Equal(1))
}

func TestEnsureRouterSPIFFEUsesTheConfiguredNamespace(t *testing.T) {
	g := NewWithT(t)
	i, _, client := setup("37", true, nil)
	i.Checker.Config.Substrate.Namespace = "substrate-prod"
	r := router()
	r.Namespace = "substrate-prod"
	_, err := client.AppsV1().Deployments("substrate-prod").Create(t.Context(), r, metav1.CreateOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(Succeed())
	d, err := client.AppsV1().Deployments("substrate-prod").Get(t.Context(), "atenet-router", metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.Join(d.Spec.Template.Spec.Containers[1].Args, " ")).To(And(
		ContainSubstring("ns/substrate-prod/sa/atenet-client"), Not(ContainSubstring("ns/ate-system"))))
}

func TestSetupFailureStopsTheStep(t *testing.T) {
	g := NewWithT(t)
	i, r, _ := setup("37", true, nil)
	r.err = errors.New("exit status 1")
	err := i.Install(t.Context())
	g.Expect(err).To(MatchError(ContainSubstring("exit status 1")))
	// No silent partial install: the error names the exact command that removes it, with the pinned binary.
	g.Expect(err.Error()).To(And(
		ContainSubstring("partial install"), ContainSubstring("delete ate-system --no-dev-env --namespace='ate-system'"),
		ContainSubstring("--kubeconfig='/etc/kube/config' --context='lab'"), ContainSubstring(r.built)))
	// The binary the command names still exists.
	_, statErr := os.Stat(r.built)
	g.Expect(statErr).NotTo(HaveOccurred())
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(r.built)) })
}

const inCluster = "https://kubernetes.default.svc"

func authData(t *testing.T, i *substrateinstall.Installer) string {
	t.Helper()
	cm, err := i.Kube.CoreV1().ConfigMaps("ate-system").Get(t.Context(), "ate-api-authentication", metav1.GetOptions{})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	return cm.Data["authentication.yaml"]
}

func TestWritesTheAuthenticationConfigForTheClusterOwnIssuer(t *testing.T) {
	g := NewWithT(t)
	i, _, client := setup("37", true, []runtime.Object{&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ate-api-server", Namespace: "ate-system"}}})
	changed, err := i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(authData(t, i)).To(And(
		ContainSubstring("name: kubernetes"), ContainSubstring("issuer: "+inCluster), ContainSubstring("api.ate-system.svc"),
		ContainSubstring("certificateAuthorityFile: "+preflight.IssuerCAFile), ContainSubstring("discoveryTokenFile: "+preflight.IssuerTokenFile)))
	d, _ := client.AppsV1().Deployments("ate-system").Get(t.Context(), "ate-api-server", metav1.GetOptions{})
	g.Expect(d.Spec.Template.Annotations).To(HaveKey("koinon.dev/authentication-config-hash"), "the api server is rolled onto the new config")

	// Again: nothing changes, so no restart.
	before := d.Spec.Template.Annotations["koinon.dev/authentication-config-hash"]
	changed, err = i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeFalse())
	d, _ = client.AppsV1().Deployments("ate-system").Get(t.Context(), "ate-api-server", metav1.GetOptions{})
	g.Expect(d.Spec.Template.Annotations["koinon.dev/authentication-config-hash"]).To(Equal(before))
}

func TestRepairsAnEntryWithoutTheFilesAndKeepsOtherProviders(t *testing.T) {
	g := NewWithT(t)
	old := "jwtProviders:\n- audiences: [extra.example]\n  issuer: " + inCluster + "\n  name: kubernetes\n- audiences: [x]\n  issuer: https://other.example\n  name: other\n"
	i, _, _ := setup("37", true, []runtime.Object{&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ate-api-authentication", Namespace: "ate-system"}, Data: map[string]string{"authentication.yaml": old},
	}})
	changed, err := i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(authData(t, i)).To(And(
		ContainSubstring("certificateAuthorityFile"), ContainSubstring("extra.example"), ContainSubstring("https://other.example")))
}

func TestLeavesTheAuthenticationConfigAloneForAnotherIssuer(t *testing.T) {
	g := NewWithT(t)
	i, _, client := setup("37", true, nil)
	changed, err := i.EnsureAPIAuthentication(t.Context(), "https://issuer.example.com")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeFalse())
	_, getErr := client.CoreV1().ConfigMaps("ate-system").Get(t.Context(), "ate-api-authentication", metav1.GetOptions{})
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue())
}

func TestWaitCRDsTimesOutOnAnUnestablishedCRD(t *testing.T) {
	g := NewWithT(t)
	crds := []runtime.Object{crd(preflight.SubstrateCRDs[0], true), crd(preflight.SubstrateCRDs[1], false)}
	i, _, _ := setup("37", true, nil, crds...)
	err := i.WaitCRDs(t.Context())
	g.Expect(err).To(MatchError(ContainSubstring(preflight.SubstrateCRDs[1])))
	g.Expect(err).To(MatchError(ContainSubstring(preflight.SubstrateCRDs[2])))
	g.Expect(err.Error()).NotTo(ContainSubstring(preflight.SubstrateCRDs[0]))
}

func TestEnsureRouterSPIFFEIsIdempotent(t *testing.T) {
	g := NewWithT(t)
	for name, args := range map[string][]string{
		"no flag":       nil,
		"other id":      {"--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/x/sa/y"},
		"already there": {"--ingress-allowed-spiffe-ids=" + preflight.SubstrateRouterSPIFFEID("ate-system")},
	} {
		i, _, client := setup("37", true, []runtime.Object{router(args...)})
		for range 2 {
			g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(Succeed(), name)
		}
		d, err := client.AppsV1().Deployments("ate-system").Get(t.Context(), "atenet-router", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		got := strings.Join(d.Spec.Template.Spec.Containers[1].Args, " ")
		g.Expect(strings.Count(got, preflight.SubstrateRouterSPIFFEID("ate-system"))).To(Equal(1), name)
		if name == "other id" {
			g.Expect(got).To(ContainSubstring("spiffe://cluster.local/ns/x/sa/y,"))
		}
	}
}

// The options a real cluster needs reach ate-setup under the names the pinned ate-setup defines
// (cmd/ate-setup/internal/config/setting.go and cmd/ate-setup/internal/cmd/deploy.go at v1.0.1).
func TestProfilePassesTheClusterOptions(t *testing.T) {
	g := NewWithT(t)
	plain := substrateinstall.Profile{StorageBucket: "b"}.SetupArgs("ate-system")
	for _, a := range plain {
		g.Expect(a).NotTo(Or(HavePrefix("--ateapi-"), HavePrefix("--setup-csi"), Equal("--kind")))
	}
	args := substrateinstall.Profile{
		StorageBucket: "b", PostgresStorageClass: "local-path", SetupCSI: "none",
		ExpectedJWTIssuer: "https://kubernetes.default.svc", Platform: substrateinstall.PlatformKind,
	}.SetupArgs("ate-system")
	g.Expect(args).To(ContainElements("--ateapi-postgres-storage-class=local-path", "--setup-csi=none",
		"--ateapi-expected-jwt-issuer=https://kubernetes.default.svc", "--kind"))
	g.Expect(substrateinstall.Profile{StorageBucket: "b", SetupCSI: "ceph"}.Validate()).To(MatchError(ContainSubstring("unknown CSI driver")))
	g.Expect(substrateinstall.Profile{StorageBucket: "b", ExpectedJWTIssuer: "http://x"}.Validate()).To(MatchError(ContainSubstring("https URL")))
	// An empty query still changes the discovery URL: the suffix would land after the "?".
	g.Expect(substrateinstall.Profile{StorageBucket: "b", ExpectedJWTIssuer: "https://issuer.example?"}.Validate()).To(MatchError(ContainSubstring("without query")))
	g.Expect(substrateinstall.Profile{StorageBucket: "b", SetupCSI: "both", ExpectedJWTIssuer: "https://kubernetes.default.svc"}.Validate()).To(Succeed())
	g.Expect(substrateinstall.Profile{StorageBucket: "b", ExpectedJWTIssuer: "https://issuer.example/tenant"}.Validate()).To(Succeed())
}

func TestProfileNamesWhatIsMissing(t *testing.T) {
	g := NewWithT(t)
	g.Expect(substrateinstall.Profile{}.Validate()).To(MatchError(ContainSubstring("storage bucket")))
}

// The probe pods run in a namespace of their own, never in Substrate's namespaces (issue #1176): Substrate's
// admission policies refuse a bare pod there. The installer creates the probe namespace before the probes, with Pod
// Security privileged (the device probe mounts hostPath devices), and removes it when it is done. It still creates
// ate-system for ate-setup, with Pod Security privileged. The fake clientset enforces neither namespaces nor the
// policies, so this test adds both rules of the API server.
func TestRunsTheProbePodsOutsideTheSubstrateNamespaces(t *testing.T) {
	g := NewWithT(t)
	i, r, client := setup("37", true, nil)
	var probeNamespaces []string
	var probeLabels map[string]string
	client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca := a.(k8stesting.CreateAction)
		probeNamespaces = append(probeNamespaces, ca.GetNamespace())
		if ca.GetNamespace() == "ate-system" || ca.GetNamespace() == "ate-workers" {
			return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "probe", errors.New("denied by ValidatingAdmissionPolicy"))
		}
		// The tracker, not the clientset: a reactor that calls the clientset deadlocks on its lock.
		nsGVR := corev1.SchemeGroupVersion.WithResource("namespaces")
		ns, err := client.Tracker().Get(nsGVR, "", ca.GetNamespace())
		if apierrors.IsNotFound(err) {
			return true, nil, apierrors.NewNotFound(corev1.Resource("namespaces"), ca.GetNamespace())
		}
		probeLabels = ns.(*corev1.Namespace).Labels
		pod := ca.GetObject().(*corev1.Pod)
		pod.Name = "probe"
		pod.Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})
	i.Checker.IssuerProbe = preflight.KubeSubstrateProber{
		Client: client, Namespace: preflight.DefaultSubstrateProbeNamespace, NamePrefix: "t", Image: "img", PollInterval: time.Millisecond, Timeout: time.Second,
		Logs: func(context.Context, string, string) (string, error) {
			return preflight.IssuerResult{Reachable: true, TokenIssuer: "https://issuer.example.com", Detail: "ok"}.String() + "\n" + preflight.SubstrateDevicesProbeMarker + "\n", nil
		},
	}
	i.Checker.Devices = i.Checker.IssuerProbe.(preflight.KubeSubstrateProber)
	i.Checker.Config.Substrate.VMTier = true
	_ = i.Install(t.Context()) // later checks fail: Substrate itself is not installed in the fake
	g.Expect(r.setupCalls()).To(HaveLen(1), "the prerequisites passed, so ate-setup ran")
	g.Expect(probeNamespaces).NotTo(BeEmpty())
	g.Expect(probeNamespaces).To(HaveEach(preflight.DefaultSubstrateProbeNamespace), "no probe pod in a Substrate namespace")
	for _, mode := range []string{"enforce", "warn", "audit"} {
		g.Expect(probeLabels).To(HaveKeyWithValue("pod-security.kubernetes.io/"+mode, "privileged"))
	}
	g.Expect(probeLabels).To(HaveKeyWithValue("app.kubernetes.io/part-of", "koinon"))
	_, err := client.CoreV1().Namespaces().Get(t.Context(), preflight.DefaultSubstrateProbeNamespace, metav1.GetOptions{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the probe namespace the installer created is removed: %v", err)
	ns, err := client.CoreV1().Namespaces().Get(t.Context(), "ate-system", metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	for _, mode := range []string{"enforce", "warn", "audit"} {
		g.Expect(ns.Labels).To(HaveKeyWithValue("pod-security.kubernetes.io/"+mode, "privileged"))
	}
}

// The flag may sit in the container's command; a sidecar first in the list is not the router.
func TestEnsureRouterSPIFFEReadsTheCommandAndPicksTheContainerByName(t *testing.T) {
	g := NewWithT(t)
	r := router()
	r.Spec.Template.Spec.Containers[0].Args = []string{"--ingress-allowed-spiffe-ids=other"}
	r.Spec.Template.Spec.Containers[1].Command = []string{"/atenet", "--ingress-allowed-spiffe-ids=spiffe://x"}
	i, _, client := setup("37", true, []runtime.Object{r})
	g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(Succeed())
	d, err := client.AppsV1().Deployments("ate-system").Get(t.Context(), "atenet-router", metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(d.Spec.Template.Spec.Containers[0].Args).To(Equal([]string{"--ingress-allowed-spiffe-ids=other"}))
	g.Expect(d.Spec.Template.Spec.Containers[1].Command[1]).To(Equal("--ingress-allowed-spiffe-ids=spiffe://x," + preflight.SubstrateRouterSPIFFEID("ate-system")))
	g.Expect(d.Spec.Template.Spec.Containers[1].Args).To(BeEmpty())
}

func TestEnsureRouterSPIFFENamesTheDeploymentInErrors(t *testing.T) {
	g := NewWithT(t)
	i, _, _ := setup("37", true, nil)
	g.Expect(i.EnsureRouterSPIFFE(t.Context())).To(MatchError(ContainSubstring("ate-system/atenet-router")))
}

func TestRepairFallsBackToTheDefaultAudienceWhenTheEntryHasNone(t *testing.T) {
	g := NewWithT(t)
	old := "jwtProviders:\n- audiences: []\n  issuer: https://other.example\n  name: kubernetes\n"
	i, _, _ := setup("37", true, []runtime.Object{&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ate-api-authentication", Namespace: "ate-system"}, Data: map[string]string{"authentication.yaml": old},
	}})
	changed, err := i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(authData(t, i)).To(ContainSubstring("api.ate-system.svc"))
}

func TestReconcilesAProviderUnderAnotherNameWithTheSameIssuer(t *testing.T) {
	g := NewWithT(t)
	old := "jwtProviders:\n- audiences: [mine.example]\n  issuer: " + inCluster + "\n  name: cluster\n"
	i, _, _ := setup("37", true, []runtime.Object{&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ate-api-authentication", Namespace: "ate-system"}, Data: map[string]string{"authentication.yaml": old},
	}})
	changed, err := i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	data := authData(t, i)
	g.Expect(strings.Count(data, "issuer: "+inCluster)).To(Equal(1), "one provider per issuer")
	g.Expect(data).To(And(ContainSubstring("name: cluster"), ContainSubstring("mine.example"), ContainSubstring("certificateAuthorityFile"), Not(ContainSubstring("name: kubernetes"))))
}

func TestRepairsEmptyAudiencesOnAnOtherwiseMatchingEntry(t *testing.T) {
	g := NewWithT(t)
	old := "jwtProviders:\n- audiences: []\n  certificateAuthorityFile: " + preflight.IssuerCAFile + "\n  discoveryTokenFile: " + preflight.IssuerTokenFile + "\n  issuer: " + inCluster + "\n  name: kubernetes\n"
	i, _, _ := setup("37", true, []runtime.Object{&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ate-api-authentication", Namespace: "ate-system"}, Data: map[string]string{"authentication.yaml": old},
	}})
	changed, err := i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(authData(t, i)).To(ContainSubstring("api.ate-system.svc"))
}

func TestRestartsTheAPIServerWhenAMatchingConfigWasNeverRolled(t *testing.T) {
	g := NewWithT(t)
	i, _, client := setup("37", true, []runtime.Object{&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ate-api-server", Namespace: "ate-system"}}})
	_, err := i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	// A first run wrote the ConfigMap but its restart failed: the Deployment carries no (or an old) hash.
	d, _ := client.AppsV1().Deployments("ate-system").Get(t.Context(), "ate-api-server", metav1.GetOptions{})
	d.Spec.Template.Annotations = map[string]string{"koinon.dev/authentication-config-hash": "stale"}
	_, err = client.AppsV1().Deployments("ate-system").Update(t.Context(), d, metav1.UpdateOptions{})
	g.Expect(err).NotTo(HaveOccurred())

	changed, err := i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	d, _ = client.AppsV1().Deployments("ate-system").Get(t.Context(), "ate-api-server", metav1.GetOptions{})
	g.Expect(d.Spec.Template.Annotations["koinon.dev/authentication-config-hash"]).NotTo(Equal("stale"))

	changed, err = i.EnsureAPIAuthentication(t.Context(), inCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeFalse(), "rolled now, so a rerun does nothing")
}

func TestUsesGVisorFollowsTheInstalledSandboxClasses(t *testing.T) {
	g := NewWithT(t)
	g.Expect(substrateinstall.Profile{}.UsesGVisor()).To(BeTrue(), "the default classes are gvisor,microvm")
	g.Expect(substrateinstall.Profile{SandboxClasses: "microvm,gvisor"}.UsesGVisor()).To(BeTrue())
	g.Expect(substrateinstall.Profile{SandboxClasses: "microvm, gvisor"}.UsesGVisor()).To(BeTrue())
	g.Expect(substrateinstall.Profile{SandboxClasses: "microvm"}.UsesGVisor()).To(BeFalse())
	g.Expect(substrateinstall.Profile{Platform: substrateinstall.PlatformGeneric}.UsesGVisor()).To(BeTrue())
}

// atelet is the DaemonSet that ate-setup leaves, without the image cache trust (the gke route has no overlay patch).
func atelet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "atelet", Namespace: "ate-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "atelet", Image: "atelet@sha256:0"}},
		}}},
	}
}
