// SPDX-License-Identifier: Apache-2.0

package services

import (
	connect "connectrpc.com/connect/v2"
)

// refuseWhileHeld keeps a held koinon-api (restore-hold mode, issue #462, ADR 0107) from acting
// outward in the code paths that have no service of their own: the code-host calls the connection
// and binding handlers make directly. Every other outward call is refused by the service that makes
// it, through the shared restorehold.Guard, so it holds for every caller and both API surfaces;
// dbError maps restorehold.ErrHeld to FailedPrecondition. It returns nil when the install is not held.
func (s *Server) refuseWhileHeld() error {
	if err := s.hold.Err(); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return nil
}
