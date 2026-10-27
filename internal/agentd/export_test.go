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

func StopClass(stop acp.StopReason) string { return string(stopFailure(stop).class) }

// RenderOutcomes, BuildHandoff and the notes expose the prompt shapes to the golden tests.
func RenderOutcomes(qs []store.Question, reqs []store.ApprovalRequest) string {
	text, _, _ := renderOutcomes(qs, reqs)
	return text
}

func BuildHandoff(title, notice, summary string, replies, messages []string, outcomes string) string {
	return handoff{Title: title, Notice: notice, Summary: summary, Replies: replies, Messages: messages, Outcomes: outcomes}.build()
}

const (
	ParkResumeNote = parkResumeNote
	ResumeNote     = resumeNote
)

func RetryNote(class string) string { return fmt.Sprintf(retryNote, class) }
