// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/flavor"
	"github.com/stateracloud/koinon/internal/triggers"
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
		golden("resume_park", agentd.AssemblePrompt(agentd.PromptParkResume, "Fix the typo", "", nil, out))
	})
	It("the resume after a lost connection", func() {
		golden("resume_interrupted", agentd.AssemblePrompt(agentd.PromptTakeover, "Fix the typo", "", nil, ""))
	})
	It("the resume after a takeover with a message that arrived since", func() {
		golden("resume_takeover_message", agentd.AssemblePrompt(agentd.PromptTakeover, "Fix the typo", "", []string{"Also the footer."}, ""))
	})
	It("a new task", func() {
		golden("prompt_new_task", agentd.AssemblePrompt(agentd.PromptNew, "Fix the typo", "[Koinon] MCP notice.",
			[]string{"Please fix the typo in the README."}, ""))
	})
	It("a follow-up message in a session that knows the task", func() {
		golden("prompt_follow_up", agentd.AssemblePrompt(agentd.PromptContinue, "Fix the typo", "", []string{"Also the footer."}, ""))
	})
	It("a scheduled task, titled from the due time in the trigger's time zone", func() {
		loc, err := time.LoadLocation("Europe/Tallinn")
		Expect(err).NotTo(HaveOccurred())
		due := time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC)
		golden("prompt_scheduled_task", agentd.AssemblePrompt(agentd.PromptNew, triggers.ScheduleTaskTitle("morning-triage", due, loc), "[Koinon] MCP notice.",
			[]string{"Triage the new issues and summarise them for the team."}, ""))
	})
	It("a reset session with the outcomes after the messages", func() {
		out := agentd.RenderOutcomes([]store.Question{question(1, store.QuestionStateAnswered, "us-east-1")}, nil)
		golden("prompt_reset_outcomes", agentd.AssemblePrompt(agentd.PromptReset, "Fix the typo", "[Koinon] MCP notice.",
			[]string{"Please fix the typo.", "Also the footer."}, out))
	})
	It("a prompt with nothing to say", func() {
		golden("prompt_empty", agentd.AssemblePrompt(agentd.PromptContinue, "Fix the typo", "", nil, ""))
	})
	It("the retry after a temporary model error", func() {
		golden("retry", agentd.RetryNote("rate_limited"))
	})
	It("the handoff of a fresh session", func() {
		out := agentd.RenderOutcomes(nil, []store.ApprovalRequest{approval(10, store.ApprovalStatusDenied, "", "", "no")})
		golden("handoff", agentd.BuildHandoff("Fix the typo", "[Koinon] MCP notice.", "Notes from the last turn.",
			[]string{"First reply.", "Second reply."}, []string{"Please fix the typo.", "Also the footer."}, out))
	})
	It("the instruction file of an agent version", func() {
		golden("instructions_file", agentd.InstructionsFile("You help with the task.", "/sandbox/work"))
	})
	It("the instruction file of an agent with web egress has no web section", func() {
		Expect(agentd.InstructionsFileWithWeb("You help with the task.", "/sandbox/work")).NotTo(ContainSubstring("## Web access"))
		Expect(agentd.InstructionsFile("You help with the task.", "/sandbox/work")).To(ContainSubstring("MCP tools"))
	})
	It("the instruction file of an agent with repositories on a Gitea host", func() {
		golden("instructions_file_repositories", agentd.InstructionsFile("You help with the task.", "/sandbox/work",
			agentd.Repository{
				Name: "acme/infra", Connector: "gitea", Provider: "Gitea", Host: "git.example.test:3000",
				CloneURL: "https://git.example.test:3000/acme/infra",
			},
			agentd.Repository{
				Name: "acme/docs", Connector: "gitea", Provider: "Gitea", Host: "git.example.test:3000",
				CloneURL: "https://git.example.test:3000/acme/docs",
			}))
	})
	It("the note after the harness cancelled a turn on refused requests", func() {
		golden("refusal_note", agentd.RefusalNote([]agentd.Refusal{
			{Kind: "edit", Title: "write", Outside: "/sandbox/.koinon/notes/external/report.md"},
			{Kind: "fetch", Title: "webfetch"},
		}, "/sandbox/work"))
	})
	It("the section on large data names only the tools the flavor declares", func() {
		d, ok := flavor.Lookup(store.AgentFlavorOpencode)
		Expect(ok).To(BeTrue())
		Expect(agentd.InstructionsFile("x", "/sandbox/work")).To(ContainSubstring(agentd.WorkingWithDataSaved(d.Tools...)))

		some := agentd.WorkingWithData("jq", "yq")
		Expect(some).To(ContainSubstring("`jq`; YAML"))
		Expect(some).NotTo(ContainSubstring("duckdb"))
		Expect(some).NotTo(ContainSubstring("python3"))
	})
	It("the saved-output line needs the harness property, not only the tools", func() {
		tools := []string{"rg", "jq", "duckdb"}
		Expect(agentd.WorkingWithDataSaved(tools...)).To(ContainSubstring("full output saved to FILE"))
		Expect(agentd.WorkingWithData(tools...)).NotTo(ContainSubstring("full output saved to FILE"))
		Expect(agentd.WorkingWithData(tools...)).To(ContainSubstring("Never try to install"))
	})
	It("the section on large data of a flavor with no tools is the neutral form", func() {
		got := agentd.WorkingWithData()
		Expect(got).To(HavePrefix("## Working with large data\n\n- Check size before reading"))
		Expect(got).To(ContainSubstring("Never try to install anything"))
		for _, tool := range []string{"jq", "duckdb", "ast-grep", "sqlite3", "pdftotext", "python3", "`rg`", "git diff"} {
			Expect(got).NotTo(ContainSubstring(tool))
		}
	})
	It("the instruction file of an agent version without instructions", func() {
		golden("instructions_file_empty", agentd.InstructionsFile(" \n", "/sandbox/work"))
	})
})
