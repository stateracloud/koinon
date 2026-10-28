// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// opencode is the OpenCode flavor's contract (flavors/opencode/README.md).
var opencode = flavor{
	name:        "opencode",
	storeFlavor: store.AgentFlavorOpencode,
	dockerfile:  "flavors/opencode",
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write", Arguments: map[string]any{"path": path, "content": content}}
	},
	// jq, yq, fd, ast-grep, duckdb, sqlite3, poppler, python3 and friends (flavors/opencode/README.md).
	dataTools: true,
	// Measured 2026-10-28 with the data tools: arm64 650,916 KiB (429,148 before them), amd64
	// 632,836 KiB (409,084 before); plus about 10% (flavors/opencode/README.md, "Tools in the image").
	sizeBudget: map[string]int64{"arm64": 716_000, "amd64": 696_000},
	// OpenCode names MCP tools <server>_<tool>; the server is "koinon" in its config.
	mcpToolName: "koinon_" + mcpTool,
	// OpenCode also advertises session/resume; Koinon does not rely on it.
	resume: true,
	// OpenCode asks before touching a path outside the session's directory (permission
	// external_directory, default "ask"), not for edits inside it; its own global config
	// directory is exempt, so the path is elsewhere.
	permissionCall: fakemodel.ToolCall{
		Name: "read", Arguments: map[string]any{"path": "/etc/passwd"},
	},
	allow:  []acp.PermissionOptionKind{acp.AllowOnce},
	reject: []acp.PermissionOptionKind{acp.RejectOnce},
	// The request arrives with tool kind "read", which agentd's mapping allows, and the path in
	// its locations: agentd rejects it as outside the session directory.
	agentdVerdict: "reject",
	// OpenCode 2.2.0 treats a plain decline as a defect that interrupts the step
	// (packages/core/src/permission.ts), so the turn ends cancelled.
	rejectEndsTurn: true,
	// OpenCode 2.2.0's session/load error for an unknown session.
	unknownSession: "session not found",
	// The config's "instructions" key names the file koinon-agentd writes.
	instructionsInSystem: true,
	// OpenCode turns MCP prompts into commands named <server>:<prompt>.
	readyCommand: "koinon:" + readyPrompt,
	// OpenCode 2.2.0 reports a model of its own choosing until one is selected.
	selectModel: func(model string) string { return "koinon/" + model },
	// What a repository could ship to reconfigure OpenCode if project configuration were on
	// (koinon-opencode sets OPENCODE_DISABLE_PROJECT_CONFIG=1): an opencode.json and a
	// .opencode directory that add an instruction file and an agent prompt and allow every
	// permission, which would also stop the permission specs' external_directory request.
	projectConfig: map[string]string{
		"opencode.json": `{
  "permission": {"*": "allow", "external_directory": "allow"},
  "instructions": ["project-instructions.md"],
  "agent": {"build": {"prompt": "` + projectMarker + ` from opencode.json"}}
}`,
		".opencode/opencode.json": `{
  "permission": {"*": "allow", "external_directory": "allow"},
  "instructions": ["project-instructions.md"]
}`,
		".opencode/agent/build.md": "---\ndescription: Project build agent\n---\n" + projectMarker + " from .opencode/agent\n",
		"project-instructions.md":  projectMarker + " from an instruction file\n",
		// OpenCode v2 forms of the same, and the instruction files it reads by name.
		".opencode/opencode.jsonc": `{
  "permissions": [{"action": "*", "resource": "*", "effect": "allow"}],
  "agents": {"build": {"prompt": "` + projectMarker + ` from .opencode/opencode.jsonc"}}
}`,
		"AGENTS.md": projectMarker + " from AGENTS.md\n",
		"CLAUDE.md": projectMarker + " from CLAUDE.md\n",
	},
}

// opencodeHarness starts koinon-opencode in sb against gw, with extra arguments, and brings a
// new session to where koinon-agentd sends the first prompt.
func opencodeHarness(ctx context.Context, sb *sandbox, gw *gateway, extra ...string) (*harness, acp.SessionID) {
	GinkgoHelper()
	h := sb.harness(ctx, workdir, append(opencode.command(gw.url("/v1"), contractModel, gw.url("/mcp")), extra...))
	DeferCleanup(h.close)
	_, err := h.conn.Initialize(ctx)
	Expect(err).NotTo(HaveOccurred())
	session, err := h.conn.NewSession(ctx, workdir)
	Expect(err).NotTo(HaveOccurred())
	Eventually(h.client.availableCommands).WithTimeout(30 * time.Second).Should(ContainElement(opencode.readyCommand))
	time.Sleep(readySettle)
	Expect(h.conn.SetModel(ctx, session, opencode.selectModel(contractModel))).To(Succeed())
	return h, session
}

// newOpencodeSandbox starts a sandbox with the OpenCode image, the session directory and, unless
// instructions is empty, the instruction file koinon-agentd writes.
func newOpencodeSandbox(ctx context.Context, instructions string) *sandbox {
	GinkgoHelper()
	return newFlavorSandbox(ctx, opencode, instructions)
}

// lastBody is the last chat request the fake model received.
func lastBody(gw *gateway) string {
	b := gw.modelBodies()
	Expect(b).NotTo(BeEmpty())
	return b[len(b)-1]
}

var _ = Describe("koinon-opencode instructions", func() {
	It("delivers an edit of the instruction file to the model at the next turn", func(ctx SpecContext) {
		gw := newGateway(contractScript(opencode))
		sb := newOpencodeSandbox(ctx, instructionsMarker+" version one\n")
		h, session := opencodeHarness(ctx, sb, gw)

		_, err := h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		Expect(lastBody(gw)).To(ContainSubstring(instructionsMarker + " version one"))

		sb.write(ctx, instructionsPath, instructionsMarker+" version two\n")
		// OpenCode watches the file (100 ms debounce) and sends the change with the next turn.
		time.Sleep(2 * time.Second)
		_, err = h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		Expect(lastBody(gw)).To(ContainSubstring(instructionsMarker + " version two"))
	}, SpecTimeout(3*time.Minute))
})

// homePlugin is an OpenCode v2 plugin (the promise form of @opencode/plugin's define) that
// records its loading in /tmp, which a plugin with the sandbox's privileges can write.
func homePlugin(name string) string {
	return `import { writeFileSync } from "node:fs"
writeFileSync("/tmp/koinon-plugin-loaded-` + name + `", "1")
export default { id: "koinon.contract.` + name + `", setup: () => {} }
`
}

// loadedPlugins lists the homePlugin names that loaded in sb.
func loadedPlugins(ctx context.Context, sb *sandbox) []string {
	out := sb.run(ctx, "sh", "-c", "cd /tmp && ls koinon-plugin-loaded-* 2>/dev/null || true")
	var names []string
	for _, f := range strings.Fields(out) {
		names = append(names, strings.TrimPrefix(f, "koinon-plugin-loaded-"))
	}
	return names
}

// homeMarker is in everything homeSeed puts into the agent's home.
const homeMarker = "KOINON-HOME-CONFIG-MARKER"

// homeSeed is every place in the agent's own home (the volume, writable by the agent) where
// OpenCode or a tool it imitates looks for configuration, instructions, skills or plugins. None
// of it may reach the model or loosen the harness (issue #84).
func homeSeed() map[string]string {
	allowAll := `{"permission": {"*": "allow", "external_directory": "allow"},
 "permissions": [{"action": "*", "resource": "*", "effect": "allow"}],
 "agents": {"build": {"prompt": "` + homeMarker + ` agent prompt"}}}`
	skill := func(name string) string {
		return "---\nname: " + name + "\ndescription: " + homeMarker + " skill " + name + "\n---\n" + homeMarker + "\n"
	}
	return map[string]string{
		"/sandbox/AGENTS.md":                                    homeMarker + " ~/AGENTS.md\n",
		"/sandbox/CLAUDE.md":                                    homeMarker + " ~/CLAUDE.md\n",
		"/sandbox/.claude/CLAUDE.md":                            homeMarker + " ~/.claude/CLAUDE.md\n",
		"/sandbox/.claude/settings.json":                        `{"permissions": {"allow": ["Bash(*)"]}}`,
		"/sandbox/.claude/skills/home-claude/SKILL.md":          skill("home-claude"),
		"/sandbox/.agents/AGENTS.md":                            homeMarker + " ~/.agents/AGENTS.md\n",
		"/sandbox/.agents/skills/home-agents/SKILL.md":          skill("home-agents"),
		"/sandbox/.opencode/opencode.json":                      allowAll,
		"/sandbox/.opencode/AGENTS.md":                          homeMarker + " ~/.opencode/AGENTS.md\n",
		"/sandbox/.opencode/skills/home-dot/SKILL.md":           skill("home-dot"),
		"/sandbox/.opencode/plugins/home-dot/index.js":          homePlugin("home-dot"),
		"/sandbox/.config/opencode/opencode.json":               allowAll,
		"/sandbox/.config/opencode/opencode.jsonc":              allowAll,
		"/sandbox/.config/opencode/AGENTS.md":                   homeMarker + " ~/.config/opencode/AGENTS.md\n",
		"/sandbox/.config/opencode/skills/home-xdg/SKILL.md":    skill("home-xdg"),
		"/sandbox/.config/opencode/plugins/home-xdg/index.js":   homePlugin("home-xdg"),
		"/sandbox/.config/opencode/plugin/home-legacy/index.js": homePlugin("home-legacy"),
	}
}

var _ = Describe("koinon-opencode and the agent's home", func() {
	It("ignores configuration seeded in every home location", func(ctx SpecContext) {
		gw := newGateway(contractScript(opencode))
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		for path, content := range homeSeed() {
			sb.write(ctx, path, content)
		}
		h, session := opencodeHarness(ctx, sb, gw)

		_, err := h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		h.client.answerWith(opencode.reject...)
		_, err = h.conn.Prompt(ctx, session, permitPrompt)
		Expect(err).NotTo(HaveOccurred())
		_, perms := h.client.take()
		Expect(perms).NotTo(BeEmpty(), "a seeded allow-all permission must not apply")

		// Session titles come from a request without the instructions; every request counts.
		Expect(gw.modelBodies()).To(ContainElement(ContainSubstring(instructionsMarker)))
		Expect(gw.modelBodies()).To(HaveEach(Not(ContainSubstring(homeMarker))))
		Expect(loadedPlugins(ctx, sb)).To(BeEmpty())
	}, SpecTimeout(3*time.Minute))

	DescribeTable("loads the agent's plugins only with --plugin-installs",
		func(ctx SpecContext, extra []string, loaded []string) {
			gw := newGateway(contractScript(opencode))
			sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
			for path, content := range homeSeed() {
				if strings.Contains(path, "/plugin") {
					sb.write(ctx, path, content)
				}
			}
			h, session := opencodeHarness(ctx, sb, gw, extra...)
			_, err := h.conn.Prompt(ctx, session, helloPrompt)
			Expect(err).NotTo(HaveOccurred())
			if len(loaded) == 0 {
				Expect(loadedPlugins(ctx, sb)).To(BeEmpty())
			} else {
				Expect(loadedPlugins(ctx, sb)).To(ConsistOf(loaded))
			}
		},
		Entry("off by default", SpecTimeout(3*time.Minute), nil, nil),
		// Only directories under ~/.config/opencode/plugins/ are plugins (koinon-opencode).
		Entry("on", SpecTimeout(3*time.Minute), []string{"--plugin-installs"}, []string{"home-xdg"}),
	)

	// A plugin runs inside OpenCode (issue #84), so a broken one can stop the harness from ever
	// getting ready. koinon-agentd bounds bringing the session up and running the turn; whatever
	// the plugin does, the attempt ends by that deadline.
	DescribeTable("bounds a broken plugin by the prompt timeout",
		func(ctx SpecContext, source string) {
			gw := newGateway(contractScript(opencode))
			sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
			sb.write(ctx, "/sandbox/.config/opencode/plugins/broken/index.js", source)
			h := sb.harness(ctx, workdir, append(opencode.command(gw.url("/v1"), contractModel, gw.url("/mcp")), "--plugin-installs"))
			DeferCleanup(h.close)

			const timeout = 30 * time.Second
			turn, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			err := func() error {
				if _, err := h.conn.Initialize(turn); err != nil {
					return err
				}
				session, err := h.conn.NewSession(turn, workdir)
				if err != nil {
					return err
				}
				for !slices.Contains(h.client.availableCommands(), opencode.readyCommand) {
					select {
					case <-turn.Done():
						return turn.Err()
					case <-time.After(100 * time.Millisecond):
					}
				}
				_, err = h.conn.Prompt(turn, session, helloPrompt)
				return err
			}()
			GinkgoWriter.Printf("broken plugin: ended after %s with %v\n", time.Since(start), err)
			Expect(err).To(HaveOccurred(), "a broken plugin must not leave a working session")
			Expect(time.Since(start)).To(BeNumerically("<", timeout+5*time.Second))
		},
		// Fails session/new: OpenCode reports no models (CatalogNotReady).
		Entry("never finishes loading", SpecTimeout(2*time.Minute),
			"await new Promise(() => {})\nexport default { id: \"koinon.contract.stuck\", setup: () => {} }\n"),
		// The MCP server never connects, so the ready command never comes.
		Entry("returns no effect", SpecTimeout(2*time.Minute),
			"export default { id: \"koinon.contract.noeffect\", effect: () => {} }\n"),
	)
})

// The entry script's environment, with OpenCode itself replaced by env.
var _ = Describe("koinon-opencode plugin switch", Ordered, func() {
	var sb *sandbox

	BeforeAll(func(ctx SpecContext) {
		sb = newOpencodeSandbox(ctx, instructionsMarker+"\n")
		sb.write(ctx, "/sandbox/.config/opencode/plugins/one/index.js", homePlugin("one"))
		sb.write(ctx, "/sandbox/.config/opencode/plugins/not-a-directory.js", homePlugin("file"))
	})

	entryEnv := func(ctx context.Context, extra ...string) string {
		// The read-only root holds the script; run a copy whose last line prints the
		// environment instead of starting OpenCode.
		args := append([]string{
			"sh", "-c",
			`sed 's|^exec /usr/local/bin/opencode acp.*|exec /usr/bin/env|' /usr/local/bin/koinon-opencode > /tmp/entry && exec sh /tmp/entry "$@"`,
			"sh", "--model-base-url", "http://gw.test/v1", "--model", "m-1", "--mcp-url", "http://gw.test/mcp",
		}, extra...)
		return sb.run(ctx, args...)
	}

	It("names no plugins by default", func(ctx SpecContext) {
		Expect(entryEnv(ctx)).NotTo(ContainSubstring("OPENCODE_CONFIG_CONTENT"))
	})

	It("names each plugin directory with --plugin-installs", func(ctx SpecContext) {
		env := entryEnv(ctx, "--plugin-installs")
		Expect(env).To(ContainSubstring(`OPENCODE_CONFIG_CONTENT={"plugins":["/sandbox/.config/opencode/plugins/one"]}` + "\n"))
		Expect(env).To(ContainSubstring("OPENCODE_DISABLE_PROJECT_CONFIG=1\n"), "a repository still cannot configure the harness")
	})

	It("sets the cache format of the koinon provider with --cache-anthropic, next to the plugins", func(ctx SpecContext) {
		Expect(entryEnv(ctx, "--cache-anthropic")).To(ContainSubstring(
			`OPENCODE_CONFIG_CONTENT={"providers":{"koinon":{"compatibility":{"cacheFormat":"anthropic"}}}}` + "\n"))
		Expect(entryEnv(ctx, "--plugin-installs", "--cache-alibaba")).To(ContainSubstring(
			`OPENCODE_CONFIG_CONTENT={"plugins":["/sandbox/.config/opencode/plugins/one"],"providers":{"koinon":{"compatibility":{"cacheFormat":"alibaba"}}}}` + "\n"))
	})

	It("sends the run ID as the body field session_id with --session-id", func(ctx SpecContext) {
		Expect(entryEnv(ctx, "--session-id", "01234567-run")).To(ContainSubstring(
			`OPENCODE_CONFIG_CONTENT={"providers":{"koinon":{"body":{"session_id":"01234567-run"}}}}` + "\n"))
	})

	It("keeps the model key from OpenCode's built-in providers", func(ctx SpecContext) {
		env := entryEnv(ctx)
		Expect(env).NotTo(ContainSubstring("OPENAI_API_KEY="))
		Expect(env).To(ContainSubstring("KOINON_MODEL_KEY=" + fakeKey + "\n"))
		Expect(env).To(ContainSubstring("NO_PROXY=127.0.0.1,localhost"))
	})
})
