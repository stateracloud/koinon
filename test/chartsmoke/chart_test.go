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
	Jobs         []*batchv1.Job
	Deployments  []*appsv1.Deployment
	ConfigMaps   []*corev1.ConfigMap
	Secrets      []*corev1.Secret
	ServiceAcct  []*corev1.ServiceAccount
	Roles        []*rbacv1.Role
	RoleBindings []*rbacv1.RoleBinding
	ClusterRoles []*rbacv1.ClusterRole
	CRBindings   []*rbacv1.ClusterRoleBinding
	// Ignored lists the kinds the smoke test does not apply (policies, Services, ...).
	Ignored map[string]int
}

func (r *rendered) job(name string) *batchv1.Job {
	for _, j := range r.Jobs {
		if j.Name == name {
			return j
		}
	}
	return nil
}

func (r *rendered) deployment(name string) *appsv1.Deployment {
	for _, d := range r.Deployments {
		if d.Name == name {
			return d
		}
	}
	return nil
}

// smokeValues are the operator inputs the CI values files leave open because rendering does not
// need them, and the two preflight probes that create pods (envtest has no kubelet to run them).
var smokeValues = []string{
	// The reconciler and agentd refuse to start without an OpenShell address. Nothing listens
	// there: the components must get past their configuration, not talk to a gateway.
	"--set", "openshell.address=openshell.openshell.svc.cluster.local:8080",
	"--set", "preflight.seccompProbe.enabled=false",
	"--set", "preflight.networkPolicyProbe.enabled=false",
}

// helmRender renders the chart exactly as chart-check.sh and the install do: release and
// namespace "koinon", the values file as given.
func helmRender(ctx context.Context, valuesFile string) *rendered {
	helm, err := exec.LookPath(envOr("HELM", "helm"))
	if err != nil {
		panic(fmt.Sprintf("helm is required to render the chart (set HELM): %v", err))
	}
	var out, stderr bytes.Buffer
	args := append([]string{"template", release, chartDir, "--namespace", namespace, "-f", valuesFile}, smokeValues...)
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
		case *corev1.ConfigMap:
			r.ConfigMaps = append(r.ConfigMaps, o)
		case *corev1.Secret:
			r.Secrets = append(r.Secrets, o)
		case *corev1.ServiceAccount:
			r.ServiceAcct = append(r.ServiceAcct, o)
		case *rbacv1.Role:
			r.Roles = append(r.Roles, o)
		case *rbacv1.RoleBinding:
			r.RoleBindings = append(r.RoleBindings, o)
		case *rbacv1.ClusterRole:
			r.ClusterRoles = append(r.ClusterRoles, o)
		case *rbacv1.ClusterRoleBinding:
			r.CRBindings = append(r.CRBindings, o)
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

// postgresMode is postgres.mode as the values file sets it (the chart's default is cnpg).
func postgresMode(valuesFile string) string {
	b, err := os.ReadFile(valuesFile)
	if err != nil {
		panic(err)
	}
	var v struct {
		Postgres struct{ Mode string } `json:"postgres"`
	}
	if err := yaml.Unmarshal(b, &v); err != nil {
		panic(fmt.Sprintf("%s: %v", valuesFile, err))
	}
	if v.Postgres.Mode == "" {
		return "cnpg"
	}
	return v.Postgres.Mode
}
