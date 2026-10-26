// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"time"

	"github.com/stateracloud/koinon/internal/acp"
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
