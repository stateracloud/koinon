// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/acp"
	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// opencode is the OpenCode flavor's contract (flavors/opencode/README.md).
var opencode = flavor{
	name:         "opencode",
	defaultImage: "koinon/flavor-opencode:dev",
	dockerfile:   "flavors/opencode",
	command: func(modelBaseURL, model, mcpURL string) []string {
		return []string{
			"/usr/local/bin/koinon-opencode",
			"--model-base-url", modelBaseURL,
			"--model", model,
			"--mcp-url", mcpURL,
		}
	},
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write", Arguments: map[string]any{"filePath": path, "content": content}}
	},
	readFile: func(path string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "read", Arguments: map[string]any{"filePath": path}}
	},
	// OpenCode asks before touching a path outside the session's directory
	// (permission external_directory, default "ask").
	outsidePath: "/etc/koinon/opencode/opencode.jsonc",
	// OpenCode names MCP tools <server>_<tool>; the server is "koinon" in its config.
	mcpToolName: "koinon_" + mcpTool,
	allow:       []acp.PermissionOptionKind{acp.AllowOnce},
	reject:      []acp.PermissionOptionKind{acp.RejectOnce},
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
	},
}

var _ = describeContract(opencode)

// The entry script's environment, with OpenCode itself replaced by env: external plugins
// load only with --plugin-installs (OPENCODE_PURE unset), which agentd passes when the
// template sets harness.pluginInstalls.
var _ = Describe("koinon-opencode plugin switch", Ordered, func() {
	var sb *sandbox

	BeforeAll(func() {
		if dockerErr != nil {
			Skip("Docker is not available: " + dockerErr.Error())
		}
		ensureImage(opencode)
		sb = newSandbox(opencode.image())
		DeferCleanup(func() { sb.remove(context.Background()) })
		sb.start(context.Background())
	})

	entryEnv := func(ctx context.Context, extra ...string) string {
		// The read-only root holds the script; run a copy whose last line prints the
		// environment instead of starting OpenCode.
		args := append([]string{"sh", "-c",
			`sed 's|^exec /usr/local/bin/opencode acp.*|exec /usr/bin/env|' /usr/local/bin/koinon-opencode > /tmp/entry && exec sh /tmp/entry "$@"`,
			"sh", "--model-base-url", "http://gw.test/v1", "--model", "m-1", "--mcp-url", "http://gw.test/mcp"}, extra...)
		return sb.run(ctx, args...)
	}

	It("keeps external plugins off by default", func(ctx SpecContext) {
		Expect(entryEnv(ctx)).To(ContainSubstring("OPENCODE_PURE=1\n"))
	})

	It("loads external plugins with --plugin-installs", func(ctx SpecContext) {
		env := entryEnv(ctx, "--plugin-installs")
		Expect(env).NotTo(ContainSubstring("OPENCODE_PURE"))
		Expect(env).To(ContainSubstring("OPENCODE_DISABLE_PROJECT_CONFIG=1\n"), "a repository still cannot configure the harness")
	})
})
