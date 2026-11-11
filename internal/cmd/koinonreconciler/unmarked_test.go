// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/reconciler"
)

var _ = Describe("koinon-reconciler unmarked list", func() {
	run := func(args []string, env map[string]string) (unmarkedConfig, string, error) {
		var got unmarkedConfig
		root := newRootCommandWith(nil, func(_ context.Context, c unmarkedConfig, out io.Writer) error {
			got = c
			return printUnmarked(out, []reconciler.UnmarkedObject{{Workspace: "kw-1", Kind: "sandbox", Name: "ka-x", Reason: reconciler.ReasonOrgNotInDB}}, c.json)
		})
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(name, cli.EnvPrefix) || name == "DATABASE_URL" || name == "POD_NAMESPACE" {
				old := os.Getenv(name)
				Expect(os.Unsetenv(name)).To(Succeed())
				DeferCleanup(func() { _ = os.Setenv(name, old) })
			}
		}
		for k, v := range env {
			GinkgoT().Setenv(k, v)
		}
		var out bytes.Buffer
		root.SetArgs(args)
		root.SetOut(&out)
		root.SetErr(io.Discard)
		err := root.Execute()
		return got, out.String(), err
	}
	// The reconciler pod's environment: what the chart's Deployment and config file give it.
	podEnv := func() map[string]string {
		return map[string]string{
			"KOINON_CONFIG": writeTemp(chartConfig), "DATABASE_URL": "postgres://x@db/koinon", "KOINON_INSTALL_MARK": "koinon_test_not-a-real-mark",
		}
	}

	It("needs nothing the reconciler pod does not already have", func() {
		c, out, err := run([]string{"unmarked", "list"}, podEnv())
		Expect(err).NotTo(HaveOccurred())
		Expect(c.databaseURL).To(Equal("postgres://x@db/koinon"))
		Expect(c.openshellAddress).To(Equal("openshell.openshell.svc:8080"))
		Expect(c.openshellTLSDir).To(Equal("/etc/koinon/openshell-tls"))
		Expect(c.installMark).To(Equal("koinon_test_not-a-real-mark"))
		Expect(c.flavors).To(HaveKeyWithValue(store.AgentFlavorOpencode, reconciler.Flavor{
			Image: "ghcr.io/stateracloud/koinon/flavors/opencode:0.1.0", Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode],
		}), "the configured flavors, whose profiles the reconciler claims")
		Expect(strings.Fields(out)).To(Equal([]string{"kw-1", "sandbox", "ka-x", "org-not-in-db"}), "one line: workspace, kind, name, reason")
	})

	It("prints a JSON array with --json, [] when nothing is unmarked", func() {
		_, out, err := run([]string{"unmarked", "list", "--json"}, podEnv())
		Expect(err).NotTo(HaveOccurred())
		var got []map[string]string
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(Equal([]map[string]string{{"workspace": "kw-1", "kind": "sandbox", "name": "ka-x", "reason": "org-not-in-db"}}))

		var empty bytes.Buffer
		Expect(printUnmarked(&empty, nil, true)).To(Succeed())
		Expect(strings.TrimSpace(empty.String())).To(Equal("[]"))
		empty.Reset()
		Expect(printUnmarked(&empty, nil, false)).To(Succeed())
		Expect(empty.String()).To(BeEmpty())
	})

	It("names every missing setting and takes no arguments", func() {
		_, _, err := run([]string{"unmarked", "list"}, nil)
		Expect(err).To(MatchError(And(ContainSubstring("database-url"), ContainSubstring("openshell-address"), ContainSubstring("install-mark"))))
		_, _, err = run([]string{"unmarked", "list", "extra"}, podEnv())
		Expect(err).To(HaveOccurred())
	})

	It("is part of the command tree the chart contract test builds", func() {
		root := NewRootCommand()
		cmd, _, err := root.Find([]string{"unmarked", "list"})
		Expect(err).NotTo(HaveOccurred())
		Expect(cmd.Name()).To(Equal("list"))
	})
})

func writeTemp(content string) string {
	f, err := os.CreateTemp(GinkgoT().TempDir(), "config-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	_, err = f.WriteString(content)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())
	return f.Name()
}
