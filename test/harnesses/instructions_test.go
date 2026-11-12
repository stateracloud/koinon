// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// newHarnessesandbox starts a sandbox with f's image, the session directory and, unless
// instructions is empty, the instruction file koinon-agentd writes.
func newHarnessesandbox(ctx context.Context, f definition, instructions string) *sandbox {
	GinkgoHelper()
	testenv.Require(testenv.Docker)
	ensureImage(f)
	sb := newSandbox(f.image(), "OPENAI_API_KEY="+fakeKey)
	DeferCleanup(func() { sb.remove(context.Background()) })
	sb.start(ctx)
	sb.run(ctx, "mkdir", "-p", workdir)
	if instructions != "" {
		sb.write(ctx, instructionsPath, instructions)
	}
	return sb
}

// startSession starts cmd in sb and brings a new session to where koinon-agentd sends the first
// prompt, as the contract does. The harness process outlives ctx; the caller closes it.
func startSession(ctx context.Context, f definition, sb *sandbox, cmd []string) (*harness, acp.SessionID) {
	GinkgoHelper()
	h := sb.harness(context.WithoutCancel(ctx), workdir, cmd)
	_, err := h.conn.Initialize(ctx)
	Expect(err).NotTo(HaveOccurred())
	session, err := h.conn.NewSession(ctx, workdir)
	Expect(err).NotTo(HaveOccurred())
	if f.readyCommand != "" {
		Eventually(h.client.availableCommands).WithTimeout(30 * time.Second).Should(ContainElement(f.readyCommand))
		time.Sleep(readySettle)
	}
	if f.selectModel != nil {
		Expect(h.conn.SetModel(ctx, session, f.selectModel(contractModel))).To(Succeed())
	}
	return h, session
}

// strayInstructions is what an agent would put in place of the instruction link.
const strayInstructions = "KOINON-STRAY-INSTRUCTIONS"

// describeInstructions registers the instruction specs for one harness: koinon-agentd's file
// reaches the system prompt through the harness's link or config, and without the file the entry script
// refuses to start.
func describeInstructions(f definition) bool {
	return Describe(f.name+" instructions", func() {
		BeforeEach(func() {
			if !f.instructionsInSystem {
				Skip(f.name + " does not read Koinon's instructions")
			}
		})

		It("puts the instruction file into the system prompt, through a link it puts back on start", func(ctx SpecContext) {
			gw := newGateway(contractScript(f))
			sb := newHarnessesandbox(ctx, f, "# Koinon\n\n"+instructionsMarker+": you are a contract test agent.\n")
			if strings.HasPrefix(f.instructionLink, "/sandbox/") {
				// What an agent could leave behind for the next start: its own instructions.
				sb.write(ctx, "/sandbox/stray.md", strayInstructions+"\n")
				sb.run(ctx, "sh", "-c", `mkdir -p "$(dirname "$1")" && rm -rf "$1" && ln -s /sandbox/stray.md "$1"`, "sh", f.instructionLink)
			}
			h, session := startSession(ctx, f, sb, f.command(gw.url("/v1"), contractModel, gw.url("/mcp")))
			DeferCleanup(h.close)
			if f.instructionLink != "" {
				Expect(sb.run(ctx, "readlink", f.instructionLink)).To(Equal(instructionsPath + "\n"))
			}

			// The fake model answers helloPrompt only when the marker is in the system prompt.
			_, err := h.conn.Prompt(ctx, session, helloPrompt)
			Expect(err).NotTo(HaveOccurred())
			updates, _ := h.client.take()
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(helloReply))
			Expect(gw.modelBodies()).To(HaveEach(Not(ContainSubstring(strayInstructions))))
		}, SpecTimeout(3*time.Minute))

		DescribeTable("refuses to start without the instruction file",
			func(ctx SpecContext, setup string) {
				sb := newHarnessesandbox(ctx, f, "")
				if setup != "" {
					sb.run(ctx, "sh", "-c", setup)
				}
				_, stderr, code := sb.try(ctx, f.command("http://gw.test/v1", contractModel, "http://gw.test/mcp")...)
				Expect(code).To(Equal(64))
				Expect(stderr).To(ContainSubstring("missing instructions: " + instructionsPath))
			},
			Entry("missing", SpecTimeout(2*time.Minute), ""),
			Entry("empty", SpecTimeout(2*time.Minute), "mkdir -p /sandbox/.koinon && : > "+instructionsPath),
			Entry("a symbolic link", SpecTimeout(2*time.Minute),
				"mkdir -p /sandbox/.koinon && echo x > /sandbox/x.md && ln -s /sandbox/x.md "+instructionsPath),
		)
	})
}

var _ = func() bool {
	for _, f := range harnesses {
		describeInstructions(f)
	}
	return true
}()
