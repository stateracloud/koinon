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
	"context"
	"encoding/json"
	"io"
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
	gitBinary       = testenv.Binary("git")
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
	cmd := exec.CommandContext(t.Context(), "kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone", dir)
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

// envOf returns the environment of the named container as name -> entry.
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
	g.Expect(firstContainer(rustfs, "containers", "rustfs")["image"]).NotTo(BeEmpty(), "rustfs has no image")
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
	g.Expect(job.containers("containers")[0]["command"]).NotTo(BeEmpty(), "the Job keeps its command")
	jobText, _ := yaml.Marshal(map[string]any(job))
	g.Expect(string(jobText)).NotTo(ContainSubstring("ate-snapshots"), "the fixed upstream bucket name is gone from the Job")

	// (d) the object store plugin of the API server and of atelet reads the Secret too.
	for _, w := range []struct{ kind, name string }{{"Deployment", "ate-api-server"}, {"DaemonSet", "atelet"}} {
		o, ok := r.find(w.kind, w.name)
		g.Expect(ok).To(BeTrue(), "%s %s", w.kind, w.name)
		env := envOf(o.containers("initContainers"), "objectstore-plugin")
		// A patch makes a container of its own when the name does not match: it then has an env and no image.
		g.Expect(firstContainer(o, "initContainers", "objectstore-plugin")["image"]).NotTo(BeEmpty(), "%s %s: objectstore-plugin has no image", w.kind, w.name)
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

	// (f) atelet trusts the image cache CA: SSL_CERT_DIR names the mount of the optional ConfigMap, and the atelet
	// container keeps its image (the patch adds no container and no image).
	ateletC := firstContainerOf(atelet, "containers", "atelet")
	g.Expect(ateletC).NotTo(BeNil(), "the atelet container")
	g.Expect(ateletC["image"]).NotTo(BeEmpty())
	g.Expect(envOf(atelet.containers("containers"), "atelet")["SSL_CERT_DIR"]["value"]).To(Equal("/etc/koinon/imagecache-ca"))
	g.Expect(ateletC["volumeMounts"]).To(ContainElement(HaveKeyWithValue("mountPath", "/etc/koinon/imagecache-ca")))
	var cacheVol map[string]any
	for _, v := range atelet.get("spec", "template", "spec", "volumes").([]any) {
		if m, _ := v.(map[string]any); m["name"] == "koinon-imagecache-ca" {
			cacheVol = m
		}
	}
	g.Expect(cacheVol).NotTo(BeNil(), "the cache CA volume")
	g.Expect(cacheVol["configMap"]).To(And(HaveKeyWithValue("name", "koinon-imagecache-ca"), HaveKeyWithValue("optional", true)))
	g.Expect(cacheVol).NotTo(HaveKey("hostPath"))
}

func firstContainerOf(o object, list, name string) map[string]any {
	for _, c := range o.containers(list) {
		if c["name"] == name {
			return c
		}
	}
	return nil
}

// execRunner runs the real git for the export step of the installer.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, name string, args, env []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	cmd.Env = append(os.Environ(), env...)
	return cmd.Run()
}

func (execRunner) Output(ctx context.Context, dir, name string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// genericNamespace is Substrate's fixed namespace (preflight.SubstrateConfig defaults to it).
const genericNamespace = "ate-system"

func TestGenericOverlayRendersThePinnedTree(t *testing.T) {
	testenv.RequireT(t, substrateSource, kubectlBinary, gitBinary)
	// The export is the installer's own: a fresh bare clone, no filter driver or replace ref of the source.
	inst := &substrateinstall.Installer{Runner: execRunner{}, SourceRepo: os.Getenv("KOINON_SUBSTRATE_SOURCE"), Out: io.Discard}
	dir := filepath.Join(t.TempDir(), "src")
	if err := inst.ExportPin(t.Context(), t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}

	p := genericProfile()
	if err := p.ApplyGenericOverlay(dir, genericNamespace); err != nil {
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

// stop is what the collecting Gomega panics with, so failures() ends the checks at the first failure and
// tells that panic from a runtime panic in the checks themselves.
type stop struct{}

// failures runs the checks with a Gomega that collects its failure. A panic that is not a Gomega failure
// (an index or a type assertion in the checks) goes on, so it cannot read as "no failure".
func failures(r rendered, bucket string) []string {
	var out []string
	g := NewGomega(func(message string, _ ...int) { out = append(out, message); panic(stop{}) })
	func() {
		defer func() {
			if v := recover(); v != nil {
				if _, ok := v.(stop); !ok {
					panic(v)
				}
			}
		}()
		checkGenericRender(g, r, bucket)
	}()
	return out
}

// edit returns a deep copy of r with fn applied to the named object (fn gets nil text changes through r.text).
func edit(t *testing.T, r rendered, kind, name string, fn func(o object)) rendered {
	t.Helper()
	raw, err := json.Marshal(r.objects)
	if err != nil {
		t.Fatal(err)
	}
	out := rendered{text: r.text}
	if err := json.Unmarshal(raw, &out.objects); err != nil {
		t.Fatal(err)
	}
	if kind != "" {
		o, ok := out.find(kind, name)
		if !ok {
			t.Fatalf("no %s %s", kind, name)
		}
		fn(o)
	} else {
		fn(nil)
	}
	return out
}

func firstContainer(o object, list, name string) map[string]any {
	for _, c := range o.containers(list) {
		if c["name"] == name {
			return c
		}
	}
	return nil
}

func TestRenderChecksFindWhatTheyLookFor(t *testing.T) {
	testenv.RequireT(t, kubectlBinary)
	kind := func(dir string) string { return filepath.Join(dir, "manifests", "ate-install", "kind") }

	// Without the overlay the checks fail: the tree holds the fixed credentials and the laptop sizes.
	if len(failures(renderKustomize(t, kind(writeTree(t, upstreamShapedTree()))), "koinon-snaps")) == 0 {
		t.Fatal("the checks pass on the tree without the generic overlay")
	}

	// With the overlay they pass.
	dir := writeTree(t, upstreamShapedTree())
	p := genericProfile()
	if err := p.ApplyGenericOverlay(dir, genericNamespace); err != nil {
		t.Fatal(err)
	}
	r := renderKustomize(t, kind(dir))
	if f := failures(r, p.StorageBucket); len(f) != 0 {
		t.Fatalf("the checks fail on the corrected tree: %v", f)
	}
	if len(failures(r, "another-bucket")) == 0 {
		t.Fatal("the checks pass for another bucket")
	}

	// Each check fails when its one correction is undone: no check is empty.
	secretToPlain := func(c map[string]any, name string) {
		for _, e := range c["env"].([]any) {
			if m := e.(map[string]any); m["name"] == name {
				delete(m, "valueFrom")
				m["value"] = "koinon_test_not-a-real-token"
			}
		}
	}
	undo := map[string]func(*testing.T) rendered{
		"a plain value in rustfs": func(t *testing.T) rendered {
			return edit(t, r, "Deployment", "rustfs", func(o object) { secretToPlain(firstContainer(o, "containers", "rustfs"), "RUSTFS_SECRET_KEY") })
		},
		"a plain value in the bucket Job": func(t *testing.T) rendered {
			return edit(t, r, "Job", "rustfs-bucket-init", func(o object) { secretToPlain(firstContainer(o, "containers", "create-bucket"), "AWS_ACCESS_KEY_ID") })
		},
		"no BUCKET in the bucket Job": func(t *testing.T) rendered {
			return edit(t, r, "Job", "rustfs-bucket-init", func(o object) { firstContainer(o, "containers", "create-bucket")["env"] = []any{} })
		},
		"a plain value in the api server plugin": func(t *testing.T) rendered {
			return edit(t, r, "Deployment", "ate-api-server", func(o object) {
				secretToPlain(firstContainer(o, "initContainers", "objectstore-plugin"), "AWS_SECRET_ACCESS_KEY")
			})
		},
		"a plain value in the atelet plugin": func(t *testing.T) rendered {
			return edit(t, r, "DaemonSet", "atelet", func(o object) {
				secretToPlain(firstContainer(o, "initContainers", "objectstore-plugin"), "AWS_ACCESS_KEY_ID")
			})
		},
		"an object store claim of 1Gi": func(t *testing.T) rendered {
			return edit(t, r, "PersistentVolumeClaim", "rustfs-data", func(o object) {
				o["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] = "1Gi"
			})
		},
		"a PostgreSQL volume of 1Gi": func(t *testing.T) rendered {
			return edit(t, r, "StatefulSet", "postgres", func(o object) {
				tpl := o["spec"].(map[string]any)["volumeClaimTemplates"].([]any)[0].(map[string]any)
				tpl["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] = "1Gi"
			})
		},
		"atelet at debug": func(t *testing.T) rendered {
			return edit(t, r, "DaemonSet", "atelet", func(o object) { firstContainer(o, "containers", "atelet")["args"] = []any{"--log-level=debug"} })
		},
		"the registry replacement": func(t *testing.T) rendered {
			return edit(t, r, "DaemonSet", "atelet", func(o object) {
				c := firstContainer(o, "containers", "atelet")
				c["args"] = append(c["args"].([]any), "--localhost-registry-replacement=kind-registry:5000")
			})
		},
		"a container that a patch made alone": func(t *testing.T) rendered {
			return edit(t, r, "Deployment", "ate-api-server", func(o object) { delete(firstContainer(o, "initContainers", "objectstore-plugin"), "image") })
		},
		"no SSL_CERT_DIR on atelet": func(t *testing.T) rendered {
			return edit(t, r, "DaemonSet", "atelet", func(o object) {
				c := firstContainer(o, "containers", "atelet")
				var keep []any
				for _, e := range c["env"].([]any) {
					if e.(map[string]any)["name"] != "SSL_CERT_DIR" {
						keep = append(keep, e)
					}
				}
				c["env"] = keep
			})
		},
		"the fixed upstream credential": func(t *testing.T) rendered {
			return edit(t, r, "", "", func(object) {})
		},
	}
	for name, mk := range undo {
		bad := mk(t)
		if name == "the fixed upstream credential" {
			bad.text += "\nvalue: rustfsadmin\n"
		}
		if len(failures(bad, p.StorageBucket)) == 0 {
			t.Errorf("the checks pass with %s", name)
		}
	}
}
