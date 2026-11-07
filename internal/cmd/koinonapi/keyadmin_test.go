// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
)

var _ = Describe("the key-admin command", func() {
	It("parses --activate-if-no-active next to --add-key", func() {
		var got keyAdminOptions
		cmd := newKeyAdminCommand()
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			var err error
			got.addID, err = cmd.Flags().GetString("add-key")
			Expect(err).NotTo(HaveOccurred())
			got.activateIfNoActive, err = cmd.Flags().GetBool("activate-if-no-active")
			return err
		}
		cmd.SetArgs([]string{
			"--kek-secret=koinon-kek", "--namespace=koinon", "--database-url=postgres://example.invalid/koinon",
			"--add-key=kek-20261106-a1b2", "--activate-if-no-active",
		})
		Expect(cmd.Execute()).To(Succeed())
		Expect(got.addID).To(Equal("kek-20261106-a1b2"))
		Expect(got.activateIfNoActive).To(BeTrue())
	})

	It("leaves recovery off unless asked", func() {
		cmd := newKeyAdminCommand()
		f := cmd.Flags().Lookup("activate-if-no-active")
		Expect(f).NotTo(BeNil())
		Expect(f.DefValue).To(Equal("false"))
	})
})
