// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("koinon-api model-metadata", func() {
	run := func(args ...string) error {
		root := NewRootCommand()
		root.SilenceUsage, root.SilenceErrors = true, true
		root.SetArgs(args)
		return root.ExecuteContext(context.Background())
	}
	// Nothing listens on port 1: a command that got past its checks fails to connect.
	const unreachable = "postgres://koinon_test:not-a-real-token@127.0.0.1:1/koinon?connect_timeout=1"

	It("needs the database before it connects", func() {
		GinkgoT().Setenv("DATABASE_URL", "")
		GinkgoT().Setenv("KOINON_DATABASE_URL", "")
		for _, args := range [][]string{
			{"model-metadata", "proposals", "list"},
			{"model-metadata", "proposals", "accept", "0197f2a0-0000-7000-8000-000000000000"},
			{"model-metadata", "proposals", "dismiss", "0197f2a0-0000-7000-8000-000000000000"},
		} {
			Expect(run(args...)).To(MatchError(ContainSubstring("--database-url is required")), args[2])
		}
	})

	It("reads the database URL from KOINON_DATABASE_URL and from its alias DATABASE_URL", func() {
		GinkgoT().Setenv("DATABASE_URL", "")
		GinkgoT().Setenv("KOINON_DATABASE_URL", unreachable)
		err := run("model-metadata", "proposals", "list")
		Expect(err).To(HaveOccurred())
		Expect(err).NotTo(MatchError(ContainSubstring("--database-url is required")))

		// Set but empty, the KOINON_ name still wins over the alias, so unset it (Setenv restores it).
		GinkgoT().Setenv("KOINON_DATABASE_URL", "")
		Expect(os.Unsetenv("KOINON_DATABASE_URL")).To(Succeed())
		GinkgoT().Setenv("DATABASE_URL", unreachable)
		err = run("model-metadata", "proposals", "list")
		Expect(err).To(HaveOccurred())
		Expect(err).NotTo(MatchError(ContainSubstring("--database-url is required")))
	})

	It("takes exactly one proposal id", func() {
		Expect(run("model-metadata", "proposals", "accept", "--database-url", unreachable)).To(MatchError(ContainSubstring("accepts 1 arg")))
	})
})
