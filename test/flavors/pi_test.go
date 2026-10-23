// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// pi is the pi flavor's contract (flavors/pi/README.md).
var pi = flavor{
	name:        "pi",
	storeFlavor: store.AgentFlavorPi,
	dockerfile:  "flavors/pi",
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write", Arguments: map[string]any{"path": path, "content": content}}
	},
	// pi names MCP tools mcp__<server>__<tool>; "exposure": "direct" declares them to the model.
	mcpToolName: "mcp__koinon__" + mcpTool,
	// pi-acp advertises session/load, list and delete, not resume.
	resume: false,
	// pi-acp asks over ACP only for pi extension dialogs; built-in tools never ask.
	editAsks:       false,
	unknownSession: "Unknown sessionId",
	// pi appends APPEND_SYSTEM.md, linked to the file koinon-agentd writes, to its system prompt.
	instructionsInSystem: true,
	instructionLink:      "/sandbox/.pi/agent/APPEND_SYSTEM.md",
	// What pi would read from a repository: context files from the session directory upward
	// (dist/core/resource-loader.js, loadProjectContextFiles), and .pi/ in a trusted project.
	projectConfig: piRepoFiles,
}

// piRepoFiles are the repository files pi would load, by path relative to the session
// directory, each with projectMarker or a tool of its own.
var piRepoFiles = map[string]string{
	"AGENTS.md":                projectMarker + " from AGENTS.md\n",
	"../CLAUDE.md":             projectMarker + " from CLAUDE.md in the parent directory\n",
	".pi/APPEND_SYSTEM.md":     projectMarker + " from .pi/APPEND_SYSTEM.md\n",
	".pi/SYSTEM.md":            projectMarker + " from .pi/SYSTEM.md\n",
	".pi/extensions/repo.ts":   piExtension(repoTool),
	".pi/skills/repo/SKILL.md": "---\nname: koinon-repo\ndescription: " + projectMarker + " skill\n---\n" + projectMarker + "\n",
}

// piHomeMarker is the text what a run left in pi's agent directory would bring into the
// model's context; plantedTool and repoTool are the tools a planted extension in the home and
// one in the repository register.
const (
	piHomeMarker = "KOINON-HOME-PLANT-MARKER"
	plantedTool  = "koinon_planted"
	repoTool     = "koinon_repo"
	piHomeReply  = "Something left in the agent directory is in the system prompt."
)

// piExtension is a pi extension that registers tool, as an agent could write one.
func piExtension(tool string) string {
	return `export default function (pi) {
  pi.registerTool({
    name: "` + tool + `",
    label: "` + tool + `",
    description: "` + piHomeMarker + ` tool",
    parameters: { type: "object", properties: {} },
    async execute() { return { content: [{ type: "text", text: "planted" }], details: {} }; },
  });
}
`
}

// plantPiHome writes what a run could leave in its home for the next one: an extension, a
// skill and a prompt template in pi's agent directory and ~/.agents, a SYSTEM.md and AGENTS.md
// there, and a trust decision for the session directory, which would load the repository's .pi/.
func plantPiHome(ctx context.Context, sb *sandbox) {
	GinkgoHelper()
	const dir = "/sandbox/.pi/agent"
	for path, content := range map[string]string{
		dir + "/extensions/planted.ts":                   piExtension(plantedTool),
		dir + "/skills/koinon-planted/SKILL.md":          "---\nname: koinon-planted\ndescription: " + piHomeMarker + " skill\n---\n" + piHomeMarker + "\n",
		"/sandbox/.agents/skills/koinon-agents/SKILL.md": "---\nname: koinon-agents\ndescription: " + piHomeMarker + " skill\n---\n" + piHomeMarker + "\n",
		dir + "/prompts/koinon-planted.md":               "---\ndescription: " + piHomeMarker + " prompt\n---\n" + piHomeMarker + "\n",
		dir + "/SYSTEM.md":                               piHomeMarker + " from SYSTEM.md\n",
		dir + "/AGENTS.md":                               piHomeMarker + " from the agent directory's AGENTS.md\n",
		dir + "/trust.json":                              `{"` + workdir + `": true}` + "\n",
	} {
		sb.write(ctx, path, content)
	}
}

// pi's tools and instructions come from the image and Koinon's instruction file: what a
// repository ships and what an earlier run left in the agent's home have no effect on the next
// start (flavors/pi/README.md, Repository and home).
var _ = Describe("koinon-pi, the repository and the home", Ordered, func() {
	var (
		gw *gateway
		sb *sandbox
		h  *harness
	)

	script := func() fakemodel.Script {
		s := contractScript(pi)
		s.Turns = append([]fakemodel.Turn{
			{When: fakemodel.When{SystemContains: projectMarker}, Text: projectReply},
			{When: fakemodel.When{SystemContains: piHomeMarker}, Text: piHomeReply},
		}, s.Turns...)
		return s
	}

	// hello starts cmd on the sandbox's home and answers helloPrompt.
	hello := func(ctx context.Context, cmd []string) string {
		if h != nil {
			h.close()
		}
		var session acp.SessionID
		h, session = startSession(ctx, pi, sb, cmd)
		_, err := h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		updates, _ := h.client.take()
		return acp.Text(updates, "agent_message_chunk")
	}

	BeforeAll(func(ctx SpecContext) {
		gw = newGateway(script())
		sb = newFlavorSandbox(ctx, pi, "# Koinon\n\n"+instructionsMarker+": you are a contract test agent.\n")
		for name, content := range piRepoFiles {
			sb.write(ctx, workdir+"/"+name, content)
		}
	}, NodeTimeout(3*time.Minute))
	AfterAll(func() {
		if h != nil {
			h.close()
		}
	})

	It("keeps the repository's and the home's files out, and APPEND_SYSTEM.md in", func(ctx SpecContext) {
		plantPiHome(ctx, sb)
		// The fake model answers helloPrompt only when Koinon's instructions are in the system
		// prompt and neither marker is.
		Expect(hello(ctx, pi.command(gw.url("/v1"), contractModel, gw.url("/mcp")))).To(ContainSubstring(helloReply))
		Expect(gw.modelBodies()).To(HaveEach(And(Not(ContainSubstring(projectMarker)), Not(ContainSubstring(piHomeMarker)))))
		tools := strings.Join(offeredTools(gw), "\n")
		Expect(tools).To(ContainSubstring(pi.mcpToolName))
		Expect(tools).NotTo(ContainSubstring(plantedTool))
		Expect(tools).NotTo(ContainSubstring(repoTool))
		Expect(h.client.availableCommands()).NotTo(ContainElement(ContainSubstring("koinon-planted")))
		_, _, code := sb.try(ctx, "test", "-e", "/sandbox/.pi/agent/SYSTEM.md")
		Expect(code).NotTo(BeZero(), "the planted SYSTEM.md is gone")
	}, SpecTimeout(3*time.Minute))

	// The control: the same files with koinon-pi's guards taken out do reach the model, so the
	// spec above would see it if the guards did not hold.
	It("would load them without koinon-pi's guards", func(ctx SpecContext) {
		plantPiHome(ctx, sb)
		const unguard = `sed -e 's|^export PI_ACP_PI_COMMAND=.*|export PI_ACP_PI_COMMAND=/opt/pi/node_modules/.bin/pi|' ` +
			`-e '/^find "\$PI_CODING_AGENT_DIR"/d' /usr/local/bin/koinon-pi > /tmp/unguarded && ` +
			`! grep -q -e '^export PI_ACP_PI_COMMAND=/usr/local/' -e '^find ' /tmp/unguarded && exec sh /tmp/unguarded "$@"`
		reply := hello(ctx, append([]string{"sh", "-c", unguard, "sh"},
			pi.command(gw.url("/v1"), contractModel, gw.url("/mcp"))[1:]...))
		Expect(reply).NotTo(ContainSubstring(helloReply))
		last := gw.modelBodies()[len(gw.modelBodies())-1]
		Expect(last).To(ContainSubstring(projectMarker+" from AGENTS.md"), "the repository's AGENTS.md")
		Expect(last).To(ContainSubstring(piHomeMarker+" from the agent directory's AGENTS.md"), "the home's AGENTS.md")
		Expect(last).To(ContainSubstring(piHomeMarker+" skill"), "the home's skills")
		// trust.json in the home makes pi trust the repository, whose .pi/SYSTEM.md then wins
		// over the home's and whose .pi/APPEND_SYSTEM.md replaces Koinon's instructions.
		Expect(last).To(ContainSubstring(projectMarker+" from .pi/SYSTEM.md"), "the repository's .pi/SYSTEM.md")
		Expect(last).NotTo(ContainSubstring(instructionsMarker), "Koinon's APPEND_SYSTEM.md is replaced")
		tools := strings.Join(offeredTools(gw), "\n")
		Expect(tools).To(ContainSubstring(plantedTool), "the home's extension")
		Expect(tools).To(ContainSubstring(repoTool), "the repository's extension, trusted by the home's trust.json")
		Eventually(h.client.availableCommands).WithTimeout(10*time.Second).
			Should(ContainElement(ContainSubstring("koinon-planted")), "the home's prompt template")
	}, SpecTimeout(3*time.Minute))
})
