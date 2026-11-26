// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	koinharness "github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// chatMessage is one message of a chat completions request, its content as raw JSON (a string or
// a list of parts), which is enough to search for a marker.
type chatMessage struct {
	Role    string
	Content string
}

func chatMessages(body string) []chatMessage {
	GinkgoHelper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	Expect(json.Unmarshal([]byte(body), &req)).To(Succeed())
	out := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		out = append(out, chatMessage{Role: m.Role, Content: string(m.Content)})
	}
	return out
}

// OpenCode 2.2.0 attaches an AGENTS.md from a directory above a file its read tool reads, as a
// message that starts with "Instructions from: <path>", and has no switch to stop it (issue #71,
// harnesses/opencode/README.md, "Repository guidance"). Koinon cannot turn it off, so its
// instruction file ranks that text below its own (agentd.InstructionPrecedence). This spec records
// the behaviour: a new OpenCode that changes it fails here, not in production.
var _ = Describe("koinon-opencode subdirectory AGENTS.md", func() {
	const (
		readMarker    = "KOINON-READ-NESTED"
		sessionMarker = "KOINON-SESSION-ROOT-AGENTS"
		repoMarker    = "KOINON-REPO-ROOT-AGENTS"
		subdirMarker  = "KOINON-SUBDIR-AGENTS"
		fileText      = "KOINON-READ-FILE-TEXT"
		repo          = workdir + "/repo"
		readPath      = repo + "/pkg/deep/file.txt"
	)
	script := fakemodel.Script{Model: contractModel, Turns: []fakemodel.Turn{
		{When: fakemodel.When{ToolResult: fakemodel.AnyTool, UserContains: readMarker}, Text: helloReply},
		{
			When:      fakemodel.When{UserContains: readMarker},
			ToolCalls: []fakemodel.ToolCall{{Name: "read", Arguments: map[string]any{"path": readPath}}},
		},
		{Text: helloReply},
	}}

	It("attaches the AGENTS.md files between the read file and the session directory after Koinon's system prompt", func(ctx SpecContext) {
		gw := newGateway(script)
		precedence := agentd.InstructionPrecedence(koinharness.OpenCode)
		sb := newOpencodeSandbox(ctx, "# Koinon\n\n"+precedence+instructionsMarker+"\n")
		sb.write(ctx, workdir+"/AGENTS.md", sessionMarker+"\n")
		sb.run(ctx, "git", "init", "-q", repo)
		sb.write(ctx, repo+"/AGENTS.md", repoMarker+"\n")
		sb.write(ctx, repo+"/pkg/AGENTS.md", subdirMarker+"\n")
		sb.write(ctx, readPath, fileText+"\n")
		h, session := opencodeHarness(ctx, sb, gw)

		_, err := h.conn.Prompt(ctx, session, readMarker+" read the file.")
		Expect(err).NotTo(HaveOccurred())
		h.client.take()

		for i, b := range gw.modelBodies() {
			GinkgoWriter.Printf("---- body %d\n%s\n", i, b)
		}
		body := lastBody(gw)
		Expect(body).To(ContainSubstring(fileText), "the model got the read tool's result")
	}, SpecTimeout(3*time.Minute))
})
