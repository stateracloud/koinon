// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

// These tests render the kind tree of Substrate with the generic overlay (ApplyGenericOverlay) and check the
// objects that kustomize makes, not the patch files. The overlay is interim (upstream #1782 or #79), and a
// new pin can change what it patches without any text edit failing: only the render shows it.
//
// TestGenericOverlayRendersThePinnedTree renders the real pinned tree. It needs a clone of Substrate that
// holds the pinned commit (KOINON_SUBSTRATE_SOURCE, the same repository as `koinon-api substrate install
// --substrate-source`) and kubectl (its built-in kustomize reads local files only). Without them it skips;
// CI that sets KOINON_REQUIRE=substrate-source,kubectl turns the skip into a failure.
//
// TestRenderChecksFindWhatTheyLookFor renders a small tree of the same shape, so the checks themselves are
// tested on a machine that has kubectl but no clone.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/substrateinstall"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var (
	substrateSource = testenv.Var("substrate-source", "KOINON_SUBSTRATE_SOURCE")
	kubectlBinary   = testenv.Binary("kubectl")
)

// object is one rendered Kubernetes object, as untyped YAML: the checks read paths, not Go types, so a
// field that moves in a new pin fails with the path in the message.
type object map[string]any

func (o object) get(path ...string) any {
	var cur any = map[string]any(o)
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func (o object) kind() string { s, _ := o.get("kind").(string); return s }
func (o object) name() string { s, _ := o.get("metadata", "name").(string); return s }

func (o object) containers(list string) []map[string]any {
	var out []map[string]any
	raw, _ := o.get("spec", "template", "spec", list).([]any)
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

type rendered struct {
	text    string
	objects []object
}

func (r rendered) find(kind, name string) (object, bool) {
	for _, o := range r.objects {
		if o.kind() == kind && o.name() == name {
			return o, true
		}
	}
	return nil, false
}

func renderKustomize(t *testing.T, dir string) rendered {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "kubectl", "kustomize", dir)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", dir, err, stderr.String())
	}
	r := rendered{text: stdout.String()}
	for _, doc := range strings.Split(r.text, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var o object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("rendered document is not YAML: %v\n%s", err, doc)
		}
		r.objects = append(r.objects, o)
	}
	return r
}

// envFrom returns the environment of the named container as name -> entry.
func envOf(containers []map[string]any, container string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, c := range containers {
		if c["name"] != container {
			continue
		}
		list, _ := c["env"].([]any)
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				name, _ := m["name"].(string)
				out[name] = m
			}
		}
	}
	return out
}

// secretRef checks one environment entry: it takes its value from the key of the credential Secret, and
// holds no plain value next to it.
func secretRef(g Gomega, env map[string]map[string]any, name, key, where string) {
	e, ok := env[name]
	g.Expect(ok).To(BeTrue(), "%s: no environment variable %s", where, name)
	g.Expect(e).NotTo(HaveKey("value"), "%s: %s has a plain value", where, name)
	ref, _ := object(e).get("valueFrom", "secretKeyRef").(map[string]any)
	g.Expect(ref).To(HaveKeyWithValue("name", substrateinstall.RustFSSecretName), "%s: %s", where, name)
	g.Expect(ref).To(HaveKeyWithValue("key", key), "%s: %s", where, name)
}

func storageOf(o object, path ...string) any {
	return o.get(append(path, "resources", "requests", "storage")...)
}

// checkGenericRender is what the generic platform promises about the rendered kind tree.
func checkGenericRender(g Gomega, r rendered, bucket string) {
	// (d) no fixed object store credential anywhere, and the Secret is the only source of the keys.
	g.Expect(r.text).NotTo(ContainSubstring("rustfsadmin"))

	rustfs, ok := r.find("Deployment", "rustfs")
	g.Expect(ok).To(BeTrue(), "Deployment rustfs")
	secretRef(g, envOf(rustfs.containers("containers"), "rustfs"), "RUSTFS_ACCESS_KEY", "access-key", "Deployment rustfs")
	secretRef(g, envOf(rustfs.containers("containers"), "rustfs"), "RUSTFS_SECRET_KEY", "secret-key", "Deployment rustfs")

	// (b) the bucket-init Job makes the bucket of the profile, with the keys from the Secret.
	job, ok := r.find("Job", "rustfs-bucket-init")
	g.Expect(ok).To(BeTrue(), "Job rustfs-bucket-init")
	jobEnv := envOf(job.containers("containers"), "create-bucket")
	secretRef(g, jobEnv, "AWS_ACCESS_KEY_ID", "access-key", "Job rustfs-bucket-init")
	secretRef(g, jobEnv, "AWS_SECRET_ACCESS_KEY", "secret-key", "Job rustfs-bucket-init")
	g.Expect(jobEnv).To(HaveKey("BUCKET"))
	g.Expect(jobEnv["BUCKET"]).To(HaveKeyWithValue("value", bucket))
	g.Expect(r.text).NotTo(ContainSubstring("ate-snapshots"), "the fixed upstream bucket name is gone")

	// (d) the object store plugin of the API server and of atelet reads the Secret too.
	for _, w := range []struct{ kind, name string }{{"Deployment", "ate-api-server"}, {"DaemonSet", "atelet"}} {
		o, ok := r.find(w.kind, w.name)
		g.Expect(ok).To(BeTrue(), "%s %s", w.kind, w.name)
		env := envOf(o.containers("initContainers"), "objectstore-plugin")
		secretRef(g, env, "AWS_ACCESS_KEY_ID", "access-key", w.kind+" "+w.name)
		secretRef(g, env, "AWS_SECRET_ACCESS_KEY", "secret-key", w.kind+" "+w.name)
	}

	// (c) the sizes.
	pvc, ok := r.find("PersistentVolumeClaim", "rustfs-data")
	g.Expect(ok).To(BeTrue(), "PersistentVolumeClaim rustfs-data")
	g.Expect(storageOf(pvc, "spec")).To(Equal(substrateinstall.DefaultRustFSStorageSize))
	pg, ok := r.find("StatefulSet", "postgres")
	g.Expect(ok).To(BeTrue(), "StatefulSet postgres")
	templates, _ := pg.get("spec", "volumeClaimTemplates").([]any)
	g.Expect(templates).NotTo(BeEmpty())
	g.Expect(storageOf(object(templates[0].(map[string]any)), "spec")).To(Equal(substrateinstall.DefaultPostgresStorageSize))

	// (e) atelet logs at info and keeps no kind registry replacement.
	atelet, ok := r.find("DaemonSet", "atelet")
	g.Expect(ok).To(BeTrue(), "DaemonSet atelet")
	var args []string
	for _, c := range atelet.containers("containers") {
		list, _ := c["args"].([]any)
		for _, a := range list {
			s, _ := a.(string)
			args = append(args, s)
		}
	}
	g.Expect(args).To(ContainElement("--log-level=info"))
	g.Expect(args).NotTo(ContainElement("--log-level=debug"))
	for _, a := range args {
		g.Expect(a).NotTo(ContainSubstring("localhost-registry-replacement"))
	}
}

func TestGenericOverlayRendersThePinnedTree(t *testing.T) {
	testenv.RequireT(t, substrateSource, kubectlBinary)
	src := os.Getenv("KOINON_SUBSTRATE_SOURCE")
	dir := t.TempDir()
	archive := filepath.Join(t.TempDir(), "pin.tar")
	if out, err := exec.CommandContext(t.Context(), "git", "-C", src, "archive", "--format=tar", "--output="+archive, substrateinstall.PinnedCommit).CombinedOutput(); err != nil {
		t.Fatalf("%s does not hold the pinned commit %s: %v\n%s", src, substrateinstall.PinnedCommit, err, out)
	}
	if out, err := exec.CommandContext(t.Context(), "tar", "-xf", archive, "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("export the pinned tree: %v\n%s", err, out)
	}

	p := genericProfile()
	if err := p.ApplyGenericOverlay(dir, "ate-system"); err != nil {
		t.Fatalf("the generic overlay does not fit the pinned tree: %v", err)
	}
	checkGenericRender(NewWithT(t), renderKustomize(t, filepath.Join(dir, "manifests", "ate-install", "kind")), p.StorageBucket)
}

// The upstream objects the corrections patch, with the fixed credentials and the laptop sizes.
func upstreamShapedTree() map[string]string {
	const kind = "manifests/ate-install/kind/"
	env := "env:\n  - {name: AWS_ACCESS_KEY_ID, value: rustfsadmin}\n  - {name: AWS_SECRET_ACCESS_KEY, value: rustfsadmin}\n"
	return map[string]string{
		kind + "kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - rustfs.yaml\n  - api-server.yaml\n  - ./atelet\n  - ./postgres\n",
		kind + "rustfs.yaml": `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: rustfs-data, namespace: ate-system}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: rustfs, namespace: ate-system}
spec:
  selector: {matchLabels: {app: rustfs}}
  template:
    metadata: {labels: {app: rustfs}}
    spec:
      containers:
        - name: rustfs
          image: rustfs
          env:
            - {name: RUSTFS_ACCESS_KEY, value: rustfsadmin}
            - {name: RUSTFS_SECRET_KEY, value: rustfsadmin}
---
apiVersion: batch/v1
kind: Job
metadata: {name: rustfs-bucket-init, namespace: ate-system}
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: create-bucket
          image: aws-cli
          command: ["/bin/sh", "-c", "aws s3api create-bucket --bucket ate-snapshots"]
          ` + strings.ReplaceAll(env, "\n", "\n          ") + `
`,
		kind + "api-server.yaml": `apiVersion: apps/v1
kind: Deployment
metadata: {name: ate-api-server, namespace: ate-system}
spec:
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      containers:
        - {name: api, image: api}
      initContainers:
        - name: objectstore-plugin
          image: plugin
          ` + strings.ReplaceAll(env, "\n", "\n          ") + `
`,
		kind + "atelet/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - atelet.yaml
patches:
  - patch: |-
      apiVersion: apps/v1
      kind: DaemonSet
      metadata: {name: atelet, namespace: ate-system}
      spec:
        template:
          spec:
            containers:
            - name: atelet
              args:
              - --localhost-registry-replacement=kind-registry:5000
              - --log-level=debug
`,
		kind + "atelet/atelet.yaml": `apiVersion: apps/v1
kind: DaemonSet
metadata: {name: atelet, namespace: ate-system}
spec:
  selector: {matchLabels: {app: atelet}}
  template:
    metadata: {labels: {app: atelet}}
    spec:
      containers:
        - {name: atelet, image: atelet}
      initContainers:
        - name: objectstore-plugin
          image: plugin
          ` + strings.ReplaceAll(env, "\n", "\n          ") + `
`,
		kind + "postgres/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - postgres.yaml
patches:
  - target: {kind: StatefulSet, name: postgres}
    patch: |-
      - op: replace
        path: /spec/volumeClaimTemplates/0/spec/resources/requests/storage
        value: 1Gi
`,
		kind + "postgres/postgres.yaml": `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: postgres, namespace: ate-system}
spec:
  serviceName: postgres
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
        - {name: postgres, image: postgres}
  volumeClaimTemplates:
    - metadata: {name: data}
      spec:
        accessModes: [ReadWriteOnce]
        resources: {requests: {storage: 5Gi}}
`,
	}
}

// failures runs the checks with a Gomega that collects its failures, so a test can expect them.
func failures(r rendered, bucket string) []string {
	var out []string
	g := NewGomega(func(message string, _ ...int) { out = append(out, message); panic(out) })
	func() {
		defer func() { _ = recover() }()
		checkGenericRender(g, r, bucket)
	}()
	return out
}

func TestRenderChecksFindWhatTheyLookFor(t *testing.T) {
	testenv.RequireT(t, kubectlBinary)
	kind := func(dir string) string { return filepath.Join(dir, "manifests", "ate-install", "kind") }

	// Without the overlay the checks fail: the tree holds the fixed credentials and the laptop sizes.
	if len(failures(renderKustomize(t, kind(writeTree(t, upstreamShapedTree()))), "koinon-snaps")) == 0 {
		t.Fatal("the checks pass on the tree without the generic overlay")
	}

	// With the overlay they pass, and a bucket other than the profile's fails them.
	dir := writeTree(t, upstreamShapedTree())
	p := genericProfile()
	if err := p.ApplyGenericOverlay(dir, "ate-system"); err != nil {
		t.Fatal(err)
	}
	r := renderKustomize(t, kind(dir))
	if f := failures(r, p.StorageBucket); len(f) != 0 {
		t.Fatalf("the checks fail on the corrected tree: %v", f)
	}
	if len(failures(r, "another-bucket")) == 0 {
		t.Fatal("the checks pass for another bucket")
	}
}
