// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/cli"
)

var _ = Describe("koinon-api keys on and off a terminal", func() {
	keys := func(args ...string) error {
		return cli.Check(NewRootCommand(), append([]string{"keys"}, args...), nil)
	}

	It("keeps today's errors when it cannot ask: no terminal, or --no-input", func() {
		for _, c := range []string{"activate", "retire"} {
			Expect(keys(c)).To(MatchError("accepts 1 arg(s), received 0"), c)
			Expect(keys(c, "--no-input")).To(MatchError("accepts 1 arg(s), received 0"), c)
			Expect(keys(c, "a", "b")).To(MatchError("accepts 1 arg(s), received 2"), c)
		}
		Expect(keys("rewrap", "x")).To(MatchError("unknown command \"x\" for \"koinon-api keys rewrap\""))
	})

	It("lets a missing ID through to the command when it can ask", func() {
		old := prompterFor
		defer func() { prompterFor = old }()
		prompterFor = func(*cobra.Command) *cli.Prompter { return &cli.Prompter{Accessible: true} }
		Expect(keys("activate", "--database-url=postgres://x")).To(Succeed())
		Expect(keys("activate")).To(MatchError(ContainSubstring("--database-url is required")), "activate records its install audit event")
		Expect(keys("activate", "a", "b")).To(MatchError("accepts 1 arg(s), received 2"))
		Expect(keys("retire", "--database-url=postgres://x")).To(Succeed())
		Expect(keys("retire")).To(MatchError(ContainSubstring("--database-url is required")))
	})
})
