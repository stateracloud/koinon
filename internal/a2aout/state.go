// SPDX-License-Identifier: Apache-2.0

package a2aout

import (
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Reason says why an external state is not a plain Koinon state. It is empty for a state that
// maps one to one.
type Reason string

const (
	// ReasonNone is a state that maps one to one.
	ReasonNone Reason = ""
	// ReasonRejected is the A2A `rejected` state. Koinon has no such task state, so the task is
	// `failed`: the agent refused the work and a retry with the same message cannot help.
	ReasonRejected Reason = "rejected"
	// ReasonAuthRequired is the A2A `auth-required` state: the external agent needs a credential
	// that the gateway does not hold. The task is `input_required`, because it waits and a person
	// can fix it (an admin adds the credential to the connection). The sandbox never supplies it.
	ReasonAuthRequired Reason = "auth_required"
)

// State maps the state of an external A2A task back to a Koinon task state (ADR 0123 decision 8).
// It is the inverse of a2aedge.State, with two states that Koinon has no name for ([Reason]).
// A state it does not know is an error: the client never guesses a state, because a wrong guess
// can end a task that still runs, or leave one open that has ended.
func State(s a2a.TaskState) (store.TaskState, Reason, error) {
	switch s {
	case a2a.TaskStateSubmitted:
		return store.TaskStateSubmitted, ReasonNone, nil
	case a2a.TaskStateWorking:
		return store.TaskStateWorking, ReasonNone, nil
	case a2a.TaskStateInputRequired:
		return store.TaskStateInputRequired, ReasonNone, nil
	case a2a.TaskStateAuthRequired:
		return store.TaskStateInputRequired, ReasonAuthRequired, nil
	case a2a.TaskStateCompleted:
		return store.TaskStateCompleted, ReasonNone, nil
	case a2a.TaskStateCanceled:
		return store.TaskStateCanceled, ReasonNone, nil
	case a2a.TaskStateFailed:
		return store.TaskStateFailed, ReasonNone, nil
	case a2a.TaskStateRejected:
		return store.TaskStateFailed, ReasonRejected, nil
	default:
		return "", ReasonNone, fmt.Errorf("%w: %q", ErrUnknownState, string(s))
	}
}

// Ended reports whether an external task state is terminal in A2A 1.0.0: completed, failed,
// canceled or rejected. A task that has ended never takes a new message.
func Ended(s a2a.TaskState) bool { return s.Terminal() }

// settled reports whether a wait can stop: the task ended, or it waits on the client (A2A calls
// `input-required` and `auth-required` interrupted states).
func settled(s a2a.TaskState) bool {
	return Ended(s) || s == a2a.TaskStateInputRequired || s == a2a.TaskStateAuthRequired
}
