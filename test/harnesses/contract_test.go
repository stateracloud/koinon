// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// definition is what the contract suite needs to know about one harness: how to drive it, and what
// it is expected to do where harnesses legitimately differ.
type definition struct {
	name         string
	storeHarness string
	// dockerfile is the build context, relative to the repository root.
	dockerfile string
	// writeFile is the harness's own tool call that writes a file, as the model would emit it.
	writeFile func(path, content string) fakemodel.ToolCall
	// mcpToolName is how the harness presents the stand-in MCP tool to the model.
	mcpToolName string
	// absentTools are tools the harness's configuration switches off; the model must not be
	// offered them.
	absentTools []string

	// anonymousProbes is whether the harness also sends metadata GETs to the model endpoint
	// without the key (a harness probing for a local model server). They carry no
	// credential; agentgateway answers them 401.
	anonymousProbes bool
	// resume is whether the harness advertises session/resume. Koinon relies on session/load
	// only (ADR 0048), so this documents the harness rather than gating anything.
	resume bool
	// editAsks is whether the harness asks for permission before writeFile.
	editAsks bool
	// permissionCall is a tool call that makes the harness ask for permission, or nil when the
	// harness never sends session/request_permission.
	permissionCall fakemodel.ToolCall
	// allow and reject are the permission option kinds to answer with.
	allow, reject []acp.PermissionOptionKind
	// agentdVerdict is how koinon-agentd's default tier mapping answers permissionCall.
	agentdVerdict string
	// rejectEndsTurn is whether the harness ends the turn as cancelled when a permission request
	// is rejected, instead of handing the model a tool error. koinon-agentd then continues the
	// run with a note (internal/agentd/refusal.go).
	rejectEndsTurn bool
	// unknownSession is part of the error session/load returns for an unknown session ID.
	unknownSession string
	// instructionsInSystem is whether the harness puts the instruction file koinon-agentd
	// writes (instructionsPath) into the model's system prompt.
	instructionsInSystem bool
	// instructionLink is the path the harness reads the instructions from, a symbolic link to
	// instructionsPath (internal/agentd, instructionLink). One in the home is put back on
	// every start; one in the image cannot change. Empty when the harness's config names the file.
	instructionLink string
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
	// dataTools is whether the image ships the data tools of ADR 0052's amendment of 2026-10-28
	// (tools_test.go).
	dataTools bool
	// sizeBudget is the most the image may weigh, in KiB per architecture, as `du -sxk /` inside
	// the container reports it: what it measured when the budget was last set, plus about 10%. A
	// tool or package bump that grows the image is then a visible decision, not a surprise. `docker
	// image inspect` is no measure: its size is compressed or layer-summed depending on the image
	// store. Empty skips the check.
	sizeBudget map[string]int64
	// plant prepares sb for a background job of the agent's that would change a later run from
	// the home, and returns the shell command the job repeats. Nil repeats nothing.
	plant func(ctx context.Context, sb *sandbox) string
}

// projectMarker is the text a harness's projectConfig would bring into the model's context.
const projectMarker = "KOINON-PROJECT-CONFIG-MARKER"

// readySettle is how long koinon-agentd waits after the ready command before the first prompt:
// OpenCode 2.2.0 announces the command and registers the server's tools on separate paths, the
// tools after a 100 ms debounce (packages/core/src/tool/mcp.ts), so the command can come first.
const readySettle = time.Second

// instructionsPath is where koinon-agentd writes the run's instructions before it starts the
// harness; instructionsMarker is in the contract's instructions, and for a harness with
// instructionsInSystem the fake model answers helloPrompt only when its system prompt carries
// it.
const (
	instructionsPath   = "/sandbox/.koinon/AGENTS.md"
	instructionsMarker = "KOINON-CONTRACT-INSTRUCTIONS"
)

func (f definition) image() string {
	if img := os.Getenv("KOINON_HARNESS_IMAGE_" + strings.ToUpper(f.name)); img != "" {
		return img
	}
	return "koinon/sandbox:" + f.tag()
}

// tag is the image tag for this tree's build context, from the script the Makefile uses, so an
// image built from another branch or worktree is never picked up.
func (f definition) tag() string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		Fail("repository root: " + err.Error())
	}
	out, err := exec.CommandContext(context.Background(), filepath.Join(root, "hack", "harness-tag.sh"), f.name).Output()
	if err != nil {
		Fail("hack/harness-tag.sh " + f.name + ": " + err.Error())
	}
	return strings.TrimSpace(string(out))
}

// command is the ACP launch command koinon-agentd execs in the sandbox: the same for every
// harness apart from the entry script (internal/agentd, harnesses/README.md).
func (f definition) command(modelBaseURL, model, mcpURL string) []string {
	return []string{
		"/usr/local/bin/koinon-" + f.name,
		"--model-base-url", modelBaseURL,
		"--model", model,
		"--mcp-url", mcpURL,
	}
}

// harnesses is every harness the suite checks.
var harnesses = []definition{opencode}

// Values the contract checks for. The key is fake: plain Docker has no OpenShell placeholder
// substitution, so the harness sends exactly this value.
const (
	contractModel = "koinon-contract/fake-1"
	fakeKey       = "sk-koinon-contract-fake"
	// fakeGitHubPlaceholder stands in for the GitHub provider's placeholder in GITHUB_TOKEN.
	fakeGitHubPlaceholder = "koinon-contract-github-placeholder"
	workdir               = "/sandbox/work"

	helloPrompt   = "Say hello."
	helloReply    = "Hello from the fake model."
	writePrompt   = "KOINON-WRITE notes.txt please."
	wroteReply    = "Wrote notes.txt."
	permitMarker  = "KOINON-PERMIT"
	permitPrompt  = permitMarker + " the guarded tool call, please."
	permitReply   = "Done with the guarded tool call."
	recallPrompt  = "What did you say first?"
	unknownSessID = "ses_koinon_contract_unknown"

	noInstructionsReply = "Koinon's instructions did not reach the system prompt."
)

// contractScript is the fake model's side of the contract. Replies to tool results are told
// apart by the prompt that started the turn, so the scripted tool calls may use the same tool.
func contractScript(f definition) fakemodel.Script {
	turns := []fakemodel.Turn{
		{When: fakemodel.When{ToolResult: fakemodel.AnyTool, UserContains: "KOINON-WRITE"}, Text: wroteReply},
		{When: fakemodel.When{ToolResult: fakemodel.AnyTool, UserContains: permitMarker}, Text: permitReply},
		{
			When:      fakemodel.When{UserContains: "KOINON-WRITE"},
			ToolCalls: []fakemodel.ToolCall{f.writeFile(workdir+"/notes.txt", "koinon\n")},
		},
	}
	if f.permissionCall.Name != "" {
		turns = append(turns, fakemodel.Turn{
			When: fakemodel.When{UserContains: permitMarker}, ToolCalls: []fakemodel.ToolCall{f.permissionCall},
		})
	}
	if f.instructionsInSystem {
		turns = append(turns,
			fakemodel.Turn{When: fakemodel.When{SystemContains: instructionsMarker}, Text: helloReply},
			fakemodel.Turn{Text: noInstructionsReply})
	} else {
		turns = append(turns, fakemodel.Turn{Text: helloReply})
	}
	return fakemodel.Script{Model: contractModel, Turns: turns}
}

// describeContract registers the contract specs for one harness. They run in order against one
// sandbox, as one agent's session would.
func describeContract(f definition) bool {
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
			testenv.Require(testenv.Docker)
			ensureImage(f)
			gw = newGateway(contractScript(f))
			sb = newSandbox(f.image(), "OPENAI_API_KEY="+fakeKey, "GITHUB_TOKEN="+fakeGitHubPlaceholder)
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

		It("refuses launch arguments that could change its configuration", func() {
			ctx := step()
			for _, args := range [][]string{
				f.command(gw.url("/v1"), `m"},"x":{"`, gw.url("/mcp")),
				f.command(gw.url("/v1"), "${OPENAI_API_KEY}", gw.url("/mcp")),
				f.command("file:///etc/passwd", contractModel, gw.url("/mcp")),
				f.command(gw.url("/v1"), contractModel, "")[:5],
			} {
				c := exec.CommandContext(ctx, "docker", append([]string{"exec", sb.name}, args...)...)
				out, err := c.CombinedOutput()
				var exit *exec.ExitError
				Expect(errors.As(err, &exit)).To(BeTrue(), "%v: %s", args, out)
				Expect(exit.ExitCode()).To(Equal(64), "%v: %s", args, out)
			}
		})

		It("gives git the GitHub placeholder through its credential helper and stores nothing", func() {
			ctx := step()
			// git asks its helpers as it would before a clone; without an answer it would
			// prompt, which GIT_TERMINAL_PROMPT=0 turns into a failure.
			credential := func(op, input string) (string, error) {
				c := exec.CommandContext(ctx, "docker", "exec", "--interactive", "--env", "GIT_TERMINAL_PROMPT=0",
					"--workdir", workdir, sb.name, "/usr/bin/git", "credential", op)
				c.Stdin = strings.NewReader(input)
				out, err := c.CombinedOutput()
				return string(out), err
			}
			out, err := credential("fill", "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n")
			Expect(err).NotTo(HaveOccurred(), out)
			Expect(out).To(ContainSubstring("username=x-access-token\n"))
			Expect(out).To(ContainSubstring("password=" + fakeGitHubPlaceholder + "\n"))
			// git hands a used credential to the helpers to store; they must not.
			out, err = credential("approve", "protocol=https\nhost=github.com\nusername=x-access-token\npassword="+fakeGitHubPlaceholder+"\n\n")
			Expect(err).NotTo(HaveOccurred(), out)

			out, err = credential("fill", "protocol=https\nhost=example.com\n\n")
			Expect(err).To(HaveOccurred(), "no credential for other hosts: %s", out)
			Expect(out).NotTo(ContainSubstring(fakeGitHubPlaceholder))

			// The configuration is the image's, read-only; nothing in the sandbox holds the value.
			Expect(sb.run(ctx, "/usr/bin/git", "config", "--show-origin", "--get-all", "credential.https://github.com.helper")).
				To(ContainSubstring("file:/etc/koinon/git/config\t/usr/local/bin/koinon-git-credential"))
			Expect(sb.run(ctx, "stat", "-c", "%u %A", "/etc/koinon/git/config", "/usr/local/bin/koinon-git-credential")).
				To(Equal("0 -rw-r--r--\n0 -rwxr-xr-x\n"))
			grep := exec.CommandContext(ctx, "docker", "exec", sb.name, "grep", "-rlsF", fakeGitHubPlaceholder, "/sandbox", "/tmp", "/etc")
			found, err := grep.Output()
			Expect(err).To(HaveOccurred(), "grep found the placeholder in %s", found)
		})

		It("initializes and advertises session/load", func() {
			init, err := h.conn.Initialize(step())
			Expect(err).NotTo(HaveOccurred())
			Expect(init.ProtocolVersion).To(BeNumerically("==", acp.ProtocolVersion))
			Expect(init.AgentCapabilities.LoadSession).To(BeTrue())
			if f.resume {
				Expect(init.AgentCapabilities.SessionCapabilities.Resume).NotTo(BeNil(), "advertises session/resume")
			} else {
				Expect(init.AgentCapabilities.SessionCapabilities.Resume).To(BeNil(), "does not advertise session/resume")
			}
		})

		It("starts a session", func() {
			ctx := step()
			var err error
			session, err = h.conn.NewSession(ctx, workdir)
			Expect(err).NotTo(HaveOccurred())
			Expect(session).NotTo(BeEmpty())
			ready(ctx)
		})

		It("answers a prompt through the gateway with the agent's key and the Koinon MCP tools", func() {
			stop, err := h.conn.Prompt(step(), session, helloPrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			updates, _ := h.client.take()
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(helloReply))

			Expect(gw.authHeaders()).NotTo(BeEmpty())
			Expect(gw.authHeaders()).To(HaveEach("Bearer " + fakeKey))
			if f.anonymousProbes {
				// Metadata reads only; every chat completion and MCP call carries the key.
				Expect(gw.anonymous()).To(HaveEach(HavePrefix("GET /v1/")))
			} else {
				Expect(gw.anonymous()).To(BeEmpty())
			}
			Expect(gw.mcpMethods()).To(ContainElements("initialize", "tools/list"))
			Expect(gw.modelBodies()).To(ContainElement(And(
				ContainSubstring(contractModel), ContainSubstring(helloPrompt), ContainSubstring(f.mcpToolName),
			)))
			for _, t := range f.absentTools {
				Expect(gw.modelBodies()).NotTo(ContainElement(MatchRegexp(`"name":\s*"`+regexp.QuoteMeta(t)+`"`)),
					"tool %s is switched off", t)
			}
		})

		It("ignores harness configuration in the repository", func() {
			if len(f.projectConfig) == 0 {
				Skip("the harness declares no project configuration to check")
			}
			Expect(gw.modelBodies()).NotTo(BeEmpty())
			Expect(gw.modelBodies()).To(HaveEach(And(
				ContainSubstring(contractModel), Not(ContainSubstring(projectMarker)),
			)))
		})

		It("runs a tool call from the model", func() {
			ctx := step()
			h.client.answerWith(f.allow...)
			stop, err := h.conn.Prompt(ctx, session, writePrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			updates, perms := h.client.take()
			if f.editAsks {
				Expect(perms).NotTo(BeEmpty(), "asks for permission before the edit")
			} else {
				Expect(perms).To(BeEmpty(), "asks no permission for an edit in the session directory")
			}
			Expect(updates).To(ContainElement(And(
				HaveField("Kind", "tool_call_update"), HaveField("ToolStatus", acp.ToolCompleted),
			)))
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(wroteReply))
			Expect(sb.run(ctx, "cat", workdir+"/notes.txt")).To(Equal("koinon\n"))
		})

		It("asks for permission and honours allow", func() {
			if f.permissionCall.Name == "" {
				Skip(f.name + " sends no permission requests")
			}
			h.client.answerWith(f.allow...)
			stop, err := h.conn.Prompt(step(), session, permitPrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			updates, perms := h.client.take()
			Expect(perms).NotTo(BeEmpty())
			Expect(perms[0].SessionId).To(Equal(session))
			Expect(updates).To(ContainElement(And(
				HaveField("Kind", "tool_call_update"), HaveField("ToolStatus", acp.ToolCompleted),
			)))
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(permitReply))
		})

		It("asks for permission and honours reject", func() {
			if f.permissionCall.Name == "" {
				Skip(f.name + " sends no permission requests")
			}
			h.client.answerWith(f.reject...)
			stop, err := h.conn.Prompt(step(), session, permitPrompt)
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

		It("refuses to load an unknown session with an error, not a new session", func() {
			err := h.conn.LoadSession(step(), unknownSessID, workdir)
			Expect(err).To(MatchError(ContainSubstring(f.unknownSession)))
		})
	})
}

var _ = func() bool {
	for _, f := range harnesses {
		describeContract(f)
	}
	return true
}()

// ensureImage fails the harness's specs when its image is missing, naming the tag and the make
// command, or builds it when KOINON_HARNESS_BUILD=1. It never falls back to another tag.
func ensureImage(f definition) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	img := f.image()
	if exec.CommandContext(ctx, "docker", "image", "inspect", img).Run() == nil {
		return
	}
	if os.Getenv("KOINON_HARNESS_BUILD") != "1" {
		Fail("image " + img + " not found for this tree's harnesses/" + f.name + " and harnesses/common; run `make harness-" +
			f.name + "` (tags are per source tree) or set KOINON_HARNESS_BUILD=1")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	Expect(err).NotTo(HaveOccurred())
	out, err := exec.CommandContext(ctx, "docker", "buildx", "build", "--load", "--tag", img,
		"--build-context", "common="+filepath.Join(root, "harnesses", "common"),
		filepath.Join(root, f.dockerfile)).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build %s: %s", img, out)
}
