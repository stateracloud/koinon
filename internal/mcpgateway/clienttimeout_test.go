// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"os"
	"regexp"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// v0.1 sends no progress notifications (ADR 0097 section 14). A client with a shorter request timeout than
// the call timeout gives up first, so the harness config must keep its timeout above CallTimeout.
var _ = Describe("the OpenCode MCP request timeout", func() {
	It("is at least the gateway call timeout", func() {
		raw, err := os.ReadFile("../../harnesses/opencode/etc/koinon/opencode/opencode.jsonc")
		Expect(err).NotTo(HaveOccurred())
		m := regexp.MustCompile(`(?s)"koinon":\s*\{.*?"timeout":\s*(\d+)`).FindSubmatch(raw)
		Expect(m).NotTo(BeNil(), "the koinon MCP server has no timeout in opencode.jsonc")
		ms, err := strconv.Atoi(string(m[1]))
		Expect(err).NotTo(HaveOccurred())
		Expect(time.Duration(ms) * time.Millisecond).To(BeNumerically(">=", mcpgateway.DefaultCallTimeout))
	})
})
