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
	name         string
	defaultImage string
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
}

func (f flavor) image() string {
	if img := os.Getenv("KOINON_FLAVOR_IMAGE_" + strings.ToUpper(f.name)); img != "" {
		return img
	}
	return f.defaultImage
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
			{Text: helloReply},
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

		It("initializes and advertises session/load", func() {
			init, err := h.conn.Initialize(step())
			Expect(err).NotTo(HaveOccurred())
			Expect(init.ProtocolVersion).To(BeNumerically("==", acp.ProtocolVersion))
			Expect(init.AgentCapabilities.LoadSession).To(BeTrue())
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
			Expect(gw.mcpMethods()).To(ContainElements("initialize", "tools/list"))
			Expect(gw.modelBodies()).To(ContainElement(And(
				ContainSubstring(contractModel), ContainSubstring(helloPrompt), ContainSubstring(f.mcpToolName),
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
