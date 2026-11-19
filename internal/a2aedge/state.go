// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/stateracloud/koinon/internal/db/store"
)

// State maps a Koinon task state to the A2A task state (ADR 0122 decision 3).
//
// Koinon's input_required means a person has to act: answer a question, approve or validate
// (ADR 0065). All of these are the A2A `input-required` state. Koinon has no `auth-required` or
// `rejected` task state, so the edge never sends them.
func State(s store.TaskState) a2a.TaskState {
	switch s {
	case store.TaskStateSubmitted:
		return a2a.TaskStateSubmitted
	case store.TaskStateWorking:
		return a2a.TaskStateWorking
	case store.TaskStateInputRequired:
		return a2a.TaskStateInputRequired
	case store.TaskStateCompleted:
		return a2a.TaskStateCompleted
	case store.TaskStateCanceled:
		return a2a.TaskStateCanceled
	case store.TaskStateFailed:
		return a2a.TaskStateFailed
	default:
		return a2a.TaskStateUnspecified
	}
}

// Ended reports whether the state is terminal in A2A: the task takes no more messages.
func Ended(s store.TaskState) bool {
	switch s {
	case store.TaskStateCompleted, store.TaskStateCanceled, store.TaskStateFailed:
		return true
	}
	return false
}

// Settled reports whether a blocking SendMessage can return: the task ended, or it waits on the
// client (A2A calls this an interrupted state).
func Settled(s store.TaskState) bool {
	return Ended(s) || s == store.TaskStateInputRequired
}
