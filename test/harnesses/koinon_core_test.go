// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// koinonCoreReadyCommand is harnesses/opencode/plugins/koinon-core/index.ts's READY_COMMAND (ADR
// 0093 decision 3.1). Kept here as a literal, not imported, because this plugin compiles for
// OpenCode/Bun, not for this Go module; test/harnesses proves it at the image, not the source, level
// (harnesses/opencode/README.md, "koinon.core").
const koinonCoreReadyCommand = "koinon-core-ready-v1"

// koinon.core (ADR 0093) is Koinon's one owned OpenCode plugin. This slice (Delivery steps 1-3)
// only proves it loads and reports ready; the modules it will host (ADR 0093 decision 3.2-3.5) and
// agentd's own wait on koinonCoreReadyCommand (decision 3.1's other half) are out of this PR.
var _ = Describe("koinon.core OpenCode plugin", Ordered, func() {
	var sb *sandbox

	BeforeAll(func(ctx SpecContext) {
		sb = newOpencodeSandbox(ctx, instructionsMarker+"\n")
	})

	It("loads the plugin from the read-only config, from a directory nothing else loads", func(ctx SpecContext) {
		Expect(sb.run(ctx, "stat", "-c", "%u %A", "/usr/local/lib/koinon/opencode/plugins/koinon-core/index.ts")).
			To(Equal("0 -rw-r--r--\n"))
		Expect(sb.run(ctx, "cat", "/etc/koinon/opencode/opencode.jsonc")).
			To(ContainSubstring(`"package": "/usr/local/lib/koinon/opencode/plugins/koinon-core"`))
	})

	It("is listed before koinon.retry, so it wins a duplicate plugin ID (README.md, \"koinon.core\")", func(ctx SpecContext) {
		config := sb.run(ctx, "cat", "/etc/koinon/opencode/opencode.jsonc")
		core := strings.Index(config, `"package": "/usr/local/lib/koinon/opencode/plugins/koinon-core"`)
		retry := strings.Index(config, `"package": "/usr/local/lib/koinon/opencode/plugins/koinon-retry"`)
		Expect(core).To(BeNumerically(">=", 0), "koinon-core is not in the config")
		Expect(retry).To(BeNumerically(">=", 0), "koinon-retry is not in the config")
		Expect(core).To(BeNumerically("<", retry), "koinon-core must be listed before koinon-retry")
	})

	It("reports ready once it loads: the ready command reaches available_commands_update", func(ctx SpecContext) {
		script := contractScript(opencode)
		gw := newGateway(script)
		h, _ := opencodeHarness(ctx, sb, gw)
		Eventually(h.client.availableCommands).
			WithTimeout(30*time.Second).
			Should(ContainElement(koinonCoreReadyCommand), "koinon.core never reported ready")
	})
})
