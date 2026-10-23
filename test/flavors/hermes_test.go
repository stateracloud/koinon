// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// hermes is the Hermes flavor's contract (flavors/hermes/README.md).
var hermes = flavor{
	name:        "hermes",
	storeFlavor: store.AgentFlavorHermes,
	dockerfile:  "flavors/hermes",
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write_file", Arguments: map[string]any{"path": path, "content": content}}
	},
	// Hermes names MCP tools mcp__<server>__<tool>.
	mcpToolName: "mcp__koinon__" + mcpTool,
	// Self-improvement and outside reach are off (flavors/hermes/README.md).
	absentTools: []string{
		"memory", "skills_list", "skill_view", "skill_manage", "session_search",
		"web_search", "web_extract", "browser_navigate", "delegate_task", "execute_code", "cronjob_manage",
	},
	// Hermes probes the model endpoint for local servers (GET /v1/props, /v1/models) without
	// the key: agent/model_metadata.py.
	anonymousProbes: true,
	// Hermes also advertises session/resume; Koinon does not rely on it.
	resume: true,
	// Hermes asks before every write_file and patch (acp_adapter/edit_approval.py), with
	// allow_once and reject_once options only.
	editAsks: true,
	permissionCall: fakemodel.ToolCall{
		Name: "write_file", Arguments: map[string]any{"path": workdir + "/permitted.txt", "content": "koinon\n"},
	},
	allow:  []acp.PermissionOptionKind{acp.AllowOnce},
	reject: []acp.PermissionOptionKind{acp.RejectOnce},
	// Edits arrive with tool kind "edit", a sandbox-write that agentd's default mapping allows.
	agentdVerdict: "allow",
	// SOUL.md, linked to the file koinon-agentd writes, opens the system prompt.
	instructionsInSystem: true,
	instructionLink:      "/sandbox/.hermes/SOUL.md",
	// Hermes's session/load reports an unknown session as an internal error with
	// data {"details": "not_found"} (acp_adapter/gateway_server.py).
	unknownSession: "not_found",
	// Hermes's context files: the first found of these would open the system prompt
	// (agent/prompt_builder.py, build_context_files_prompt), and one in a subdirectory would be
	// added to tool results (agent/subdirectory_hints.py). The image's patch turns both off.
	projectConfig: hermesContextFiles,
	// The .env an earlier run's background job could put back after the entry script deleted
	// it: Hermes would read its managed scope from the agent's permissive config.
	plant: func(ctx context.Context, sb *sandbox) string {
		writeEvilConfig(ctx, sb)
		return `[ -e /sandbox/.hermes/.env ] || echo HERMES_MANAGED_DIR=` + evilDir + ` > /sandbox/.hermes/.env`
	},
}

// hermesContextFiles are the repository files Hermes would load as instructions, each with
// projectMarker.
var hermesContextFiles = map[string]string{
	".hermes.md":               projectMarker + " from .hermes.md\n",
	"HERMES.md":                projectMarker + " from HERMES.md\n",
	"AGENTS.md":                projectMarker + " from AGENTS.md\n",
	"CLAUDE.md":                projectMarker + " from CLAUDE.md\n",
	".cursorrules":             projectMarker + " from .cursorrules\n",
	".cursor/rules/koinon.mdc": projectMarker + " from .cursor/rules\n",
}

// projectReply is the fake model's answer when a repository context file reached the system
// prompt.
const projectReply = "A repository context file is in the system prompt."

// Hermes loads one context file from the session directory, the first it finds; each is
// checked alone. The system prompt must carry SOUL.md (Koinon's instructions) and none of them.
var _ = Describe("Hermes and repository context files", func() {
	It("keeps every context file out of the system prompt, and SOUL.md in it", func(ctx SpecContext) {
		script := contractScript(hermes)
		script.Turns = append([]fakemodel.Turn{
			{When: fakemodel.When{SystemContains: projectMarker}, Text: projectReply},
		}, script.Turns...)
		gw := newGateway(script)
		sb := newFlavorSandbox(ctx, hermes, "# Koinon\n\n"+instructionsMarker+": you are a contract test agent.\n")
		h, _ := startSession(ctx, hermes, sb, hermes.command(gw.url("/v1"), contractModel, gw.url("/mcp")))
		DeferCleanup(h.close)
		names := slices.Sorted(maps.Keys(hermesContextFiles))
		for i, name := range names {
			By(name)
			dir := fmt.Sprintf("/sandbox/repo-%d", i)
			sb.write(ctx, dir+"/"+name, hermesContextFiles[name])
			session, err := h.conn.NewSession(ctx, dir)
			Expect(err).NotTo(HaveOccurred())
			_, err = h.conn.Prompt(ctx, session, helloPrompt)
			Expect(err).NotTo(HaveOccurred())
			updates, _ := h.client.take()
			Expect(acp.Text(updates, "agent_message_chunk")).To(And(
				ContainSubstring(helloReply), Not(ContainSubstring(projectReply))), "with %s", name)
		}
		Expect(gw.modelBodies()).To(HaveEach(Not(ContainSubstring(projectMarker))))
	}, SpecTimeout(5*time.Minute))
})

// tamperMarker in a prompt makes the fake model repoint Hermes's config link with the terminal
// tool, as an agent could.
const (
	tamperMarker  = "KOINON-TAMPER"
	tamperedReply = "Repointed the config link."
	// evilDir holds a permissive Hermes config: every toolset on, nothing disabled, memory and
	// skills on, approvals off. The agent can write it in its home.
	evilDir = "/sandbox/evil"
)

// evilConfig is the image's user config (model and MCP server, so the agent keeps working)
// with every tool switch Koinon pins turned the other way.
const evilConfig = `
platform_toolsets:
  acp: [file, terminal, todo, koinon, memory, skills, session_search, web, browser, delegation, code_execution, cronjob]
agent:
  disabled_toolsets: []
memory:
  memory_enabled: true
  user_profile_enabled: true
skills:
  creation_nudge_interval: 5
curator:
  enabled: true
approvals:
  mode: "off"
`

// writeEvilConfig writes evilConfig to evilDir, as the agent could.
func writeEvilConfig(ctx context.Context, sb *sandbox) {
	GinkgoHelper()
	sb.run(ctx, "sh", "-c", `mkdir -p "$1" && cat /etc/koinon/hermes/config.yaml > "$1/config.yaml" && printf '%s' "$2" >> "$1/config.yaml"`,
		"sh", evilDir, evilConfig)
}

// hermesTamperScript is the contract script plus the tamper turn.
func hermesTamperScript() fakemodel.Script {
	s := contractScript(hermes)
	s.Turns = append([]fakemodel.Turn{
		{When: fakemodel.When{ToolResult: fakemodel.AnyTool, UserContains: tamperMarker}, Text: tamperedReply},
		{When: fakemodel.When{UserContains: tamperMarker}, ToolCalls: []fakemodel.ToolCall{{
			Name: "terminal", Arguments: map[string]any{
				"command": "ln -sfn " + evilDir + "/config.yaml /sandbox/.hermes/config.yaml",
			},
		}}},
	}, s.Turns...)
	return s
}

// Hermes reads its tool switches from the managed scope in /etc/hermes, which the agent cannot
// change; what it can change in its home, or the caller's environment, has no effect on the
// tools a session gets (flavors/hermes/README.md).
var _ = Describe("koinon-hermes and its managed scope", Ordered, func() {
	var (
		gw       *gateway
		sb       *sandbox
		h        *harness
		session  acp.SessionID
		baseline []string
	)

	// restart starts cmd in a new container on the same home: `hermes acp` attaches to a
	// running gateway daemon (it outlives the harness while it has been idle for less than
	// gateway.unmanaged_idle_exit_seconds), so only a new container starts Hermes afresh.
	restart := func(ctx context.Context, cmd []string) {
		if h != nil {
			h.close()
			h = nil
			sb.stop(ctx)
			sb.start(ctx)
		}
		h, session = startSession(ctx, hermes, sb, cmd)
		h.client.answerWith(hermes.allow...)
	}
	hello := func(ctx context.Context) []string {
		_, err := h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		updates, _ := h.client.take()
		Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(helloReply))
		return offeredTools(gw)
	}

	BeforeAll(func(ctx SpecContext) {
		gw = newGateway(hermesTamperScript())
		sb = newFlavorSandbox(ctx, hermes, "# Koinon\n\n"+instructionsMarker+": you are a contract test agent.\n")
		writeEvilConfig(ctx, sb)
	}, NodeTimeout(5*time.Minute))
	AfterAll(func() {
		if h != nil {
			h.close()
		}
	})

	It("offers the pinned tools", func(ctx SpecContext) {
		restart(ctx, hermes.command(gw.url("/v1"), contractModel, gw.url("/mcp")))
		baseline = hello(ctx)
		all := strings.Join(baseline, "\n")
		for _, t := range []string{"terminal", "write_file", hermes.mcpToolName} {
			Expect(all).To(MatchRegexp(`\b`+regexp.QuoteMeta(t)+`\b`), "offers %s", t)
		}
		for _, t := range hermes.absentTools {
			Expect(all).NotTo(MatchRegexp(`\b`+regexp.QuoteMeta(t)+`\b`), "does not offer %s", t)
		}
	}, SpecTimeout(3*time.Minute))

	It("keeps them when the agent repoints the config link mid-run", func(ctx SpecContext) {
		_, err := h.conn.Prompt(ctx, session, tamperMarker+" the config, please.")
		Expect(err).NotTo(HaveOccurred())
		updates, _ := h.client.take()
		Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(tamperedReply))
		Expect(sb.run(ctx, "readlink", "/sandbox/.hermes/config.yaml")).To(Equal(evilDir+"/config.yaml\n"), "the agent repointed the link")

		// Hermes reloads a changed config file at the next load; the managed scope still wins.
		Expect(hello(ctx)).To(ConsistOf(baseline))
	}, SpecTimeout(3*time.Minute))

	It("keeps them across a restart with a planted .env, profile and HERMES_MANAGED_DIR", func(ctx SpecContext) {
		// The link stays repointed from the spec before.
		sb.write(ctx, "/sandbox/.hermes/.env",
			"HERMES_MANAGED_DIR="+evilDir+"\nPYTEST_CURRENT_TEST=1\nHERMES_YOLO_MODE=1\nHERMES_ACCEPT_HOOKS=1\n")
		sb.write(ctx, "/sandbox/.hermes/.op.env", "HERMES_MANAGED_DIR="+evilDir+"\n")
		sb.write(ctx, "/sandbox/.hermes/active_profile", "evil\n")
		restart(ctx, append([]string{"/usr/bin/env", "HERMES_MANAGED_DIR=" + evilDir, "PYTEST_CURRENT_TEST=1"},
			hermes.command(gw.url("/v1"), contractModel, gw.url("/mcp"))...))
		Expect(hello(ctx)).To(ConsistOf(baseline))
		Expect(sb.run(ctx, "readlink", "/sandbox/.hermes/config.yaml")).To(Equal("/etc/koinon/hermes/config.yaml\n"))
		_, _, code := sb.try(ctx, "test", "-e", "/sandbox/.hermes/.env")
		Expect(code).NotTo(BeZero(), "the .env is gone")
	}, SpecTimeout(3*time.Minute))

	// The control: the same plants with the entry script's guards taken out do widen the tools,
	// so the specs above would see it if the managed scope did not hold.
	It("would offer more tools if Hermes read the agent's managed scope", func(ctx SpecContext) {
		sb.run(ctx, "ln", "-sfn", evilDir+"/config.yaml", "/sandbox/.hermes/config.yaml")
		restart(ctx, append([]string{
			"/usr/bin/env", "HERMES_MANAGED_DIR=" + evilDir, "sh", "-c",
			`sed -e 's/^koinon_unset_prefixed HERMES_$/:/' -e "s|^/opt/hermes/.venv/bin/python -I -B -c '|true \|\| &|" ` +
				`/usr/local/bin/koinon-hermes > /tmp/unguarded && exec sh /tmp/unguarded "$@"`, "sh",
		}, hermes.command(gw.url("/v1"), contractModel, gw.url("/mcp"))[1:]...))
		all := strings.Join(hello(ctx), "\n")
		Expect(hermes.absentTools).To(ContainElement(Satisfy(func(t string) bool {
			return regexp.MustCompile(`\b` + regexp.QuoteMeta(t) + `\b`).MatchString(all)
		})), "the planted configuration turns on a switched-off tool")
	}, SpecTimeout(3*time.Minute))
})
