// SPDX-License-Identifier: Apache-2.0

package services

import connect "connectrpc.com/connect/v2"

// heldMessage is what an admin sees when a held install refuses a call that would reach outward.
const heldMessage = "install is held for restore; release the hold first (restoreHold=false in the chart, then a rollout; " +
	"see the \"Back up and restore\" guide)"

// refuseWhileHeld is the one place that keeps a held koinon-api (restore-hold mode, issue #462,
// ADR 0107) from acting outward when an admin asks: a model call, a code-host read, an MCP server
// request or a trace export. Every handler that calls outside the install starts with it, after
// authorization, so a caller who may not use the call learns nothing about the hold. It returns
// nil when the install is not held.
func (s *Server) refuseWhileHeld() error {
	if s.restoreHold {
		return connect.NewError(connect.CodeFailedPrecondition, heldMessage)
	}
	return nil
}
