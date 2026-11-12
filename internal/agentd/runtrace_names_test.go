// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
)

// The shell tool has two names: OpenCode 2.2.5 (#60687) runs a call named bash as shell (ADR 0093 decision 5).
var _ = DescribeTable("traces a shell call under its own name",
	func(title string) {
		Expect(agentd.ToolName(title, "execute")).To(Equal(title))
	},
	Entry("bash", "bash"),
	Entry("shell", "shell"),
)

var _ = It("traces an unknown title under its ACP kind", func() {
	Expect(agentd.ToolName("/etc/passwd", "read")).To(Equal("read"))
})
