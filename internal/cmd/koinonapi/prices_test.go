// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("koinon-api prices database URL", func() {
	It("reads DATABASE_URL, the only variable the chart sets", func() {
		GinkgoT().Setenv("DATABASE_URL", "postgres://koinon_test:not-a-real-token@127.0.0.1:1/koinon?connect_timeout=1")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		root := NewRootCommand()
		root.SilenceUsage, root.SilenceErrors = true, true
		root.SetArgs([]string{"prices", "proposals", "list"})
		err := root.ExecuteContext(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("--database-url is required"))
	})

	It("names the variables it reads when none is set", func() {
		GinkgoT().Setenv("DATABASE_URL", "")
		GinkgoT().Setenv("KOINON_DATABASE_URL", "")
		root := NewRootCommand()
		root.SilenceUsage, root.SilenceErrors = true, true
		root.SetArgs([]string{"prices", "proposals", "list"})
		err := root.ExecuteContext(context.Background())
		Expect(err).To(MatchError(And(ContainSubstring("KOINON_DATABASE_URL"), ContainSubstring("DATABASE_URL"))))
	})
})
