// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/acp"
	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// flavor is what the contract suite needs to know about one flavor.
type flavor struct {
	name string
	// dockerfile is the build context, relative to the repository root.
	dockerfile string
	// command is the ACP launch command koinon-agentd execs in the sandbox.
	command func(modelBaseURL, model, mcpURL string) []string
	// writeFile and readFile are the harness's own tool calls for these actions, as the
	// model would emit them.
	writeFile func(path, content string) fakemodel.ToolCall
	readFile  func(path string) fakemodel.ToolCall
	// outsidePath is a readable file whose access makes the harness ask for permission.
	outsidePath string
	// mcpToolName is how the harness presents the stand-in MCP tool to the model.
	mcpToolName string
	// allow and reject are the permission option kinds to answer with.
	allow, reject []acp.PermissionOptionKind
	// readyCommand is the command the harness announces once the Koinon MCP server's tools are
	// registered (the server's readyPrompt as a command); koinon-agentd waits for it before the
	// first prompt. Empty means the harness announces none.
	readyCommand string
	// selectModel, when set, is the model value koinon-agentd selects with
	// session/set_config_option after starting or loading a session.
	selectModel func(model string) string
	// projectConfig is harness configuration a repository could ship, by path relative to the
	// session directory. It must have no effect: read by the harness, it would put
	// projectMarker into the model's context, and it may loosen permissions, which the
	// permission specs then catch. Empty skips the check.
	projectConfig map[string]string
}

// projectMarker is the text a flavor's projectConfig would bring into the model's context.
const projectMarker = "KOINON-PROJECT-CONFIG-MARKER"

// readySettle is how long koinon-agentd waits after the ready command before the first prompt:
// OpenCode 2.0.24 announces the command and registers the server's tools on separate paths, the
// tools after a 100 ms debounce (packages/core/src/tool/mcp.ts), so the command can come first.
const readySettle = time.Second

// instructionsPath is where koinon-agentd writes the run's instructions before it starts the
// harness; instructionsMarker is in the contract's instructions, and the fake model answers
// helloPrompt only when its system prompt carries it.
const (
	instructionsPath   = "/sandbox/.koinon/AGENTS.md"
	instructionsMarker = "KOINON-CONTRACT-INSTRUCTIONS"
)

func (f flavor) image() string {
	if img := os.Getenv("KOINON_FLAVOR_IMAGE_" + strings.ToUpper(f.name)); img != "" {
		return img
	}
	return "koinon/flavor-" + f.name + ":" + f.tag()
}

// tag is the image tag for this tree's build context, from the script the Makefile uses, so an
// image built from another branch or worktree is never picked up.
func (f flavor) tag() string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		Fail("repository root: " + err.Error())
	}
	out, err := exec.CommandContext(context.Background(), filepath.Join(root, "hack", "flavor-tag.sh"), f.name).Output() //nolint:gosec // test-controlled
	if err != nil {
		Fail("hack/flavor-tag.sh " + f.name + ": " + err.Error())
	}
	return strings.TrimSpace(string(out))
}

// Values the contract checks for. The key is fake: plain Docker has no OpenShell placeholder
// substitution, so the harness sends exactly this value.
const (
	contractModel = "koinon-contract/fake-1"
	fakeKey       = "sk-koinon-contract-fake"
	workdir       = "/sandbox/work"

	helloPrompt  = "Say hello."
	helloReply   = "Hello from the fake model."
	writePrompt  = "KOINON-WRITE notes.txt please."
	wroteReply   = "Wrote notes.txt."
	readPrompt   = "KOINON-READ-OUTSIDE the config, please."
	readReply    = "Done with the file."
	recallPrompt = "What did you say first?"

	noInstructionsReply = "Koinon's instructions did not reach the system prompt."
)

func contractScript(f flavor) fakemodel.Script {
	return fakemodel.Script{
		Model: contractModel,
		Turns: []fakemodel.Turn{
			{When: fakemodel.When{ToolResult: f.writeFile("", "").Name}, Text: wroteReply},
			{When: fakemodel.When{ToolResult: f.readFile("").Name}, Text: readReply},
			{
				When:      fakemodel.When{UserContains: "KOINON-WRITE"},
				ToolCalls: []fakemodel.ToolCall{f.writeFile(workdir+"/notes.txt", "koinon\n")},
			},
			{
				When:      fakemodel.When{UserContains: "KOINON-READ-OUTSIDE"},
				ToolCalls: []fakemodel.ToolCall{f.readFile(f.outsidePath)},
			},
			{When: fakemodel.When{SystemContains: instructionsMarker}, Text: helloReply},
			{Text: noInstructionsReply},
		},
	}
}

// describeContract registers the contract specs for one flavor. They run in order against one
// sandbox, as one agent's session would.
func describeContract(f flavor) bool {
	return Describe(f.name, Ordered, func() {
		var (
			gw      *gateway
			sb      *sandbox
			h       *harness
			session acp.SessionID
		)

		// step bounds each spec; harness start-up and model turns take seconds.
		step := func() context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			DeferCleanup(cancel)
			return ctx
		}

		startHarness := func(ctx context.Context) {
			h = sb.harness(ctx, workdir, f.command(gw.url("/v1"), contractModel, gw.url("/mcp")))
		}

		// ready does what koinon-agentd does between session/new or load and the first prompt.
		ready := func(ctx context.Context) {
			if f.readyCommand != "" {
				Eventually(h.client.availableCommands).WithTimeout(30*time.Second).
					Should(ContainElement(f.readyCommand), "the harness never announced the Koinon MCP server")
				time.Sleep(readySettle)
			}
			if f.selectModel != nil {
				Expect(h.conn.SetModel(ctx, session, f.selectModel(contractModel))).To(Succeed())
			}
		}

		BeforeAll(func() {
			if dockerErr != nil {
				Skip("Docker is not available: " + dockerErr.Error())
			}
			ensureImage(f)
			gw = newGateway(contractScript(f))
			sb = newSandbox(f.image(), "OPENAI_API_KEY="+fakeKey)
			// Background: harness processes outlive a single spec's context.
			bg, cancel := context.WithCancel(context.Background())
			DeferCleanup(func() {
				if h != nil {
					h.close()
				}
				cancel()
				sb.remove(context.Background())
			})
			sb.start(bg)
			sb.run(bg, "mkdir", "-p", workdir)
			sb.write(bg, instructionsPath, "# Koinon\n\n"+instructionsMarker+": you are a contract test agent.\n")
			for name, content := range f.projectConfig {
				sb.write(bg, workdir+"/"+name, content)
			}
			startHarness(bg)
		})

		It("initializes and advertises session/load", func() {
			init, err := h.conn.Initialize(step())
			Expect(err).NotTo(HaveOccurred())
			Expect(init.ProtocolVersion).To(BeNumerically("==", acp.ProtocolVersion))
			Expect(init.AgentCapabilities.LoadSession).To(BeTrue())
		})

		It("starts a session", func() {
			ctx := step()
			var err error
			session, err = h.conn.NewSession(ctx, workdir)
			Expect(err).NotTo(HaveOccurred())
			Expect(session).NotTo(BeEmpty())
			ready(ctx)
		})

		It("answers a prompt through the gateway with the agent's key, its instructions and the Koinon MCP tools", func() {
			stop, err := h.conn.Prompt(step(), session, helloPrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			updates, _ := h.client.take()
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(helloReply))

			Expect(gw.authHeaders()).NotTo(BeEmpty())
			Expect(gw.authHeaders()).To(HaveEach("Bearer " + fakeKey))
			Expect(gw.mcpMethods()).To(ContainElements("initialize", "tools/list"))
			Expect(gw.modelBodies()).To(ContainElement(And(
				ContainSubstring(contractModel), ContainSubstring(helloPrompt), ContainSubstring(f.mcpToolName),
			)))
		})

		It("ignores harness configuration in the repository", func() {
			if len(f.projectConfig) == 0 {
				Skip("the flavor declares no project configuration to check")
			}
			Expect(gw.modelBodies()).NotTo(BeEmpty())
			Expect(gw.modelBodies()).To(HaveEach(And(
				ContainSubstring(contractModel), Not(ContainSubstring(projectMarker)),
			)))
		})

		It("runs a tool call from the model", func() {
			ctx := step()
			stop, err := h.conn.Prompt(ctx, session, writePrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			updates, _ := h.client.take()
			Expect(updates).To(ContainElement(And(
				HaveField("Kind", "tool_call_update"), HaveField("ToolStatus", acp.ToolCompleted),
			)))
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(wroteReply))
			Expect(sb.run(ctx, "cat", workdir+"/notes.txt")).To(Equal("koinon\n"))
		})

		It("asks for permission and honours allow", func() {
			h.client.answerWith(f.allow...)
			stop, err := h.conn.Prompt(step(), session, readPrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			updates, perms := h.client.take()
			Expect(perms).NotTo(BeEmpty())
			Expect(perms[0].SessionId).To(Equal(session))
			Expect(updates).To(ContainElement(And(
				HaveField("Kind", "tool_call_update"), HaveField("ToolStatus", acp.ToolCompleted),
			)))
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(readReply))
		})

		It("asks for permission and honours reject", func() {
			h.client.answerWith(f.reject...)
			stop, err := h.conn.Prompt(step(), session, readPrompt)
			Expect(err).NotTo(HaveOccurred())
			updates, perms := h.client.take()
			Expect(perms).NotTo(BeEmpty())
			Expect(updates).To(ContainElement(And(
				HaveField("Kind", "tool_call_update"), HaveField("ToolStatus", acp.ToolFailed),
			)))
			Expect(updates).NotTo(ContainElement(HaveField("ToolStatus", acp.ToolCompleted)))
			// A rejected tool call may end the turn or hand the error back to the model.
			Expect(stop).To(BeElementOf(acp.StopEndTurn, acp.StopCancelled))
		})

		It("cancels a running turn", func() {
			ctx := step()
			done := make(chan acp.StopReason, 1)
			go func() {
				defer GinkgoRecover()
				stop, err := h.conn.Prompt(ctx, session, hangMarker+" wait for the model.")
				Expect(err).NotTo(HaveOccurred())
				done <- stop
			}()
			Eventually(gw.hung).WithTimeout(time.Minute).Should(Receive())
			Expect(h.conn.Cancel(ctx, session)).To(Succeed())
			Eventually(done).WithTimeout(time.Minute).Should(Receive(Equal(acp.StopCancelled)))
			h.client.take()
		})

		It("loads the session after a restart, with the home on the volume", func() {
			ctx := step()
			h.close()
			h = nil
			sb.stop(ctx)
			bg := context.Background()
			sb.start(bg)
			startHarness(bg)

			_, err := h.conn.Initialize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(h.conn.LoadSession(ctx, session, workdir)).To(Succeed())
			ready(ctx)
			updates, _ := h.client.take()
			Expect(acp.Text(updates, "user_message_chunk")).To(ContainSubstring(helloPrompt))
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(helloReply))

			stop, err := h.conn.Prompt(ctx, session, recallPrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			// The model sees the earlier turns: the history came back from the volume.
			Expect(gw.modelBodies()).To(ContainElement(And(
				ContainSubstring(recallPrompt), ContainSubstring(helloReply), ContainSubstring(wroteReply),
			)))
		})

		It("refuses to load an unknown session", func() {
			Expect(h.conn.LoadSession(step(), "ses_koinon_contract_unknown", workdir)).NotTo(Succeed())
		})
	})
}

// ensureImage fails the flavor's specs when its image is missing, naming the tag and the make
// command, or builds it when KOINON_FLAVOR_BUILD=1. It never falls back to another tag.
func ensureImage(f flavor) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	img := f.image()
	if exec.CommandContext(ctx, "docker", "image", "inspect", img).Run() == nil { //nolint:gosec // test-controlled
		return
	}
	if os.Getenv("KOINON_FLAVOR_BUILD") != "1" {
		Fail("image " + img + " not found for this tree's flavors/" + f.name + " and flavors/common; run `make flavor-" +
			f.name + "` (tags are per source tree) or set KOINON_FLAVOR_BUILD=1")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	Expect(err).NotTo(HaveOccurred())
	args := []string{"buildx", "build", "--load", "--tag", img}
	if _, err := os.Stat(filepath.Join(root, "flavors", "common")); err == nil {
		args = append(args, "--build-context", "common="+filepath.Join(root, "flavors", "common"))
	}
	out, err := exec.CommandContext(ctx, "docker", append(args, filepath.Join(root, f.dockerfile))...).CombinedOutput() //nolint:gosec // test-controlled
	Expect(err).NotTo(HaveOccurred(), "build %s: %s", img, out)
}
