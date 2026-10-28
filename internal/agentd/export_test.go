// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Endpoints and LaunchCommand expose the flavor launch commands to the external test package.
type Endpoints = endpoints

var LaunchCommand = launchCommand

// ClassifyError and StopClass expose the failure classification to the external test package.
func ClassifyError(err error) (class string, retryAfter time.Duration) {
	c := classifyPromptError(err)
	return string(c.class), c.retryAfter
}

// ClassifyGateway reports whether the error is a gateway refusal (401 or 429).
func ClassifyGateway(err error) bool { return classifyPromptError(err).gateway }

func StopClass(stop acp.StopReason) string { return string(stopFailure(stop).class) }

// RenderOutcomes, BuildHandoff and the notes expose the prompt shapes to the golden tests.
func RenderOutcomes(qs []store.Question, reqs []store.ApprovalRequest) string {
	text, _, _ := renderOutcomes(qs, reqs)
	return text
}

func BuildHandoff(title, notice, summary string, replies, messages []string, outcomes string) string {
	return handoff{Title: title, Notice: notice, Summary: summary, Replies: replies, Messages: messages, Outcomes: outcomes}.build()
}

// AssemblePrompt exposes the pure prompt assembly to the golden tests; mode is one of the
// Prompt* constants.
func AssemblePrompt(mode PromptMode, title, notice string, msgs []string, outcomes string) string {
	return assemblePrompt(mode, title, notice, msgs, outcomes)
}

type PromptMode = promptMode

const (
	PromptNew        = promptNew
	PromptContinue   = promptContinue
	PromptTakeover   = promptTakeover
	PromptParkResume = promptParkResume
	PromptReset      = promptReset
)

const (
	ParkResumeNote = parkResumeNote
	ResumeNote     = resumeNote
)

func RetryNote(class string) string { return fmt.Sprintf(retryNote, class) }

// InstructionsFile exposes the instruction file's content for a version's instructions to the
// golden tests.
func InstructionsFile(instructions, cwd string, repos ...Repository) string {
	return string(instructionsFile(instructions, cwd, repos))
}

// Repository is a granted repository as the instruction file lists it.
type Repository = repository

// RefusalNote and Refusal expose the prompt that continues a turn after refused requests.
type Refusal = refusal

func RefusalNote(refs []Refusal, cwd string) string { return refusalNote(refs, cwd) }
