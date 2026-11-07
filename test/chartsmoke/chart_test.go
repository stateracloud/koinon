// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

const (
	chartDir  = "../../charts/koinon"
	namespace = "koinon"
	release   = "koinon"
)

// rendered is what `helm template` produced for one values file, sorted by kind.
type rendered struct {
	Jobs        []*batchv1.Job
	Deployments []*appsv1.Deployment
	// Objects are the ServiceAccounts, Secrets, ConfigMaps and RBAC objects, in render order.
	Objects []runtime.Object
	// Ignored lists the kinds the smoke test does not apply (policies, Services, ...).
	Ignored map[string]int
}

func (r *rendered) deployment(name string) *appsv1.Deployment {
	for _, d := range r.Deployments {
		if d.Name == name {
			return d
		}
	}
	return nil
}

// externalDatabase turns any values file into one with an external database (and no pod probes in the preflight): the components'
// DATABASE_URL then comes from a Secret that this suite fills with a URI for its Postgres
// container. (The cnpg URIs use sslmode=verify-full against the CNPG cluster's CA; a Postgres
// with a CA of its own would test that TLS set-up, not the chart's startup contract, and the
// rest of the pod specs does not depend on the database mode.)
var externalDatabase = []string{
	"--set", "postgres.mode=external",
	"--set", "postgres.external.secretName=koinon-db",
	// The preflight's two probes create pods and wait for them to run; envtest has no kubelet.
	"--set", "preflight.seccompProbe.enabled=false",
	"--set", "preflight.networkPolicyProbe.enabled=false",
}

// helmRender renders the chart as chart-check.sh and the install do: release and namespace
// "koinon", the values file as given, with the database overridden to external.
func helmRender(ctx context.Context, valuesFile string) *rendered {
	return helmRenderWith(ctx, valuesFile, externalDatabase...)
}

// cnpgUpgrade renders what a `helm upgrade` in cnpg mode runs after the release's objects: the
// install audit writer's grant hook (templates/install-audit-grants.yaml), alone.
var cnpgUpgrade = []string{
	"--set", "postgres.mode=cnpg", "--is-upgrade",
	"--set", "preflight.seccompProbe.enabled=false", "--set", "preflight.networkPolicyProbe.enabled=false",
	"--show-only", "templates/install-audit-grants.yaml",
}

// helmRenderWith renders the chart with the values file and extra helm arguments.
func helmRenderWith(ctx context.Context, valuesFile string, extra ...string) *rendered {
	helm, err := exec.LookPath(envOr("HELM", "helm"))
	if err != nil {
		panic(fmt.Sprintf("helm is required to render the chart (set HELM): %v", err))
	}
	var out, stderr bytes.Buffer
	args := append([]string{"template", release, chartDir, "--namespace", namespace, "-f", valuesFile}, extra...)
	cmd := exec.CommandContext(ctx, helm, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		panic(fmt.Sprintf("helm template -f %s: %v\n%s", valuesFile, err, stderr.String()))
	}
	r := &rendered{Ignored: map[string]int{}}
	dec := scheme.Codecs.UniversalDeserializer()
	for _, doc := range strings.Split(out.String(), "\n---") {
		if strings.TrimSpace(stripComments(doc)) == "" {
			continue
		}
		obj, gvk, err := dec.Decode([]byte(doc), nil, nil)
		if err != nil {
			// A kind the client-go scheme does not know (a CRD object): not applied here.
			var meta struct{ Kind string }
			_ = yaml.Unmarshal([]byte(doc), &meta)
			r.Ignored[meta.Kind]++
			continue
		}
		switch o := obj.(type) {
		case *batchv1.Job:
			r.Jobs = append(r.Jobs, o)
		case *appsv1.Deployment:
			r.Deployments = append(r.Deployments, o)
		case *corev1.ConfigMap, *corev1.Secret, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding,
			*rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding:
			r.Objects = append(r.Objects, obj)
		default:
			r.Ignored[gvk.Kind]++
		}
	}
	return r
}

func stripComments(doc string) string {
	var b strings.Builder
	for _, l := range strings.Split(doc, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// valuesFiles lists charts/koinon/ci/*-values.yaml.
func valuesFiles() []string {
	files, err := filepath.Glob(filepath.Join(chartDir, "ci", "*-values.yaml"))
	if err != nil || len(files) == 0 {
		panic(fmt.Sprintf("no values files in %s/ci: %v", chartDir, err))
	}
	return files
}
