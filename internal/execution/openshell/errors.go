// SPDX-License-Identifier: Apache-2.0

package openshell

import "github.com/stateracloud/koinon/internal/execution"

// wrap maps a gRPC error to an *execution.Error; see execution.GRPCError.
func wrap(op string, err error) error { return execution.GRPCError(op, err) }
