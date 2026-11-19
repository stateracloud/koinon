// SPDX-License-Identifier: Apache-2.0

package a2aout

import "errors"

// Errors of the client. Errors that the external agent sends are the errors of the a2a package
// (a2a.ErrTaskNotFound, a2a.ErrUnsupportedOperation and so on) and match with errors.Is.
var (
	// ErrBadTarget is a target URL that is not an http or https URL with a host.
	ErrBadTarget = errors.New("a2aout: bad target URL")
	// ErrEgressDenied is a target the URL policy refuses (ADR 0120).
	ErrEgressDenied = errors.New("a2aout: the egress policy denies the target")
	// ErrCard is an agent card that cannot be read or used.
	ErrCard = errors.New("a2aout: agent card")
	// ErrCardChanged is a card whose hash is not the pin. The agent changed its card since an admin read
	// it. An admin reads the new card and accepts it (ADR 0123 decision 11). The text names nothing of the
	// card.
	ErrCardChanged = errors.New("a2aout: the agent card changed since it was pinned")
	// ErrCardUnpinned is a client built with no card hash: the pin is required.
	ErrCardUnpinned = errors.New("a2aout: the agent card is not pinned")
	// ErrCardHost is a card with no JSON-RPC interface on the host of the target.
	ErrCardHost = errors.New("a2aout: agent card names another host")
	// ErrBadRequest is a request the client refuses before it sends it.
	ErrBadRequest = errors.New("a2aout: bad request")
	// ErrTaskEnded is a message for a task that ended. A2A 1.0.0: it never takes one. Send a
	// follow-up turn: a new task in the same context, with After.
	ErrTaskEnded = errors.New("a2aout: the task has ended")
	// ErrNotAwaitingInput is an answer to a task that is not in `input-required`.
	ErrNotAwaitingInput = errors.New("a2aout: the task does not wait for input")
	// ErrTaskIDReused is a follow-up that the agent answered with the task it follows.
	ErrTaskIDReused = errors.New("a2aout: the agent reused a task ID")
	// ErrTaskMismatch is an answer for another task than the one asked.
	ErrTaskMismatch = errors.New("a2aout: the agent answered for another task")
	// ErrContextMismatch is an answer in another context than the one asked.
	ErrContextMismatch = errors.New("a2aout: the agent answered in another context")
	// ErrUnknownState is a task state that A2A 1.0.0 does not have.
	ErrUnknownState = errors.New("a2aout: unknown task state")
	// ErrBadResponse is a response that is neither a task nor a message.
	ErrBadResponse = errors.New("a2aout: bad response")
	// ErrBodyTooLarge is a response over the size limit.
	ErrBodyTooLarge = errors.New("a2aout: the response is too large")
	// ErrTransport is a failed request: no connection, a refused address, a timeout.
	ErrTransport = errors.New("a2aout: request failed")
)
