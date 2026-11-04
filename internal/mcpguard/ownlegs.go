// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/githubtools"
)

// ownResultLegs are the legs the result of each of Koinon's own tools sets (ADR 0091 decision 4).
// They are declared here, in code, next to the tools they describe, because an own tool has no row
// an admin could declare. Every one returns a fact Koinon made (a pull request's URL, an approval's
// state, the person's answer to a question, the task's closing), never text from a third party and
// never org data the agent had not already put in the call, so none sets a leg.
var ownResultLegs = map[string]Legs{
	githubtools.ToolCreatePullRequest: {},
	githubtools.ToolCommentOnIssue:    {},
	githubtools.ToolGetApprovalStatus: {},
	gateway.ToolAskUser:               {},
	gateway.ToolCancelTask:            {},
}

// OwnResultLegs returns the legs the result of one of Koinon's own tools sets. A tool that is not
// declared above sets both (fail closed): a new own tool starts out cautious until its author
// declares what it returns.
func OwnResultLegs(tool string) Legs {
	if l, ok := ownResultLegs[tool]; ok {
		return l
	}
	return Legs{UntrustedInput: true, PrivateData: true}
}
