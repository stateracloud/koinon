// SPDX-License-Identifier: Apache-2.0

package execution

import "errors"

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
	// ErrUnsupported: the gateway does not support something Koinon requires.
	ErrUnsupported = errors.New("unsupported")
	// ErrStreamClosed: an exec stream ended without an exit status.
	ErrStreamClosed = errors.New("exec stream ended without exit status")
)

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
