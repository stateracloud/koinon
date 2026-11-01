// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/cmd/koinonagentd"
	"github.com/stateracloud/koinon/internal/cmd/koinonapi"
)

// commandTrees are the binaries whose command tree the contract test knows (ADR 0088). A binary
// that has moved to the shared CLI stack adds itself here; until then its containers are not
// checked (the reconciler: #457).
var commandTrees = map[string]func() *cobra.Command{
	"koinon-api":    koinonapi.NewRootCommand,
	"koinon-agentd": koinonagentd.NewRootCommand,
}

// TestChartContract is the chart's side of the startup contract, without Docker or a cluster. It
// renders the chart for every charts/koinon/ci values file and, for each rendered container whose
// binary has a command tree, runs that binary's real startup (internal/cli.Check) with the args and
// the environment the chart gives it: the command must exist, every flag must be known and parse,
// every required flag must be present (by flag or environment), the rules between flags must hold,
// and no KOINON_* variable may be set that the command does not read. Only the command's work is
// skipped. #284 (the migrate Job lacking a token a serve-only check wanted) and 3151301a
// (KOINON_KEK_DIR missing from a Deployment) are failures of this kind; the container-level smoke
// test (smoke_test.go, make test-chart-smoke) still boots the same containers against Postgres.
//
// A value that comes from a Secret, a ConfigMap or the downward API is replaced by a placeholder,
// so a check that parses such a value (a duration, a mode) cannot be proved here.
func TestChartContract(t *testing.T) {
	if _, err := exec.LookPath(envOr("HELM", "helm")); err != nil {
		t.Skip("helm is not installed (set HELM)")
	}
	for _, values := range valuesFiles() {
		t.Run(filepath.Base(values), func(t *testing.T) {
			r := helmRender(t.Context(), values)
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
					if err := cli.Check(newRoot(), c.Args, env); err != nil {
						t.Errorf("%s %v does not start: %v", imageName(c.Image), c.Args, err)
					}
				})
			}
			if checked == 0 {
				t.Fatalf("no container of %s runs a binary with a command tree", values)
			}
		})
	}
}

// podContainer is a rendered container and where it runs.
type podContainer struct {
	corev1.Container
	where string
}

func containersOf(r *rendered) []podContainer {
	var out []podContainer
	add := func(where string, spec corev1.PodSpec) {
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			out = append(out, podContainer{c, where})
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
