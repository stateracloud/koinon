// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("koinon-api org-limits", func() {
	run := func(args ...string) error {
		root := NewRootCommand()
		root.SilenceUsage, root.SilenceErrors = true, true
		root.SetArgs(args)
		return root.ExecuteContext(context.Background())
	}

	It("needs the database and the org before it connects", func() {
		GinkgoT().Setenv("DATABASE_URL", "")
		GinkgoT().Setenv("KOINON_DATABASE_URL", "")
		Expect(run("org-limits", "show", "--org", "acme")).To(MatchError(ContainSubstring("--database-url is required")))
		Expect(run("org-limits", "show", "--database-url", "postgres://x")).To(MatchError(ContainSubstring("--org is required")))
	})

	It("set refuses a call that names no limit", func() {
		err := run("org-limits", "set", "--database-url", "postgres://koinon_test:not-a-real-token@127.0.0.1:1/koinon", "--org", "acme")
		Expect(err).To(MatchError(ContainSubstring("name a limit to change")))
	})
})
