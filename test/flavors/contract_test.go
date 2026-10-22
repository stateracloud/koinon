// SPDX-License-Identifier: Apache-2.0

package flavors_test

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

	"github.com/procoduck/koinon/internal/acp"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// flavor is what the contract suite needs to know about one flavor: how to drive it, and what
// it is expected to do where harnesses legitimately differ.
type flavor struct {
	name         string
	storeFlavor  store.AgentFlavor
	defaultImage string
	// dockerfile is the build context, relative to the repository root.
	dockerfile string
	// writeFile is the harness's own tool call that writes a file, as the model would emit it.
	writeFile func(path, content string) fakemodel.ToolCall
	// mcpToolName is how the harness presents the stand-in MCP tool to the model.
	mcpToolName string
	// absentTools are tools the flavor's configuration switches off; the model must not be
	// offered them.
	absentTools []string

	// anonymousProbes is whether the harness also sends metadata GETs to the model endpoint
	// without the key (Hermes probing for a local llama.cpp or vLLM server). They carry no
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
	// unknownSession is part of the error session/load returns for an unknown session ID.
	unknownSession string
}

func (f flavor) image() string {
	if img := os.Getenv("KOINON_FLAVOR_IMAGE_" + strings.ToUpper(f.name)); img != "" {
		return img
	}
	return f.defaultImage
}

// command is the ACP launch command koinon-agentd execs in the sandbox: the same for every
// flavor apart from the entry script (internal/agentd, flavors/README.md).
func (f flavor) command(modelBaseURL, model, mcpURL string) []string {
	return []string{
		"/usr/local/bin/koinon-" + f.name,
		"--model-base-url", modelBaseURL,
		"--model", model,
		"--mcp-url", mcpURL,
	}
}

// flavors is every flavor the suite checks.
var flavors = []flavor{opencode, hermes, pi}

// Values the contract checks for. The key is fake: plain Docker has no OpenShell placeholder
// substitution, so the harness sends exactly this value.
const (
	contractModel = "koinon-contract/fake-1"
	fakeKey       = "sk-koinon-contract-fake"
	workdir       = "/sandbox/work"

	helloPrompt   = "Say hello."
	helloReply    = "Hello from the fake model."
	writePrompt   = "KOINON-WRITE notes.txt please."
	wroteReply    = "Wrote notes.txt."
	permitMarker  = "KOINON-PERMIT"
	permitPrompt  = permitMarker + " the guarded tool call, please."
	permitReply   = "Done with the guarded tool call."
	recallPrompt  = "What did you say first?"
	unknownSessID = "ses_koinon_contract_unknown"
)

// contractScript is the fake model's side of the contract. Replies to tool results are told
// apart by the prompt that started the turn, so the scripted tool calls may use the same tool.
func contractScript(f flavor) fakemodel.Script {
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
	turns = append(turns, fakemodel.Turn{Text: helloReply})
	return fakemodel.Script{Model: contractModel, Turns: turns}
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
				c := exec.CommandContext(ctx, "docker", append([]string{"exec", sb.name}, args...)...) //nolint:gosec // test-controlled
				out, err := c.CombinedOutput()
				var exit *exec.ExitError
				Expect(errors.As(err, &exit)).To(BeTrue(), "%v: %s", args, out)
				Expect(exit.ExitCode()).To(Equal(64), "%v: %s", args, out)
			}
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
			var err error
			session, err = h.conn.NewSession(step(), workdir)
			Expect(err).NotTo(HaveOccurred())
			Expect(session).NotTo(BeEmpty())
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
	for _, f := range flavors {
		describeContract(f)
	}
	return true
}()

// ensureImage skips the flavor's specs when its image is missing, or builds it when
// KOINON_FLAVOR_BUILD=1.
func ensureImage(f flavor) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	img := f.image()
	if exec.CommandContext(ctx, "docker", "image", "inspect", img).Run() == nil { //nolint:gosec // test-controlled
		return
	}
	if os.Getenv("KOINON_FLAVOR_BUILD") != "1" {
		Skip("image " + img + " not found; run `make flavor-" + f.name + "` or set KOINON_FLAVOR_BUILD=1")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	Expect(err).NotTo(HaveOccurred())
	out, err := exec.CommandContext(ctx, "docker", "buildx", "build", "--load", "--tag", img, //nolint:gosec // test-controlled
		filepath.Join(root, f.dockerfile)).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build %s: %s", img, out)
}
