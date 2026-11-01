// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Check runs the startup of root with args and the container environment env, without running any
// command's work: flag parsing, the environment and config resolution, the required flags and each
// command's PreRunE, exactly as a start would. It is how a test asks "would this container, as the
// chart renders it, start?" (test/chartsmoke). It also fails on a KOINON_* variable that nothing in
// the command reads: neither a flag of that command, an alias it declares (EnvAlias) nor a variable
// the binary declares (AllowEnv). The process environment is saved, cleared of every variable the
// command reads, set to env and restored afterwards, so Check does not run in parallel with other
// tests that use the environment.
func Check(root *cobra.Command, args []string, env map[string]string) (err error) {
	args = DefaultSubcommand(root, args)
	cmd, _, ferr := root.Find(args)
	if ferr != nil {
		return ferr
	}
	read := map[string]bool{}
	for _, n := range AllowedEnv(root) {
		read[n] = true
	}
	for _, fs := range []*pflag.FlagSet{cmd.NonInheritedFlags(), cmd.InheritedFlags()} {
		fs.VisitAll(func(f *pflag.Flag) {
			for _, n := range EnvNames(f) {
				read[n] = true
			}
		})
	}
	var unknown []string
	for n := range env {
		if strings.HasPrefix(n, EnvPrefix) && !read[n] {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s reads no environment variable %s: no flag, alias or declared variable has that name",
			cmd.CommandPath(), strings.Join(unknown, ", "))
	}

	touched := map[string]bool{}
	for n := range read {
		touched[n] = true
	}
	for n := range env {
		touched[n] = true
	}
	type saved struct {
		val string
		set bool
	}
	before := map[string]saved{}
	for n := range touched {
		v, ok := os.LookupEnv(n)
		before[n] = saved{v, ok}
		_ = os.Unsetenv(n)
	}
	defer func() {
		for n, s := range before {
			if s.set {
				_ = os.Setenv(n, s.val)
			} else {
				_ = os.Unsetenv(n)
			}
		}
	}()
	for n, v := range env {
		_ = os.Setenv(n, v)
	}

	neutralize(root)
	root.SetArgs(args)
	root.SetOut(discard{})
	root.SetErr(discard{})
	return root.Execute()
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// neutralize replaces what every command runs with nothing, and keeps its pre-run hooks.
func neutralize(c *cobra.Command) {
	if c.Run != nil || c.RunE != nil {
		c.Run, c.RunE = nil, func(*cobra.Command, []string) error { return nil }
	}
	for _, sub := range c.Commands() {
		neutralize(sub)
	}
}
