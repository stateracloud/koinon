// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	"github.com/stateracloud/koinon/test/e2e/e2eclient"
	"github.com/stateracloud/koinon/test/scenarios"
)

// The security proof of the Substrate leg (issue #1428). Both cases run in the actor of a real run of the
// quickstart's agent, through the harness's own shell tool: the fake model script (repo-helper.yaml) runs a
// command and lets the run go on only when the command printed its marker. So a check that fails leaves no
// turn that matches, the model answers 400 and the run fails with the command's output in its transcript.

// substrateTaskRun starts a task of agentID with message and returns its run and task IDs and a watcher.
func substrateTaskRun(ctx context.Context, c *e2eclient.Client, org, agentID, title, message string) (string, string, *watcher) {
	GinkgoHelper()
	res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{OrgId: org, AgentId: agentID, Title: title, Message: message})
	Expect(err).NotTo(HaveOccurred())
	return res.GetRun().GetId(), res.GetTask().GetId(), watchRun(context.WithoutCancel(ctx), c, org, res.GetRun().GetId(), 0)
}

// substrateGVisorE2E proves that the actor of a quickstart run is a gVisor guest with the hardening of the
// guest launcher on (ADR 0075, ADR 0119 D3, ADR 0120 section 4). It reads two sides:
//
//   - the control plane: every WorkerPool of the cluster runs the sandbox class gvisor and nothing else. An
//     actor runs only on a worker of a pool, and a pool has exactly one class (WorkerPool.spec.sandboxClasses,
//     maxItems 1, Substrate v1.0.1 manifests/ate-install/generated/ate.dev_workerpools.yaml), so no actor of
//     this install can run under another runtime. GKE Sandbox calls the control plane's record the more
//     trustworthy check, because a guest can fake what it reports about itself;
//   - the guest, from the agent's own shell in the actor of the run: the kernel release that gVisor reports
//     ends in "-gvisor", and the payload runs as the sandbox user (uid 10001) with no_new_privs, seccomp filter
//     mode 2 and no effective capability. Those are the controls of the launcher, read from the process, not
//     from a setting: execution.substrate.allowUnhardened is an acknowledgement that the chart and the CLI
//     require (charts/koinon/templates/_helpers.tpl, internal/cli/execution.go) and no code path reads it to
//     change what the launcher does.
func substrateGVisorE2E(ctx context.Context, c *e2eclient.Client, kubectl, org, agentID string) {
	GinkgoHelper()
	Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")

	By("the control plane: every WorkerPool runs the gvisor sandbox class only")
	out, err := exec.CommandContext(ctx, kubectl, "get", "workerpools.ate.dev", "-A",
		"-o", `jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}={.spec.sandboxClasses[*].name}{"\n"}{end}`).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get workerpools.ate.dev: %s", out)
	pools := strings.Fields(string(out))
	Expect(pools).NotTo(BeEmpty(), "the chart makes the default WorkerPool")
	for _, p := range pools {
		Expect(p).To(HaveSuffix("=gvisor"), "a pool with another class could run an actor outside gVisor")
	}

	By("the guest: the agent's shell in the actor of a run reads a gVisor kernel and the launcher's controls")
	k := scenarios.PullRequest.SandboxCheck
	runID, _, w := substrateTaskRun(ctx, c, org, agentID, "Sandbox check", k.Prompt+": read the kernel and the process controls.")
	var seq int64
	var kinds []string
	done := w.until(ctx, c, org, runID, 8*time.Minute, &seq, &kinds, func(r *runv1alpha1.Run) bool { return terminal(r.GetState()) })
	Expect(done.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_SUCCEEDED),
		"the script goes on only when the command printed %q (events: %v); the run's transcript has what it read", k.Marker, kinds)
}

// substrateLeakGuardEscapeE2E is the Substrate form of the leak guard escape case (ESCAPE_MCP; the OpenShell
// form is test/escape/exfil_test.go through test/e2e/escape-mcp.sh). The threat is the same: code in the agent's
// sandbox skips the harness's MCP client and sends private text out through the MCP path with a raw client. The
// form differs in two ways, both from Substrate: the client runs in the agent's own actor, because the gateway
// knows an actor by the JWT that atenet-egress adds outside the guest (issue #1489), so a probe sandbox would not
// be this agent; and it runs inside a task, because the driver offers no exec into the actor to the test. Inside
// a task the guard holds a call for an approval where, outside one, it refuses it; the case accepts both, as the
// OpenShell form does. The case denies every approval it made, so nothing it held is ever sent.
//
// Precondition (mcpServerE2E, in step 6b): read_item is auto with sink class none, write_item and fetch_url are
// auto and undeclared (external-high).
func substrateLeakGuardEscapeE2E(ctx context.Context, c *e2eclient.Client, org, agentID, fake string) {
	GinkgoHelper()
	x := scenarios.PullRequest.RawExfil
	By("the same attack from a raw MCP client in the agent's actor is stopped too (Substrate form of ESCAPE_MCP, #1428)")
	all := len(fakeMCPRequests(ctx, fake))
	runID, taskID, w := substrateTaskRun(ctx, c, org, agentID, "Raw MCP exfil", x.Prompt+": read an item, then send it out.")
	var seq int64
	var kinds []string
	r := w.until(ctx, c, org, runID, 8*time.Minute, &seq, &kinds, func(r *runv1alpha1.Run) bool {
		return r.GetState() == runv1alpha1.RunState_RUN_STATE_PARKED || terminal(r.GetState())
	})
	Expect(r.GetState()).NotTo(BeElementOf(runv1alpha1.RunState_RUN_STATE_FAILED, runv1alpha1.RunState_RUN_STATE_CANCELED),
		"the script goes on only when the client printed %q: the guard stopped both calls (events: %v)", x.Marker, kinds)

	By("every call the guard held is the leak guard's, on write_item or fetch_url; the approver denies each one")
	list, err := c.Approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: org, TicketId: taskID})
	Expect(err).NotTo(HaveOccurred())
	Expect(len(list.GetApprovals())).To(BeNumerically("<=", 2))
	for _, a := range list.GetApprovals() {
		d, err := c.Approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())
		ap := d.GetApproval()
		Expect(ap.GetConnector()).To(Equal("mcp"))
		Expect(ap.GetTool()).To(BeElementOf(x.WriteTool, x.FetchTool))
		Expect(ap.GetMcp().GetLeakGuard()).NotTo(BeNil(), "the approval is the leak guard's")
		if ap.GetStatus() == approvalv1alpha1.Status_STATUS_PENDING {
			_, err = c.Approvals.Deny(ctx, &approvalv1alpha1.DenyRequest{OrgId: org, ApprovalId: ap.GetId()})
			Expect(err).NotTo(HaveOccurred())
		}
	}
	if r.GetState() == runv1alpha1.RunState_RUN_STATE_PARKED {
		Expect(list.GetApprovals()).NotTo(BeEmpty(), "the run parked on the guard's approvals")
		r = w.until(ctx, c, org, runID, 5*time.Minute, &seq, &kinds, func(r *runv1alpha1.Run) bool { return terminal(r.GetState()) })
	}
	Expect(r.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_SUCCEEDED), "events: %v", kinds)

	By("nothing the guard stopped reached the fake server")
	got := fakeMCPRequests(ctx, fake)[all:]
	Expect(slices.ContainsFunc(got, func(r fakeMCPRecord) bool { return r.Tool == "read_item" && strings.Contains(r.Args, x.Canary) })).
		To(BeTrue(), "the positive control: the raw client's read reached the server: %+v", got)
	for _, r := range got {
		Expect(r.Tool).NotTo(BeElementOf(x.WriteTool, x.FetchTool), "a write_item or fetch_url here means the guard was bypassed")
		Expect(r.Args).NotTo(ContainSubstring(x.Attacker))
	}
}

// substrateEscapeMCP reports whether the run asked for the Substrate form of the leak guard escape case:
// test/e2e/run.sh sets KOINON_E2E_ESCAPE_SUBSTRATE=1 with DRIVER=substrate and ESCAPE_MCP=1.
func substrateEscapeMCP() bool {
	return substrateDriver() && os.Getenv("KOINON_E2E_ESCAPE_SUBSTRATE") == "1"
}
