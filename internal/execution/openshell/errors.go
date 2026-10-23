// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
)

// wrap maps a gRPC error to an *execution.Error. It returns nil for a nil error. Context
// errors keep matching context.Canceled and context.DeadlineExceeded.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var kind error
	switch status.Code(err) {
	case codes.NotFound:
		kind = execution.ErrNotFound
	case codes.AlreadyExists:
		kind = execution.ErrAlreadyExists
	case codes.Aborted:
		kind = execution.ErrConflict
	case codes.InvalidArgument, codes.OutOfRange:
		kind = execution.ErrInvalid
	case codes.FailedPrecondition:
		kind = execution.ErrFailedPrecondition
	case codes.PermissionDenied, codes.Unauthenticated:
		kind = execution.ErrPermissionDenied
	case codes.Unavailable, codes.ResourceExhausted:
		kind = execution.ErrUnavailable
	case codes.Unimplemented:
		kind = execution.ErrUnsupported
	case codes.Canceled:
		kind = context.Canceled
	case codes.DeadlineExceeded:
		kind = context.DeadlineExceeded
	}
	if kind == nil {
		switch {
		case errors.Is(err, context.Canceled):
			kind = context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			kind = context.DeadlineExceeded
		case errors.Is(err, execution.ErrStreamClosed):
			kind = execution.ErrStreamClosed
		}
	}
	return &execution.Error{Op: op, Kind: kind, Err: err}
}
