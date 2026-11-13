// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/testing/testenv"
	"github.com/stateracloud/koinon/test/scenarios"
)

// bash5 returns a bash 5 or newer: the scripts of hack/ need one (ADR 0090) and the system bash of macOS is 3.2.
func bash5() (string, error) {
	cands := []string{"bash", "/opt/homebrew/bin/bash", "/usr/local/bin/bash"}
	for _, c := range cands {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(context.Background(), path, "-c", `echo "${BASH_VERSINFO[0]}"`).Output()
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(string(bytes.TrimSpace(out))); err == nil && n >= 5 {
			return path, nil
		}
	}
	return "", errors.New("no bash 5 or newer on PATH")
}

// pendingCodeHostApproval records the create_pull_request call of the repo-helper-ask scenario on a code host, with the
// target the real tool server builds (githubtools.Target; its own spec pins what the server stores).
func (e *env) pendingCodeHostApproval(ctx context.Context, f orgFixture, connector, repo string) store.ApprovalRequest {
	GinkgoHelper()
	pr := githubtools.PullRequestArgs{
		Branch: "koinon/add-hello-md", Title: "Add hello.md", Body: "Adds hello.md as requested in the task.",
		Files: []githubtools.FileChange{{Path: "hello.md", Content: "hello from koinon\n"}},
	}
	args, err := githubtools.Canonical(pr)
	Expect(err).NotTo(HaveOccurred())
	risk, err := json.Marshal(githubtools.AssessRisk(pr))
	Expect(err).NotTo(HaveOccurred())
	row, err := store.New(e.pool).CreateApprovalRequest(ctx, store.CreateApprovalRequestParams{
		OrgID: uuid(f.org), TaskID: uuid(f.ticket.GetId()), RunID: uuid(f.run.GetId()), AgentID: uuid(f.agent.GetId()),
		Connector: connector, Tool: scenarios.GiteaAsk.PRTool, Target: githubtools.Target(connector, repo),
		ArgsJson: string(args), ArgsSha256: githubtools.HashArgs(args), Risk: risk,
		Decision: store.ApprovalDecisionAsk, Status: store.ApprovalStatusPending,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	Expect(err).NotTo(HaveOccurred())
	return row
}

// The scripted approver of the real-model runs (#967): hack/answer-question.sh and hack/approve-pr.sh run against
// this package's real API (the services and the auth in front of them, on Postgres), not a stand-in, so a change to
// the JSON the server answers with or to the target format fails here. hack/answer-question-test.sh and
// hack/approve-pr-test.sh are the fast tests of the scripts' own checks against a fake API.
var _ = Describe("the scripted approver (hack/answer-question.sh, hack/approve-pr.sh)", func() {
	var (
		e    *env
		f    orgFixture
		bash string
		jar  string
		user string
	)

	BeforeEach(func(ctx SpecContext) {
		testenv.Require(testenv.Binary("curl"), testenv.Binary("jq"))
		var err error
		if bash, err = bash5(); err != nil {
			if testenv.Required(os.Getenv(testenv.RequireVar), "bash5") {
				Fail(testenv.Message("bash5", err))
			}
			Skip(testenv.Message("bash5", err))
		}
		// The scripts send the session cookie by hand, as they do through the drill's plain-HTTP port-forward.
		e = newEnv(ctx, envOptions{plainHTTP: true})
		f = e.newOrgFixture(ctx)
		Expect(e.pool.QueryRow(ctx, `SELECT username FROM local_credentials WHERE user_id = $1`, f.owner.userID).Scan(&user)).To(Succeed())
		jar = filepath.Join(GinkgoT().TempDir(), "jar")
	})

	script := func(ctx context.Context, extra []string, name string, args ...string) (string, int) {
		GinkgoHelper()
		cmd := exec.CommandContext(ctx, bash, append([]string{filepath.Join("..", "..", "hack", name)}, args...)...)
		cmd.Env = append(os.Environ(), "KOINON_APPROVER_USER="+user, "KOINON_APPROVER_PASSWORD="+fixturePassword, "KOINON_APPROVER_WAIT=0")
		cmd.Env = append(cmd.Env, extra...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return out.String(), 0
		case errors.As(err, &ee):
			return out.String(), ee.ExitCode()
		default:
			Expect(err).NotTo(HaveOccurred())
			return "", -1
		}
	}

	It("answers the one open question of a parked run by its run and question ID, and the run resumes", func(ctx SpecContext) {
		g := scenarios.GiteaAsk
		q := e.openQuestion(ctx, f)
		ad := e.agentd("agentd-script")
		run := ad.mustClaim(ctx, f.org)
		_, err := ad.q.ParkRun(ctx, store.ParkRunParams{OrgID: run.OrgID, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonInputRequired})
		Expect(err).NotTo(HaveOccurred())

		By("a choice the question does not offer is refused and changes nothing")
		// The scenario's own answer is not one of this fixture's choices (post, do not post).
		out, code := script(ctx, []string{"KOINON_ANSWER_CHOICE=" + g.Answer}, "answer-question.sh", e.srv.URL, jar, f.org, f.run.GetId(), q.ID.String())
		Expect(code).To(Equal(1), out)
		Expect(out).To(ContainSubstring("no such choice"))

		By("a question of another run is not found")
		post := []string{"KOINON_ANSWER_CHOICE=post"}
		out, code = script(ctx, post, "answer-question.sh", e.srv.URL, jar, f.org, "00000000-0000-0000-0000-000000000000", q.ID.String())
		Expect(code).To(Equal(3), out)

		By("the choice of the plan answers it")
		out, code = script(ctx, post, "answer-question.sh", e.srv.URL, jar, f.org, f.run.GetId(), q.ID.String())
		Expect(code).To(Equal(0), out)
		Expect(out).To(ContainSubstring("answered " + q.ID.String()))
		got, err := f.owner.questions.ListQuestions(ctx, &questionv1alpha1.ListQuestionsRequest{OrgId: f.org, TaskId: f.ticket.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetQuestions()).To(HaveLen(1))
		Expect(got.GetQuestions()[0].GetState()).To(Equal(questionv1alpha1.QuestionState_QUESTION_STATE_ANSWERED))
		Expect(got.GetQuestions()[0].GetAnswer()).To(Equal("post"))
		Expect(got.GetQuestions()[0].GetAnswerKind()).To(Equal(questionv1alpha1.AnswerKind_ANSWER_KIND_CHOICE))
		resumed, err := ad.q.GetRun(ctx, store.GetRunParams{OrgID: run.OrgID, ID: run.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(resumed.ReadyAt.Valid).To(BeTrue(), "the run can be claimed again")

		By("it was open once: the same script finds no open question now")
		_, code = script(ctx, post, "answer-question.sh", e.srv.URL, jar, f.org, f.run.GetId(), q.ID.String())
		Expect(code).To(Equal(3))
	})

	It("approves the pending pull request of a gitea: target, and leaves the same repository on another host alone", func(ctx SpecContext) {
		g := scenarios.GiteaAsk
		gitea := e.pendingCodeHostApproval(ctx, f, g.Connector, "acme/widgets")
		github := e.pendingCodeHostApproval(ctx, f, "github", "acme/widgets")
		Expect(gitea.Target).To(Equal("gitea:acme/widgets"))

		out, code := script(ctx, nil, "approve-pr.sh", e.srv.URL, jar, f.org, "gitea:acme/other")
		Expect(code).To(Equal(3), out)

		out, code = script(ctx, nil, "approve-pr.sh", e.srv.URL, jar, f.org, "gitea:acme/widgets")
		Expect(code).To(Equal(0), out)
		Expect(out).To(ContainSubstring(fmt.Sprintf("approved %s (gitea:acme/widgets)", gitea.ID.String())))

		// Approve starts the execution at once, and this env has no code-host connection to execute on, so the
		// approval may already be failed: what matters here is that it was approved, not denied or left pending.
		decided := []approvalv1alpha1.Status{approvalv1alpha1.Status_STATUS_APPROVED, approvalv1alpha1.Status_STATUS_EXECUTED, approvalv1alpha1.Status_STATUS_FAILED}
		status := func(id pgtype.UUID) approvalv1alpha1.Status {
			res, err := f.owner.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: f.org, ApprovalId: id.String()})
			Expect(err).NotTo(HaveOccurred())
			return res.GetApproval().GetStatus()
		}
		Expect(status(gitea.ID)).To(BeElementOf(decided), "the gitea pull request was approved")
		Expect(status(github.ID)).To(Equal(approvalv1alpha1.Status_STATUS_PENDING), "the github one was not")

		By("a bare owner/name still means github:")
		out, code = script(ctx, nil, "approve-pr.sh", e.srv.URL, jar, f.org, "acme/widgets")
		Expect(code).To(Equal(0), out)
		Expect(status(github.ID)).To(BeElementOf(decided))
	})
})
