// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/term"

	"github.com/stateracloud/koinon/internal/project"
)

// The command-line stack of every Koinon binary (ADR 0088): cobra commands, pflag flags and viper
// as the one owner of where a setting comes from. The precedence is, highest first:
//
//  1. the flag on the command line,
//  2. the environment variable (KOINON_ plus the flag name in upper case, dashes as underscores,
//     or an alias the flag declares with EnvAlias),
//  3. the config file, when the binary reads one (the key is the flag name),
//  4. the flag's default.
//
// An environment variable that is set but empty counts as set: KOINON_OTLP_ADDR= turns the OTLP
// receiver off, as --otlp-addr= does.

const (
	aliasAnnotation = "koinon.dev/env-alias"
	extraEnvKey     = "koinon.dev/env"
	defaultKey      = "koinon.dev/default-subcommand"
)

// EnvAlias makes the flag of cmd also readable from the environment variables names, after its
// KOINON_ one (for example DATABASE_URL, set by the dev manifests, and POD_NAMESPACE from the
// downward API). It panics when the flag does not exist: that is a bug in the command definition.
func EnvAlias(cmd *cobra.Command, flag string, names ...string) {
	f := cmd.Flags().Lookup(flag)
	if f == nil {
		f = cmd.PersistentFlags().Lookup(flag)
	}
	if f == nil {
		panic(fmt.Sprintf("cli.EnvAlias: command %q has no flag --%s", cmd.Name(), flag))
	}
	if f.Annotations == nil {
		f.Annotations = map[string][]string{}
	}
	f.Annotations[aliasAnnotation] = append(f.Annotations[aliasAnnotation], names...)
}

// EnvNames lists the environment variables that set f, in precedence order.
func EnvNames(f *pflag.Flag) []string {
	return append([]string{EnvName(f.Name)}, f.Annotations[aliasAnnotation]...)
}

// AllowEnv declares KOINON_* environment variables that root's binary reads without a flag (for
// example KOINON_MEMORY_LIMIT_BYTES), so the chart contract test does not call them unknown.
func AllowEnv(root *cobra.Command, names ...string) {
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations[extraEnvKey] = strings.Join(append(AllowedEnv(root), names...), ",")
}

// AllowedEnv lists what AllowEnv declared on root.
func AllowedEnv(root *cobra.Command) []string {
	if v := root.Annotations[extraEnvKey]; v != "" {
		return strings.Split(v, ",")
	}
	return nil
}

// NewViper returns a viper that reads nothing yet: the environment is bound per flag by Resolve,
// and a binary with a config file calls SetConfigFile and ReadInConfig before Execute.
func NewViper() *viper.Viper {
	v := viper.New()
	v.AllowEmptyEnv(true)
	return v
}

// Resolve sets every flag of cmd that was not given on the command line from v: its environment
// variables, then its config file key. The first value that does not parse is returned as an error
// naming where it came from. Call it before the command runs (Bind does, in PersistentPreRunE),
// and before cobra checks required flags, so a required flag may come from the environment.
func Resolve(cmd *cobra.Command, v *viper.Viper) error {
	if v == nil {
		v = NewViper()
	}
	var err error
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if err != nil || f.Changed {
			return
		}
		names := EnvNames(f)
		if bindErr := v.BindEnv(append([]string{f.Name}, names...)...); bindErr != nil {
			err = bindErr
			return
		}
		if !v.IsSet(f.Name) {
			return
		}
		var val string
		switch f.Value.Type() {
		case "stringSlice", "stringArray":
			val = strings.Join(v.GetStringSlice(f.Name), ",")
		default:
			val = v.GetString(f.Name)
		}
		if setErr := cmd.Flags().Set(f.Name, val); setErr != nil {
			err = fmt.Errorf("%s: %w", sourceOf(f, names), setErr)
		}
	})
	return err
}

// sourceOf names where a flag's value came from, for an error message.
func sourceOf(f *pflag.Flag, envNames []string) string {
	for _, n := range envNames {
		if _, ok := os.LookupEnv(n); ok {
			return n
		}
	}
	return "config key " + f.Name
}

// Bind makes root resolve the flags of whichever command runs from v, and gives every command the
// same error handling: Execute reports errors, so cobra neither prints them nor the usage. A
// command that declares its own PersistentPreRunE must call Resolve itself.
func Bind(root *cobra.Command, v *viper.Viper) {
	root.SilenceErrors, root.SilenceUsage = true, true
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { return Resolve(cmd, v) }
}

// DefaultTo makes root run the subcommand name when args name none (see DefaultSubcommand). A
// binary whose main job has a subcommand (koinon-api serve) keeps starting without one, so
// "koinon-api --addr :8080" and a pod that is configured by environment variables alone still serve.
func DefaultTo(root *cobra.Command, name string) {
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations[defaultKey] = name
}

// DefaultSubcommand returns args with root's default subcommand (DefaultTo) in front when args
// name no subcommand: none at all, or they start with a flag other than help and version.
func DefaultSubcommand(root *cobra.Command, args []string) []string {
	name := root.Annotations[defaultKey]
	if name == "" {
		return args
	}
	if len(args) == 0 {
		return []string{name}
	}
	if strings.HasPrefix(args[0], "-") && !slices.Contains([]string{"-h", "--help", "-v", "--version"}, args[0]) {
		return append([]string{name}, args...)
	}
	return args
}

// Styled reports whether help and errors may use colour and layout: only on a terminal, and not
// when NO_COLOR is set (ADR 0088). In pods and CI output stays plain text.
func Styled(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Execute runs root with args and returns the process exit code. On a terminal fang styles help,
// usage, errors and the version; elsewhere cobra prints plain text and a failure is one structured
// slog line, as the binaries have always logged it.
func Execute(ctx context.Context, root *cobra.Command, args []string) int {
	root.SetArgs(DefaultSubcommand(root, args))
	if root.Version == "" {
		root.Version = project.Version
	}
	if Styled(os.Stdout) && Styled(os.Stderr) {
		if err := fang.Execute(ctx, root, fang.WithVersion(root.Version)); err != nil {
			return 1
		}
		return 0
	}
	if err := root.ExecuteContext(ctx); err != nil {
		slog.Error(root.Name()+" failed", "err", err)
		return 1
	}
	return 0
}
