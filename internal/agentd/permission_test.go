// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/agentd"
)

var _ = DescribeTable("the workspace check of permission requests",
	func(path string, inside bool) {
		Expect(agentd.Within("/sandbox/work", path)).To(Equal(inside))
	},
	Entry("the directory", "/sandbox/work", true),
	Entry("a file in it", "/sandbox/work/a/b.txt", true),
	Entry("a relative path", "a/b.txt", true),
	Entry("a trailing slash", "/sandbox/work/", true),
	Entry("the home", "/sandbox/.koinon/AGENTS.md", false),
	Entry("a sibling with the same prefix", "/sandbox/workshop/x", false),
	Entry("dot-dot out of it", "/sandbox/work/../.koinon/AGENTS.md", false),
	Entry("a relative escape", "../x", false),
	Entry("the root", "/", false),
	Entry("system files", "/etc/passwd", false),
)
