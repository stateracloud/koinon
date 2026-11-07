// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/cmd/koinonagentd"
	"github.com/stateracloud/koinon/internal/cmd/koinonapi"
	"github.com/stateracloud/koinon/internal/cmd/koinonreconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// commandTrees are the binaries whose command tree the contract test knows (ADR 0088). A binary
// that has moved to the shared CLI stack adds itself here; until then its containers are not
// checked. koinon-api, koinon-agentd and koinon-reconciler are all on the stack (koinon-private#497, koinon-private#458, koinon-private#457).
var commandTrees = map[string]func() *cobra.Command{
	"koinon-api":        koinonapi.NewRootCommand,
	"koinon-agentd":     koinonagentd.NewRootCommand,
	"koinon-reconciler": koinonreconciler.NewRootCommand,
}

// TestChartContract is the chart's side of the startup contract, without Docker or a cluster. It
// renders the chart for every charts/koinon/ci values file and, for each rendered container whose
// binary has a command tree, runs that binary's real startup (internal/cli.Check) with the args and
// the environment the chart gives it: the command must exist, every flag must be known and parse,
// every required flag must be present (by flag or environment), the rules between flags must hold,
// and no KOINON_* variable may be set that the command does not read. Only the command's work is
// skipped. koinon-private#284 (the migrate Job lacking a token a serve-only check wanted) and 3151301a
// (KOINON_KEK_DIR missing from a Deployment) are failures of this kind; the container-level smoke
// test (smoke_test.go, make test-chart-smoke) still boots the same containers against Postgres.
//
// A ConfigMap mounted into the container is written to a temporary directory and the paths under its
// mount point are rewritten to it, so the settings the reconciler reads from config.yaml are checked
// as rendered. A value that comes from a Secret or the downward API is replaced by a placeholder, so
// a check that parses such a value (a duration, a mode) cannot be proved here.
func TestChartContract(t *testing.T) {
	testenv.RequireT(t, testenv.BinaryNamed("helm", envOr("HELM", "helm"))) // set HELM for a helm outside PATH
	for _, values := range valuesFiles() {
		t.Run(filepath.Base(values), func(t *testing.T) {
			r := helmRender(t.Context(), values)
			// The cnpg-only post-upgrade grant hook (ADR 0100), which the external render lacks.
			r.Jobs = append(r.Jobs, helmRenderWith(t.Context(), values, cnpgUpgrade...).Jobs...)
			checked := 0
			for _, c := range containersOf(r) {
				newRoot, ok := commandTrees[imageName(c.Image)]
				if !ok {
					continue
				}
				checked++
				t.Run(c.where+"/"+c.Name, func(t *testing.T) {
					if len(c.Command) > 0 {
						t.Fatalf("command %v overrides the image's entrypoint; the contract is the binary's own args", c.Command)
					}
					if len(c.EnvFrom) > 0 {
						t.Fatalf("envFrom cannot be checked against the flags; list the variables")
					}
					env := map[string]string{}
					for _, e := range c.Env {
						env[e.Name] = envValue(e)
					}
					args := c.Args
					if mounts := configMapFiles(t, r, c); len(mounts) > 0 {
						args = mounts.rewriteAll(c.Args)
						for k, v := range env {
							env[k] = mounts.rewrite(v)
						}
					}
					if err := cli.Check(newRoot(), args, env); err != nil {
						t.Errorf("%s %v does not start: %v", imageName(c.Image), c.Args, err)
					}
				})
			}
			if checked == 0 {
				t.Fatalf("no container of %s runs a binary with a command tree", values)
			}
			// ADR 0100 decision 1.1a: on an external database (this render) nothing creates the install
			// audit writer role after the migrate Job, so the Job requires it; the cnpg grant hook waits
			// for it instead and must not. No Job checks koinon-api's membership: koinon-api checks its
			// own login (maintainer decision 2026-11-07), the migration cannot know it.
			for _, j := range r.Jobs {
				args := j.Spec.Template.Spec.Containers[0].Args
				if slices.Contains(args, "--install-writer-member") {
					t.Errorf("Job %s checks koinon-api's membership; koinon-api checks its own: %v", j.Name, args)
				}
				switch j.Name {
				case "koinon-migrate", "koinon-migrate-install":
					if !slices.Contains(args, "--require-install-writer") {
						t.Errorf("the external-database migrate Job %s lacks --require-install-writer: %v", j.Name, args)
					}
				case "koinon-migrate-grants":
					if slices.Contains(args, "--require-install-writer") {
						t.Errorf("the cnpg grant hook must wait for the role, not require it: %v", args)
					}
				}
			}
		})
	}
}

// podContainer is a rendered container and where it runs.
type podContainer struct {
	corev1.Container
	where   string
	volumes []corev1.Volume
}

// fileMounts maps a container path (a ConfigMap volume's mount point) to where the test wrote it.
type fileMounts map[string]string

func (m fileMounts) rewrite(v string) string {
	for from, to := range m {
		if v == from || strings.HasPrefix(v, from+"/") {
			return to + strings.TrimPrefix(v, from)
		}
	}
	return v
}

func (m fileMounts) rewriteAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = m.rewrite(v)
	}
	return out
}

// configMapFiles writes each ConfigMap volume the container mounts to a temporary directory.
func configMapFiles(t *testing.T, r *rendered, c podContainer) fileMounts {
	t.Helper()
	out := fileMounts{}
	for _, m := range c.VolumeMounts {
		for _, v := range c.volumes {
			if v.Name != m.Name || v.ConfigMap == nil {
				continue
			}
			var cm *corev1.ConfigMap
			for _, o := range r.Objects {
				if o, ok := o.(*corev1.ConfigMap); ok && o.Name == v.ConfigMap.Name {
					cm = o
				}
			}
			if cm == nil {
				// The operator's own ConfigMap (the OpenBao CA): its keys are the volume's items,
				// with placeholder content, since only the startup is checked here.
				if len(v.ConfigMap.Items) == 0 {
					t.Fatalf("volume %s: ConfigMap %s is not rendered, and the volume names no items an operator's ConfigMap would hold", v.Name, v.ConfigMap.Name)
				}
				cm = &corev1.ConfigMap{Data: map[string]string{}}
				for _, it := range v.ConfigMap.Items {
					cm.Data[it.Path] = "placeholder-" + it.Key
				}
			}
			dir := t.TempDir()
			for key, data := range cm.Data {
				if err := os.WriteFile(filepath.Join(dir, key), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out[m.MountPath] = dir
		}
	}
	return out
}

func containersOf(r *rendered) []podContainer {
	var out []podContainer
	add := func(where string, spec corev1.PodSpec) {
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			out = append(out, podContainer{c, where, spec.Volumes})
		}
	}
	for _, j := range r.Jobs {
		add("job/"+j.Name, j.Spec.Template.Spec)
	}
	for _, d := range r.Deployments {
		add("deployment/"+d.Name, d.Spec.Template.Spec)
	}
	return out
}

func envValue(e corev1.EnvVar) string {
	if e.ValueFrom != nil {
		return "placeholder-" + fmt.Sprint(e.Name)
	}
	return e.Value
}
