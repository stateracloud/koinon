// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/test/e2e/e2eclient"
)

// The clone through koinon-gateway's egress listener (ADR 0097, ADR 0120 sections 1 and 2, #1086 part C2).
//
// The install runs gateway.mcp.driver=koinon with gateway.mcp.koinon.egress.enabled (test/e2e/kind/
// koinon-values.yaml). The fake Gitea (test/e2e/kind/fakes.yaml) serves git over HTTPS with the run CA and
// accepts only its tokens. The agent's sandbox holds no token: koinon-agentd writes the git prefix
// (url.<gateway>/egress/<host>/.insteadOf) and a short-lived actor token, and the gateway attaches the
// connection's read token. So a git-upload-pack that the fake answered 200 with a credential is the
// gateway's work: the sandbox could not send one.
const (
	fakeGiteaURL  = "https://fakegitea.koinon-e2e-fakes.svc.cluster.local"
	fakeGiteaRepo = "koinon-demo/hello"
	// The fake's tokens (test/e2e/kind/fakes.yaml): obviously fake, never a real credential.
	fakeGiteaReadToken  = "koinon_test_not-a-real-gitea-read-token"
	fakeGiteaWriteToken = "fake-gitea-token"
)

// fakeGitRequest is one git smart HTTP request the fake Gitea served (cmd/koinon-fakegitea, GET /_fake/git).
type fakeGitRequest struct {
	Method, Path, Service string
	Status                int
	Auth, ActorToken      bool
}

// fakeGiteaGit reads the fake Gitea's git requests through the API server's service proxy, so the suite
// needs no port-forward. The service proxy does not check the fake's certificate.
func fakeGiteaGit(ctx context.Context, kubectl string) []fakeGitRequest {
	GinkgoHelper()
	out, err := exec.CommandContext(ctx, kubectl, "get", "--raw",
		"/api/v1/namespaces/koinon-e2e-fakes/services/https:fakegitea:443/proxy/_fake/git").Output()
	Expect(err).NotTo(HaveOccurred(), "GET /_fake/git of the fake Gitea")
	var got []fakeGitRequest
	Expect(json.Unmarshal(out, &got)).To(Succeed(), string(out))
	return got
}

func egressCloneE2E(ctx context.Context, c *e2eclient.Client, org string) {
	GinkgoHelper()
	kubectl := os.Getenv("KOINON_E2E_KUBECTL")
	Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")

	By("the install runs koinon-gateway's egress listener, and koinon-agentd has the egress URL")
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "deployment", "koinon-mcp-gateway",
		"-o", "jsonpath={.spec.template.spec.containers[*].args}").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get deployment koinon-mcp-gateway: %s", out)
	Expect(string(out)).To(ContainSubstring("--egress-addr="), "the kind install turns the egress listener on")
	out, err = exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "deployment", "koinon-agentd",
		"-o", `jsonpath={.spec.template.spec.containers[*].env[?(@.name=="KOINON_EGRESS_URL")].value}`).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get deployment koinon-agentd: %s", out)
	Expect(strings.TrimSpace(string(out))).To(HavePrefix("http://koinon-mcp-gateway."))

	By("a Gitea connection with the fake's tokens and the run CA")
	ca, err := os.ReadFile(os.Getenv("KOINON_E2E_FAKEMCP_CA"))
	Expect(err).NotTo(HaveOccurred(), "KOINON_E2E_FAKEMCP_CA: the run CA, which also signs the fake Gitea")
	created, err := c.Connections.CreateGiteaConnection(ctx, &connectionv1alpha1.CreateGiteaConnectionRequest{
		OrgId: org, DisplayName: "Fake Gitea", BaseUrl: fakeGiteaURL,
		ReadToken: fakeGiteaReadToken, WriteToken: fakeGiteaWriteToken, CaBundle: string(ca), Test: true,
	})
	Expect(err).NotTo(HaveOccurred())
	conn := created.GetConnection()
	Expect(conn.GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_OK), "the connection test")

	By("an agent from repo-helper-gitea, bound to " + fakeGiteaRepo)
	ts, err := c.Templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
	Expect(err).NotTo(HaveOccurred())
	var tmpl string
	for _, tp := range ts.GetTemplates() {
		if tp.GetSlug() == "repo-helper-gitea" {
			tmpl = tp.GetId()
		}
	}
	Expect(tmpl).NotTo(BeEmpty(), "the repo-helper-gitea template")
	ag, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
		OrgId: org, TemplateId: tmpl, DisplayName: "Gitea clone", Harness: harness.OpenCode,
		Grants: []*agentv1alpha1.GrantBinding{{Connector: "gitea", ConnectionId: conn.GetId(), Repositories: []string{fakeGiteaRepo}}},
	})
	Expect(err).NotTo(HaveOccurred())
	act, err := c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
		OrgId: org, AgentId: ag.GetAgent().GetId(), Version: ag.GetAgent().GetLatestVersion(),
	})
	Expect(err).NotTo(HaveOccurred())
	agent := act.GetAgent()
	Expect(agent.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE))

	By("a task whose agent runs git clone in its sandbox (test/fakemodel/scripts/repo-helper.yaml, \"egress clone\")")
	before := len(fakeGiteaGit(ctx, kubectl))
	res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
		OrgId: org, AgentId: agent.GetId(), Title: "Egress clone",
		Message: "egress clone: clone " + fakeGiteaRepo + " and change nothing.",
	})
	Expect(err).NotTo(HaveOccurred())
	var seq int64
	var kinds []string
	w := watchRun(context.WithoutCancel(ctx), c, org, res.GetRun().GetId(), 0)
	done := w.until(ctx, c, org, res.GetRun().GetId(), 8*time.Minute, &seq, &kinds, func(r *runv1alpha1.Run) bool {
		return terminal(r.GetState())
	})
	// Read the fake's requests first: when the run fails, their statuses say where the clone stopped.
	got := fakeGiteaGit(ctx, kubectl)
	Expect(done.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_SUCCEEDED), "events: %v; the fake Gitea's git requests: %+v", kinds, got[min(before, len(got)):])

	By("the fake Gitea served the clone's git-upload-pack with a credential it accepted, and without the actor token")
	Expect(len(got)).To(BeNumerically(">=", before))
	clone := got[before:]
	Expect(clone).To(ContainElement(fakeGitRequest{
		Method: http.MethodPost, Path: "/" + fakeGiteaRepo + ".git/git-upload-pack", Status: http.StatusOK, Auth: true,
	}), "the clone reached the fake through the gateway: %+v", clone)
	for _, r := range clone {
		Expect(r.Path).To(HavePrefix("/"+fakeGiteaRepo+".git/"), "git reached only the bound repository")
		Expect(r.Service).NotTo(Equal("git-receive-pack"), "the agent pushed nothing")
		Expect(r.ActorToken).To(BeFalse(), "the gateway removes the actor token: the request came through it")
	}
}
