// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error kinds. Backend errors match one of these with errors.Is; the backend's own error
// (for OpenShell, the gRPC status) stays reachable through errors.As.
var (
	// ErrNotFound: the workspace, sandbox, provider or profile does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists: a create raced with another one.
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict: an optimistic-concurrency check failed; re-read and retry.
	ErrConflict = errors.New("conflict")
	// ErrForeign: the object exists but another install owns it (its install label differs from
	// this install's). Koinon neither adopts nor changes nor deletes it (issue 461).
	ErrForeign = errors.New("owned by another install")
	// ErrInvalid: the request was rejected as invalid, including policies that break the
	// hardening defaults.
	ErrInvalid = errors.New("invalid")
	// ErrFailedPrecondition: the target is in the wrong state, for example exec on a
	// sandbox that is not ready.
	ErrFailedPrecondition = errors.New("failed precondition")
	// ErrPermissionDenied: the gateway rejected Koinon's identity or role.
	ErrPermissionDenied = errors.New("permission denied")
	// ErrUnavailable: the gateway or the sandbox relay is unreachable or overloaded;
	// retrying later may succeed.
	ErrUnavailable = errors.New("unavailable")
	// ErrUnreachable: the driver cannot open its connection to the data path (the address, the TLS
	// handshake or the client certificate is wrong) and it never was connected. Unlike ErrUnavailable, retrying
	// later does not help until an operator fixes the install, so the caller fails the work with the error as the
	// reason. An outage of a connection that worked is ErrUnavailable.
	ErrUnreachable = errors.New("unreachable")
	// ErrUnsupported: the gateway does not support something Koinon requires.
	ErrUnsupported = errors.New("unsupported")
	// ErrNoSnapshot means a revert has no snapshot to go back to: the sandbox never suspended.
	ErrNoSnapshot = errors.New("no snapshot")
	// ErrRevertResume means the revert itself worked and the resume after it failed. The sandbox holds
	// its snapshot: the caller must not delete it. Read the sandbox to see its state.
	ErrRevertResume = errors.New("resume after revert failed")
	// ErrRepointRefused means a repoint did not change the sandbox, and nothing was lost: the template change
	// breaks an invariant that keeps the home (container name, volumes, mounts, storage location, tier), or
	// Substrate refused UpdateActor. The caller falls back to delete and restore (issue #1482).
	ErrRepointRefused = errors.New("repoint refused")
	// ErrRepointResume means the repoint itself worked and a step after it failed (the egress policy write or
	// the resume). The sandbox names the new template and holds its home: the caller must not delete it.
	ErrRepointResume = errors.New("resume after repoint failed")
	// ErrFrontDown means the guest front of a running sandbox did not answer in time.
	ErrFrontDown = errors.New("guest front does not answer")
	// ErrStreamClosed: an exec stream ended without an exit status.
	ErrStreamClosed = errors.New("exec stream ended without exit status")
)

// FrontDownError is the ErrFrontDown of CheckFront with the worker that hosts the sandbox. The
// reconciler counts dead fronts by worker: silence on more than one worker at once is more likely the
// router than the guests (issue #1481, the fleet breaker).
type FrontDownError struct {
	// Worker names the worker that hosts the sandbox. Empty when the driver does not know it.
	Worker string
	// Err is the error that matches ErrFrontDown.
	Err error
}

func (e *FrontDownError) Error() string { return e.Err.Error() }

// Unwrap returns the error that matches ErrFrontDown.
func (e *FrontDownError) Unwrap() error { return e.Err }

// FrontWorker returns the worker of the FrontDownError in err, or "" when err has none.
func FrontWorker(err error) string {
	var fd *FrontDownError
	if errors.As(err, &fd) {
		return fd.Worker
	}
	return ""
}

// Error is an error from a backend operation.
type Error struct {
	// Op is the backend operation, for example "CreateSandbox".
	Op string
	// Kind is one of the Err* values, or nil if the error has no known kind.
	Kind error
	// Err is the underlying error.
	Err error
}

func (e *Error) Error() string {
	return "execution: " + e.Op + ": " + e.Err.Error()
}

// Unwrap returns the kind and the underlying error, so errors.Is and errors.As see both.
func (e *Error) Unwrap() []error {
	if e.Kind == nil {
		return []error{e.Err}
	}
	return []error{e.Kind, e.Err}
}

// GRPCError maps a gRPC error to an *Error for the operation op, so that every driver reports the
// same kinds for the same status codes. It returns nil for a nil error. Context errors keep
// matching context.Canceled and context.DeadlineExceeded. The status stays reachable through
// errors.As.
func GRPCError(op string, err error) error {
	if err == nil {
		return nil
	}
	var kind error
	switch status.Code(err) {
	case codes.NotFound:
		kind = ErrNotFound
	case codes.AlreadyExists:
		kind = ErrAlreadyExists
	case codes.Aborted:
		kind = ErrConflict
	case codes.InvalidArgument, codes.OutOfRange:
		kind = ErrInvalid
	case codes.FailedPrecondition:
		kind = ErrFailedPrecondition
	case codes.PermissionDenied, codes.Unauthenticated:
		kind = ErrPermissionDenied
	case codes.Unavailable, codes.ResourceExhausted:
		kind = ErrUnavailable
	case codes.Unimplemented:
		kind = ErrUnsupported
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
		case errors.Is(err, ErrStreamClosed):
			kind = ErrStreamClosed
		}
	}
	return &Error{Op: op, Kind: kind, Err: err}
}
