// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/cli"
)

type settings struct {
	workers int
	lease   time.Duration
	tlsDir  string
	token   string
}

// tree is a root with a default subcommand "run" and a "tool" command.
func tree(got *settings) *cobra.Command {
	root := &cobra.Command{Use: "t"}
	cli.Bind(root, nil)
	cli.DefaultTo(root, "run")
	run := &cobra.Command{Use: "run", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return nil }}
	run.Flags().IntVar(&got.workers, "workers", 8, "")
	run.Flags().DurationVar(&got.lease, "lease", time.Second, "")
	run.Flags().StringVar(&got.tlsDir, "openshell-tls-dir", "", "")
	run.Flags().StringVar(&got.token, "token", "", "")
	cli.EnvAlias(run, "token", "LEGACY_TOKEN")
	_ = run.MarkFlagRequired("token")
	tool := &cobra.Command{Use: "tool", RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(run, tool)
	return root
}

var _ = Describe("the command stack", func() {
	var got settings
	check := func(env map[string]string, args ...string) error { return cli.Check(tree(&got), args, env) }

	It("sets flags from the environment, and command-line flags win", func() {
		Expect(check(map[string]string{"KOINON_OPENSHELL_TLS_DIR": "/tls", "KOINON_WORKERS": "3", "WORKERS": "99", "KOINON_TOKEN": "t"},
			"--workers=4")).To(Succeed())
		Expect(got.tlsDir).To(Equal("/tls"))
		Expect(got.workers).To(Equal(4))
		Expect(got.lease).To(Equal(time.Second), "unset variables keep the default")
	})

	It("reads the aliases after the KOINON_ variable", func() {
		Expect(check(map[string]string{"LEGACY_TOKEN": "old"})).To(Succeed())
		Expect(got.token).To(Equal("old"))
		Expect(check(map[string]string{"LEGACY_TOKEN": "old", "KOINON_TOKEN": "new"})).To(Succeed())
		Expect(got.token).To(Equal("new"))
	})

	It("treats an empty variable as set", func() {
		Expect(check(map[string]string{"KOINON_TOKEN": "t", "KOINON_OPENSHELL_TLS_DIR": ""})).To(Succeed())
		Expect(got.tlsDir).To(BeEmpty())
	})

	It("checks the required flags after the environment was read", func() {
		Expect(check(nil)).To(MatchError(ContainSubstring(`"token" not set`)))
	})

	It("names the variable whose value does not parse", func() {
		err := check(map[string]string{"KOINON_TOKEN": "t", "KOINON_WORKERS": "many"})
		Expect(err).To(MatchError(ContainSubstring("KOINON_WORKERS")))
	})

	It("refuses KOINON_ variables the command does not read", func() {
		Expect(check(map[string]string{"KOINON_TOKEN": "t", "KOINON_WORKER": "3"})).To(MatchError(ContainSubstring("KOINON_WORKER")))
	})

	It("falls back to the default subcommand only without one", func() {
		Expect(cli.DefaultSubcommand(tree(&got), nil)).To(Equal([]string{"run"}))
		Expect(cli.DefaultSubcommand(tree(&got), []string{"--workers=2"})).To(Equal([]string{"run", "--workers=2"}))
		Expect(cli.DefaultSubcommand(tree(&got), []string{"tool", "--x"})).To(Equal([]string{"tool", "--x"}))
		Expect(cli.DefaultSubcommand(tree(&got), []string{"--help"})).To(Equal([]string{"--help"}))
	})

	It("reads a config file below the environment and above the default", func() {
		cfg := filepath.Join(GinkgoT().TempDir(), "c.yaml")
		Expect(os.WriteFile(cfg, []byte("workers: 7\nlease: 9s\ntoken: t\n"), 0o600)).To(Succeed())
		v := cli.NewViper()
		v.SetConfigFile(cfg)
		Expect(v.ReadInConfig()).To(Succeed())

		root := tree(&got)
		cli.Bind(root, v)
		run, _, _ := root.Find([]string{"run"})
		GinkgoT().Setenv("KOINON_WORKERS", "5")
		Expect(cli.Resolve(run, v)).To(Succeed())
		Expect(got.workers).To(Equal(5), "the environment beats the file")
		Expect(got.lease).To(Equal(9*time.Second), "the file beats the default")
	})

	It("styles output only on a terminal without NO_COLOR", func() {
		GinkgoT().Setenv("NO_COLOR", "1")
		Expect(cli.Styled(os.Stdout)).To(BeFalse())
	})
})

var _ = Describe("the config file", func() {
	type got struct {
		mode, name string
		jobs       int
		on         bool
	}
	var g got
	root := func() *cobra.Command {
		g = got{}
		r := &cobra.Command{Use: "t"}
		cli.ConfigFile(r, cli.NewViper())
		run := &cobra.Command{Use: "run", RunE: func(*cobra.Command, []string) error { return nil }}
		run.Flags().StringVar(&g.mode, "mode", "standalone", "")
		run.Flags().StringVar(&g.name, "name", "", "")
		run.Flags().IntVar(&g.jobs, "jobs", 0, "")
		run.Flags().BoolVar(&g.on, "on", false, "")
		cli.ConfigKey(run, "mode", "gateway.mode")
		cli.ConfigKey(run, "jobs", "reconciler.jobs")
		cli.ConfigKey(run, "on", "feature.enabled")
		r.AddCommand(run)
		return r
	}
	file := func(content string) string {
		p := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(p, []byte(content), 0o600)).To(Succeed())
		return p
	}
	const yamlFile = "gateway:\n  mode: kubernetes\nreconciler:\n  jobs: 4\nfeature:\n  enabled: true\nname: from-file\n"

	It("sets flags from the file, at their config key or their own name, ranked below the environment and the flag", func() {
		p := file(yamlFile)
		Expect(cli.Check(root(), []string{"run", "--config", p}, nil)).To(Succeed())
		Expect(g).To(Equal(got{mode: "kubernetes", name: "from-file", jobs: 4, on: true}))

		Expect(cli.Check(root(), []string{"run"}, map[string]string{"KOINON_CONFIG": p, "KOINON_JOBS": "9", "KOINON_ON": "false"})).To(Succeed())
		Expect(g).To(Equal(got{mode: "kubernetes", name: "from-file", jobs: 9, on: false}))

		Expect(cli.Check(root(), []string{"run", "--jobs=2", "--mode=standalone"}, map[string]string{"KOINON_CONFIG": p, "KOINON_JOBS": "9"})).To(Succeed())
		Expect(g).To(Equal(got{mode: "standalone", name: "from-file", jobs: 2, on: true}))
	})

	It("keeps the default where the file has an empty value, and where it has no key", func() {
		Expect(cli.Check(root(), []string{"run", "--config", file("gateway:\n  mode: \"\"\n")}, nil)).To(Succeed())
		Expect(g.mode).To(Equal("standalone"))
		Expect(cli.Check(root(), []string{"run"}, nil)).To(Succeed())
		Expect(g.mode).To(Equal("standalone"))
	})

	It("lets an empty variable override the file", func() {
		Expect(cli.Check(root(), []string{"run"}, map[string]string{"KOINON_CONFIG": file(yamlFile), "KOINON_NAME": ""})).To(Succeed())
		Expect(g.name).To(BeEmpty())
	})

	It("names the config key whose value does not parse, and an unreadable file", func() {
		Expect(cli.Check(root(), []string{"run", "--config", file("reconciler:\n  jobs: many\n")}, nil)).
			To(MatchError(ContainSubstring("config key reconciler.jobs")))
		Expect(cli.Check(root(), []string{"run", "--config", filepath.Join(GinkgoT().TempDir(), "none.yaml")}, nil)).
			To(MatchError(ContainSubstring("read config")))
	})
})
