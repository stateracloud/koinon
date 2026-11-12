// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// Endpoints and LaunchCommand expose the flavor launch commands to the external test package.
type Endpoints = endpoints

var LaunchCommand = launchCommand

// CompactionTokens and DefaultCompactionTokens expose the compaction point to the external test package.
var CompactionTokens = compactionTokens

const DefaultCompactionTokens = defaultCompactionTokens

// RetryPolicy is the model-retry policy a retrying harness is launched with.
type RetryPolicy = retryPolicy

// DefaultRetryPolicy is the policy of a Config with the defaults.
func DefaultRetryPolicy() RetryPolicy {
	return retryPolicyOf(Config{ModelRetries: DefaultModelRetries, RetryBackoff: DefaultRetryBackoff, MaxRetryAfter: DefaultMaxRetryAfter})
}

// OwnRetryLayer makes the pool repeat failed turns itself even for a flavor whose harness retries
// model requests, so specs can exercise agentd's own retry layer with the fake harness, which
// does not retry.
func OwnRetryLayer(c *Config) { c.ownRetryLayer = true }

// ModelOption exposes the model value agentd selects for the session.
var ModelOption = modelOption

// ClassifyError and StopClass expose the failure classification to the external test package.
func ClassifyError(err error) (class string, retryAfter time.Duration) {
	c := classifyPromptError(err)
	return string(c.class), c.retryAfter
}

// ClassifyGateway reports whether the error is a gateway refusal (401 or 429).
func ClassifyGateway(err error) bool { return classifyPromptError(err).gateway }

// RefusedByProvider applies the gateway's record of a provider refusal to the failure the harness's
// error classifies as, and returns the class, whether it still counts as a gateway refusal, and the
// reason.
func RefusedByProvider(err error, status int, providerType string) (class string, gateway bool, reason string) {
	f := classFailure("session/prompt", err)
	f = refusedByProvider(f, providerRefusal{status: status, providerType: providerType})
	return string(f.class), f.gateway, f.reason
}

// IsAuthLike reports whether the error is one the gateway's record can improve.
func IsAuthLike(err error) bool { return classFailure("session/prompt", err).authLike }

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
	return string(instructionsFile(store.AgentFlavorOpencode, instructions, cwd, repos, false))
}

// InstructionsFileWithWeb is InstructionsFile for an agent with web egress.
func InstructionsFileWithWeb(instructions, cwd string, repos ...Repository) string {
	return string(instructionsFile(store.AgentFlavorOpencode, instructions, cwd, repos, true))
}

// Repository is a granted repository as the instruction file lists it.
type Repository = repository

// RefusalNote and Refusal expose the prompt that continues a turn after refused requests.
type Refusal = refusal

func RefusalNote(refs []Refusal, cwd string) string { return refusalNote(refs, cwd) }

// OutcomeMetrics is what the run owner counts with (internal/runoutcome).
func OutcomeMetrics(m *Metrics) runoutcome.Metrics { return outcomeMetrics{m} }

// NewTaintCalls returns the leak guard feed's view of one run's tool calls (taint.go): it takes
// each session update as agentd marshals it and returns what a finished call reports.
func NewTaintCalls() func(update []byte) (mcpguard.Legs, []string, bool) {
	return NewTaintCallsFor("")
}

// NewTaintCallsFor is NewTaintCalls for a flavor whose MCP tool calls have the title prefix.
func NewTaintCallsFor(mcpPrefix string) func(update []byte) (mcpguard.Legs, []string, bool) {
	t := &taintCalls{}
	t.setMCPPrefix(mcpPrefix)
	return t.finished
}

// FeedCounts reports whether the leak guard's feed counts an update as a finished tool call, once
// as the reader reads the notification line and once as the handler gets the update.
func FeedCounts(line []byte) (read, handled bool) {
	var m struct {
		Params struct {
			Update json.RawMessage `json:"update"`
		} `json:"params"`
	}
	_ = json.Unmarshal(line, &m)
	return lineFinishesTool(line), finishesTool(m.Params.Update)
}

// WaitProcess is the bounded wait for a harness that was disconnected.
var WaitProcess = waitProcess

var LineTooLongReason = lineTooLongReason

// WorkingWithData exposes the "Working with large data" section for a flavor's tools, which
// need not belong to a shipped flavor.
func WorkingWithData(tools ...string) string { return workingWithDataFor(tools, false) }

// WorkingWithDataSaved is WorkingWithData for a harness that saves a cut tool result to a file.
func WorkingWithDataSaved(tools ...string) string { return workingWithDataFor(tools, true) }
