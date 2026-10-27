// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
)

// update rewrites the golden files: go test ./internal/agentd -update.
var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got with testdata/<name>.golden.
func golden(name, got string) {
	GinkgoHelper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		Expect(os.WriteFile(path, []byte(got), 0o644)).To(Succeed())
	}
	want, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred(), "run go test ./internal/agentd -update to create it")
	Expect(got).To(Equal(string(want)))
}

// fixedID is a stable ID for prompt text: the prompt builders take rows, so IDs are inputs.
func fixedID(n byte) pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.UUID{15: n}, Valid: true}
}

func question(n byte, state store.QuestionState, answer string) store.Question {
	return store.Question{
		ID: fixedID(n), Question: "Which region should I deploy to?", State: state,
		Answer: pgtype.Text{String: answer, Valid: true},
	}
}

func approval(n byte, status store.ApprovalStatus, result, errText, note string) store.ApprovalRequest {
	return store.ApprovalRequest{
		ID: fixedID(n), Tool: "create_pull_request", Target: "acme/widgets", Status: status,
		Result: []byte(result), Error: pgtype.Text{String: errText, Valid: errText != ""},
		DecisionNote: pgtype.Text{String: note, Valid: note != ""},
	}
}

var _ = Describe("Prompt shapes (golden)", func() {
	It("a question the person answered", func() {
		golden("outcomes_question_answered", agentd.RenderOutcomes([]store.Question{question(1, store.QuestionStateAnswered, "us-east-1")}, nil))
	})
	It("a question the person dismissed", func() {
		golden("outcomes_question_dismissed", agentd.RenderOutcomes([]store.Question{question(2, store.QuestionStateDismissed, "eu-west-1")}, nil))
	})
	It("a question that expired, with the no-answer run's instruction", func() {
		golden("outcomes_question_expired", agentd.RenderOutcomes([]store.Question{question(3, store.QuestionStateExpired, "eu-west-1")}, nil))
	})
	It("a question still open is left out", func() {
		Expect(agentd.RenderOutcomes([]store.Question{question(4, store.QuestionStateOpen, "")}, nil)).To(BeEmpty())
	})
	It("every approval outcome", func() {
		golden("outcomes_approvals", agentd.RenderOutcomes(nil, []store.ApprovalRequest{
			approval(10, store.ApprovalStatusExecuted, `{"url":"https://github.test/acme/widgets/pull/1"}`, "", ""),
			approval(11, store.ApprovalStatusFailed, "", "branch exists", ""),
			approval(12, store.ApprovalStatusDenied, "", "", "touch only the docs"),
			approval(13, store.ApprovalStatusDenied, "", "", ""),
			approval(14, store.ApprovalStatusExpired, "", "", ""),
		}))
	})
	It("questions and approvals together", func() {
		golden("outcomes_mixed", agentd.RenderOutcomes(
			[]store.Question{question(5, store.QuestionStateAnswered, "us-east-1")},
			[]store.ApprovalRequest{approval(15, store.ApprovalStatusDenied, "", "", "no")}))
	})
	It("the resume after a park", func() {
		out := agentd.RenderOutcomes([]store.Question{question(1, store.QuestionStateAnswered, "us-east-1")}, nil)
		golden("resume_park", agentd.ParkResumeNote+out)
	})
	It("the resume after a lost connection", func() {
		golden("resume_interrupted", agentd.ResumeNote)
	})
	It("the retry after a temporary model error", func() {
		golden("retry", agentd.RetryNote("rate_limited"))
	})
	It("the handoff of a fresh session", func() {
		out := agentd.RenderOutcomes(nil, []store.ApprovalRequest{approval(10, store.ApprovalStatusDenied, "", "", "no")})
		golden("handoff", agentd.BuildHandoff("Fix the typo", "[Koinon] MCP notice.", "Notes from the last turn.",
			[]string{"First reply.", "Second reply."}, []string{"Please fix the typo.", "Also the footer."}, agentd.ParkResumeNote+out))
	})
})
