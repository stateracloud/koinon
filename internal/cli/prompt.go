// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Interactive prompts (ADR 0088): a command may ask a person on a terminal for what the flags and
// arguments left out. Prompts are a convenience over the flag forms, never a replacement: without a
// terminal, or with --no-input (KOINON_NO_INPUT), a command behaves exactly as it does without
// this file, and a pod, a script or CI never waits on input.

const noInputFlag = "no-input"

// ErrCancelled is returned when the person declined a confirmation or aborted a prompt.
var ErrCancelled = errors.New("cancelled")

func addNoInputFlag(root *cobra.Command) {
	if root.PersistentFlags().Lookup(noInputFlag) != nil {
		return
	}
	root.PersistentFlags().Bool(noInputFlag, false,
		"never prompt: commands that can ask for missing input fail instead (also when there is no terminal)")
}

// Interactive reports whether cmd may prompt: stdin and stdout are both terminals and --no-input
// (KOINON_NO_INPUT) is off. Call it from the command's RunE, after the flags are resolved.
func Interactive(cmd *cobra.Command) bool {
	if f := cmd.Flags().Lookup(noInputFlag); f != nil && f.Value.String() == "true" {
		return false
	}
	return isTerminal(cmd.InOrStdin()) && isTerminal(cmd.OutOrStdout())
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Prompter asks questions on a terminal with huh. Build one with PrompterFor, or for a test with
// a Reader and Writer and Accessible set, which needs no pty.
type Prompter struct {
	In  io.Reader
	Out io.Writer
	// Accessible uses plain line prompts instead of the full-screen forms (TERM=dumb, screen readers).
	Accessible bool
	// Styled uses colour (see Styled); otherwise the plain base theme.
	Styled bool
}

// PrompterFor returns a Prompter for cmd, or nil when cmd is not Interactive: a nil Prompter is
// "do not ask".
func PrompterFor(cmd *cobra.Command) *Prompter {
	if !Interactive(cmd) {
		return nil
	}
	return &Prompter{
		In: cmd.InOrStdin(), Out: cmd.OutOrStdout(),
		Accessible: os.Getenv("TERM") == "dumb",
		Styled:     Styled(cmd.OutOrStdout()),
	}
}

// Choice is one entry of Select. A Choice with a non-empty Disabled is listed with that reason but
// cannot be picked.
type Choice struct {
	Label    string
	Value    string
	Disabled string
}

func (p *Prompter) form(f huh.Field) *huh.Form {
	theme := huh.ThemeFunc(huh.ThemeBase)
	if p.Styled {
		theme = huh.ThemeFunc(huh.ThemeCharm)
	}
	in := p.In
	if p.Accessible {
		// huh reads each accessible answer through a new buffered scanner, which would swallow the
		// answers to the prompts after it; one byte per read leaves them in the stream.
		in = oneByteReader{in}
	}
	return huh.NewForm(huh.NewGroup(f)).WithInput(in).WithOutput(p.Out).
		WithAccessible(p.Accessible).WithTheme(theme)
}

// note shows a description. The accessible prompts print only their title, so there it is a line
// of its own above the prompt.
func (p *Prompter) note(description string) string {
	if p.Accessible && description != "" {
		_, _ = fmt.Fprintln(p.Out, description)
		return ""
	}
	return description
}

func (p *Prompter) run(ctx context.Context, f huh.Field) error {
	if err := p.form(f).RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrCancelled
		}
		return err
	}
	return nil
}

// Confirm asks a yes/no question; the default is no.
func (p *Prompter) Confirm(ctx context.Context, title, description string) (bool, error) {
	var ok bool
	err := p.run(ctx, huh.NewConfirm().Title(title).Description(p.note(description)).Value(&ok))
	return ok, err
}

// Input asks for a line of text. validate, if set, rejects an answer (the person is asked again).
func (p *Prompter) Input(ctx context.Context, title, description string, validate func(string) error) (string, error) {
	var s string
	in := huh.NewInput().Title(title).Description(p.note(description)).Value(&s)
	if validate != nil {
		in = in.Validate(validate)
	}
	err := p.run(ctx, in)
	return s, err
}

// Select asks for one of the choices and returns its Value. Disabled choices are listed above the
// prompt with their reason. It fails when no choice can be picked.
func (p *Prompter) Select(ctx context.Context, title string, choices []Choice) (string, error) {
	var pickable []Choice
	for _, c := range choices {
		if c.Disabled != "" {
			_, _ = fmt.Fprintf(p.Out, "  %s (not available: %s)\n", c.Label, c.Disabled)
			continue
		}
		pickable = append(pickable, c)
	}
	if len(pickable) == 0 {
		return "", errors.New("nothing to choose from")
	}
	if p.Accessible {
		return p.selectLines(title, pickable)
	}
	opts := make([]huh.Option[string], len(pickable))
	for i, c := range pickable {
		opts[i] = huh.NewOption(c.Label, c.Value)
	}
	var v string
	err := p.run(ctx, huh.NewSelect[string]().Title(title).Options(opts...).Value(&v))
	return v, err
}

// selectLines is Select for accessible mode: a numbered list and a line to answer, reading
// byte by byte like the other accessible prompts. End of input cancels.
func (p *Prompter) selectLines(title string, pickable []Choice) (string, error) {
	_, _ = fmt.Fprintln(p.Out, title)
	for i, c := range pickable {
		_, _ = fmt.Fprintf(p.Out, "%d. %s\n", i+1, c.Label)
	}
	for {
		_, _ = fmt.Fprintf(p.Out, "Enter a number between 1 and %d: ", len(pickable))
		line, err := readLine(p.In)
		var n int
		if _, scanErr := fmt.Sscanf(line, "%d", &n); scanErr == nil && n >= 1 && n <= len(pickable) {
			return pickable[n-1].Value, nil
		}
		if err != nil {
			_, _ = fmt.Fprintln(p.Out)
			return "", ErrCancelled
		}
		_, _ = fmt.Fprintf(p.Out, "Invalid: must be a number between 1 and %d\n", len(pickable))
	}
}

// readLine reads up to a newline one byte at a time, so nothing after it is consumed.
func readLine(r io.Reader) (string, error) {
	var buf bytes.Buffer
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return buf.String(), nil
			}
			buf.WriteByte(b[0])
		}
		if err != nil {
			return buf.String(), err
		}
	}
}

type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}
