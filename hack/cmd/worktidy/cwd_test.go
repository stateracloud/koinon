// SPDX-License-Identifier: Apache-2.0

package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("parseLsofCwd", func() {
	const dir = "/tmp/kw/l1"

	It("reports another process inside the worktree and skips lsof and this process", func() {
		out := "p100\ncbash\nn/tmp/kw/l1/sub\n" +
			"p200\nclsof\nn/tmp/kw/l1\n" +
			"p300\ncgo\nn/tmp/kw/l1\n" +
			"p400\ncvim\nn/tmp/kw/other\n"
		Expect(parseLsofCwd(out, dir, dir, "300")).To(Equal([]string{"100"}))
	})

	It("reports nothing when only lsof itself is inside", func() {
		Expect(parseLsofCwd("p9\nclsof\nn/tmp/kw/l1\n", dir, dir, "1")).To(BeEmpty())
	})

	It("matches the resolved path", func() {
		Expect(parseLsofCwd("p5\nczsh\nn/private/tmp/kw/l1\n", dir, "/private/tmp/kw/l1", "1")).To(Equal([]string{"5"}))
	})
})
