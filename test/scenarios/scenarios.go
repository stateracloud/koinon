// SPDX-License-Identifier: Apache-2.0

// Package scenarios holds what the fake model scripts (test/fakemodel/scripts) are expected to
// produce in Koinon, as data and small checks, in one place.
//
// The kind e2e (test/e2e) drives a script through the whole product and compares the outcome with
// these expectations. The owning packages' own tests (internal/githubtools for branch naming,
// internal/ledger for the waiver and estimate rules, internal/agentmodel for the org default)
// assert the same expectations against their logic, so a product rule change fails in its own
// package in seconds instead of in the 25-minute e2e. This package's own test keeps the data
// honest against the scripts themselves.
//
// Nothing here talks to a cluster, a database or a container.
package scenarios

import (
	"fmt"
	"strings"

	"github.com/stateracloud/koinon/internal/githubtools"
)

// PullRequest is the repo-helper flow (script repo-helper.yaml, ADR 0049): the agent proposes a
// pull request, the run parks on the approval, and Koinon opens the pull request once a human
// approves.
var PullRequest = pullRequest{
	Model:      "repo-helper",
	Repository: "koinon-demo/hello",
	Base:       "main",
	// Suggestion is the branch name the fake model proposes. Koinon files it under the agent's
	// namespace: see Branch.
	Suggestion: "koinon/add-hello-md",
	Title:      "Add hello.md",
	File:       "hello.md",
	Content:    "hello from koinon\n",
	// ParkingTool is the tool call that parks the run on an approval; a tool of the same name
	// behind a harness or gateway prefix is the same call.
	ParkingTool: "create_pull_request",
	// TrackerAskTool is the approval-gated tool of the remote MCP server in the MC1 step, as the
	// agent sees it: <org>-<server>-ask_<tool> behind the gateway's target prefix.
	TrackerAskTool:   "default-tracker-ask_write_item",
	TrackerAskTarget: "mcp:tracker",
	TrackerAskArgs:   `{"title":"from the e2e ask"}`,
	// TrackerGuardPrompt starts the leak guard's in-task case (ADR 0091): the agent reads with
	// TrackerReadTool, so the session holds both legs, then calls TrackerGuardTool, an auto-tier
	// tool on the server's own target, which the guard parks for an approval of that exact call.
	TrackerGuardPrompt: "tracker exfil",
	TrackerReadTool:    "default-tracker_read_item",
	TrackerGuardTool:   "default-tracker_write_item",
	TrackerGuardArgs:   `{"title":"from the e2e guard"}`,
	Ledger: Ledger{
		// StreamedInputTokens is what the script's first (tool call) turn reports as input. The
		// streamed requests carry usage, so the run's input tokens reach at least this once the
		// ledger has them.
		StreamedInputTokens: 1200,
		// RefusedStatus is the status of the title request the script refuses (a harness's title
		// request matches no turn: HTTP 400 without usage).
		RefusedStatus: 400,
		// MaxRefused bounds the refused requests: OpenCode asks for a title with each prompt until
		// it has one, so the task's prompt and the end-of-turn note's prompt make at most two.
		MaxRefused: 2,
		// OutputCeiling is above the script's streamed output in the first turns and far below what
		// an estimate for the refused request would charge.
		OutputCeiling: 1000,
	},
}

type pullRequest struct {
	Model, Repository, Base                          string
	Suggestion, Title, File, Content                 string
	ParkingTool                                      string
	TrackerAskTool, TrackerAskTarget, TrackerAskArgs string
	TrackerGuardPrompt, TrackerReadTool              string
	TrackerGuardTool, TrackerGuardArgs               string
	Ledger                                           Ledger
}

// Branch is the branch Koinon files the pull request under for an agent: koinon/<agent
// namespace>/<suggestion> (githubtools.FinalBranch, old issue 281). The e2e and the githubtools tests call
// the same helper, so a naming change is made once and every expectation follows.
func (p pullRequest) Branch(agentDisplayName, agentID string) (string, error) {
	return githubtools.FinalBranch(githubtools.AgentNamespace(agentDisplayName, agentID), p.Suggestion)
}

// Ledger is the shape the ledger has for a run of the scenario (ADR 0056, amendment 2026-10-29).
//
// Two kinds of entry exist. A streamed request carries the provider's usage through the gateway:
// tokens, a cost, and it is not an estimate. The title request the fake model refuses is the
// provider's refusal (4xx without usage): the ledger keeps the entry with the provider's status
// and no tokens and no cost, marks the charge waived (the quickstart's provider type does not bill
// a refusal), and it is not an estimate either (old issue 241, old issue 253).
//
// The two do not arrive in request order: the gateway exports its access log in batches, and the
// refused entry can be ingested before the streamed ones (old issue 291).
type Ledger struct {
	StreamedInputTokens int64
	RefusedStatus       int
	MaxRefused          int
	OutputCeiling       int64
}

// Entry is one spend entry as a test sees it.
type Entry struct {
	// UpstreamStatus is the provider's error status; 0 when the provider answered.
	UpstreamStatus int
	Input, Output  int64
	// CostZero reports a cost of exactly zero.
	CostZero bool
	// ChargeWaived is the recorded reason a refusal was charged nothing; "" when it was not waived.
	ChargeWaived string
	// Estimated reports an entry whose usage the ledger estimated rather than the provider reported.
	Estimated bool
}

// Totals is a run's cost as the API reports it (run.Cost).
type Totals struct {
	Requests, EstimatedRequests int64
	InputTokens, OutputTokens   int64
}

// CheckRefused reports how e differs from the scenario's refused title request, nil when it
// matches: the provider's status kept, no tokens, no cost, waived, not estimated.
func (l Ledger) CheckRefused(e Entry) error {
	var bad []string
	if e.UpstreamStatus != l.RefusedStatus {
		bad = append(bad, fmt.Sprintf("status %d, want the provider's %d", e.UpstreamStatus, l.RefusedStatus))
	}
	if e.ChargeWaived == "" {
		bad = append(bad, "charge not waived")
	}
	if e.Input+e.Output != 0 {
		bad = append(bad, fmt.Sprintf("%d tokens charged", e.Input+e.Output))
	}
	if !e.CostZero {
		bad = append(bad, "cost is not zero")
	}
	if e.Estimated {
		bad = append(bad, "marked as an estimate: a waived refusal is not one")
	}
	return join(bad)
}

// CheckStreamed reports how e differs from a streamed request of the scenario: answered by the
// provider, carrying usage, not an estimate.
func (l Ledger) CheckStreamed(e Entry) error {
	var bad []string
	if e.UpstreamStatus != 0 {
		bad = append(bad, fmt.Sprintf("provider status %d on an answered request", e.UpstreamStatus))
	}
	if e.Input == 0 {
		bad = append(bad, "no input tokens: streamed requests carry usage")
	}
	if e.Estimated {
		bad = append(bad, "marked as an estimate")
	}
	if e.ChargeWaived != "" {
		bad = append(bad, "charge waived on an answered request")
	}
	return join(bad)
}

// Settled reports whether t holds the streamed requests' usage, not only the first entry.
//
// A run's Requests becoming positive does not mean its totals are final: the refused entry (no
// tokens) can be the first one the ledger ingests. Anything that waits for the ledger must wait on
// the tokens it expects, which is what this does (old issue 291).
func (l Ledger) Settled(t Totals) bool {
	return t.Requests > 0 && t.InputTokens >= l.StreamedInputTokens
}

// CheckTotals reports how settled totals differ from the scenario: no request an estimate, and no
// output charged for the refused request.
func (l Ledger) CheckTotals(t Totals) error {
	var bad []string
	if t.EstimatedRequests != 0 {
		bad = append(bad, fmt.Sprintf("%d estimated requests: streamed requests report usage and a refusal is waived, not estimated", t.EstimatedRequests))
	}
	if t.InputTokens < l.StreamedInputTokens {
		bad = append(bad, fmt.Sprintf("%d input tokens, want at least %d", t.InputTokens, l.StreamedInputTokens))
	}
	if t.OutputTokens >= l.OutputCeiling {
		bad = append(bad, fmt.Sprintf("%d output tokens, want fewer than %d", t.OutputTokens, l.OutputCeiling))
	}
	return join(bad)
}

func join(bad []string) error {
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(bad, "; "))
}
