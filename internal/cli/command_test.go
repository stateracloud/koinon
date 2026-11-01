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
