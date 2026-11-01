// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/cli"
)

var _ = Describe("prompts", func() {
	var out *bytes.Buffer
	ask := func(input string) *cli.Prompter {
		out = &bytes.Buffer{}
		return &cli.Prompter{In: strings.NewReader(input), Out: out, Accessible: true}
	}

	Describe("Interactive", func() {
		run := func(env map[string]string, args ...string) (interactive bool) {
			root := &cobra.Command{Use: "t"}
			cli.Bind(root, nil)
			root.AddCommand(&cobra.Command{Use: "c", RunE: func(cmd *cobra.Command, _ []string) error {
				interactive = cli.Interactive(cmd)
				return nil
			}})
			Expect(cli.Check(root, args, env)).To(Succeed())
			return interactive
		}

		It("is off without a terminal on stdin and stdout, as in a test, a pod or CI", func(ctx SpecContext) {
			Expect(run(nil, "c")).To(BeFalse())
		})

		It("has a persistent --no-input on the root, also set by KOINON_NO_INPUT", func(ctx SpecContext) {
			Expect(run(nil, "--no-input", "c")).To(BeFalse())
			Expect(run(nil, "c", "--no-input")).To(BeFalse())
			Expect(run(map[string]string{"KOINON_NO_INPUT": "true"}, "c")).To(BeFalse())
		})
	})

	It("confirms: yes answers true, anything else and end of input answer false", func(ctx SpecContext) {
		ok, err := ask("y\n").Confirm(ctx, "Activate kek-1?", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(out.String()).To(ContainSubstring("Activate kek-1?"))
		ok, _ = ask("n\n").Confirm(ctx, "Sure?", "")
		Expect(ok).To(BeFalse())
		ok, _ = ask("").Confirm(ctx, "Sure?", "")
		Expect(ok).To(BeFalse())
	})

	It("answers several prompts from one stream", func(ctx SpecContext) {
		p := ask("y\n2\nkek-1\n")
		ok, _ := p.Confirm(ctx, "Sure?", "")
		Expect(ok).To(BeTrue())
		v, err := p.Select(ctx, "Which?", []cli.Choice{{Label: "one", Value: "1"}, {Label: "two", Value: "2"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(Equal("2"))
		s, err := p.Input(ctx, "Type it", "", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(s).To(Equal("kek-1"))
	})

	It("selects by number, asks again for a bad one and lists disabled choices with their reason", func(ctx SpecContext) {
		v, err := ask("7\nx\n1\n").Select(ctx, "Which?", []cli.Choice{
			{Label: "kek-a", Value: "a"},
			{Label: "kek-b", Value: "b", Disabled: "3 value(s) still use it"},
			{Label: "kek-c", Value: "c"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(Equal("a"))
		Expect(out.String()).To(ContainSubstring("kek-b (not available: 3 value(s) still use it)"))
		Expect(out.String()).To(ContainSubstring("2. kek-c"), "disabled choices are not numbered")
		Expect(out.String()).To(ContainSubstring("Invalid"))
	})

	It("cancels a selection at end of input and fails when nothing can be picked", func(ctx SpecContext) {
		_, err := ask("").Select(ctx, "Which?", []cli.Choice{{Label: "a", Value: "a"}})
		Expect(err).To(MatchError(cli.ErrCancelled))
		_, err = ask("1\n").Select(ctx, "Which?", []cli.Choice{{Label: "a", Value: "a", Disabled: "no"}})
		Expect(err).To(HaveOccurred())
	})

	It("asks for text again until it validates", func(ctx SpecContext) {
		s, err := ask("nope\nkek-1\n").Input(ctx, "Type kek-1", "", func(s string) error {
			if s != "kek-1" {
				return errors.New("not kek-1")
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(s).To(Equal("kek-1"))
	})
})

var _ = Describe("accessible prompts", func() {
	It("print the description, which huh leaves out of its line prompts", func(ctx SpecContext) {
		out := &bytes.Buffer{}
		p := &cli.Prompter{In: strings.NewReader("y\n"), Out: out, Accessible: true}
		_, err := p.Confirm(ctx, "Retire kek-1?", "Keep a copy in escrow.")
		Expect(err).NotTo(HaveOccurred())
		Expect(out.String()).To(ContainSubstring("Keep a copy in escrow.\n"))
	})
})
