// SPDX-License-Identifier: Apache-2.0

package runoutcome

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// EventClass says what a failed write of a run event does to the run (ADR 0074, amendment of
// 2026-10-31; ADR 0023, same date).
type EventClass int

const (
	// ClassBusiness is state the run depends on: its state changes, park and resume, approvals and
	// answers, the task's status and the final reply. A write that fails after its tries abandons
	// the run (FenceAbandon), as it always did: the lease expires and another worker resumes it.
	ClassBusiness EventClass = iota
	// ClassDiagnostic is everything else: thoughts, tool-call progress, usage and debug notes. A
	// write that fails after its tries is logged and counted, and the run goes on without it.
	ClassDiagnostic
)

func (c EventClass) String() string {
	if c == ClassDiagnostic {
		return "diagnostic"
	}
	return "business"
}

// eventClasses is the class of every run event kind agentd writes through Record. A kind that is
// not listed is business: a new kind is never dropped silently before someone decides it may be.
//
// The rule for the list: a kind that the run's outcome, the task's reply or the UI's view of the
// run's state reads is business. The timeline renders most kinds, which alone does not make one
// business. internal/runreply reads acp.agent_message_chunk for the reply and acp.tool_call and
// acp.tool_call_update as the boundaries of its messages, so those three are business although
// ADR 0074 lists tool-call items as diagnostic: dropping one would merge interim narration into
// the reply.
var eventClasses = map[string]EventClass{
	// Written by this package's verbs in their own transactions; listed so the table is the whole
	// list of kinds (they never go through Record).
	"run.succeeded": ClassBusiness,
	"run.failed":    ClassBusiness,
	KindRunCanceled: ClassBusiness,
	KindParked:      ClassBusiness,
	"run.requeued":  ClassBusiness,

	// The run's harness session and its turns: the session's starts and resets, what each turn was
	// asked and why one was interrupted. They are state changes of the run.
	"run.session":           ClassBusiness,
	"run.session_reset":     ClassBusiness,
	"run.session_restarted": ClassBusiness,
	"run.prompt":            ClassBusiness,
	"run.interrupted":       ClassBusiness,
	// An approval decision of the run's tool call; the approval's audit trail.
	"acp.permission": ClassBusiness,
	// The agent's words and the tool calls between them: internal/runreply builds the reply from them.
	"acp.agent_message_chunk": ClassBusiness,
	"acp.tool_call":           ClassBusiness,
	"acp.tool_call_update":    ClassBusiness,

	// The rest of the harness's session updates. A chunk kind's event is one stored item (koinon-private#344,
	// runoutcome.RecordUpdate); the classes stay with the kinds.
	"acp.agent_thought_chunk": ClassDiagnostic,
	"acp.user_message_chunk":  ClassDiagnostic,
	"acp.plan":                ClassDiagnostic,
	// Not recorded today: agentd keeps it as harness state and returns before the write (run.go);
	// listed so the class is settled if it ever is.
	"acp.available_commands_update": ClassDiagnostic,
	"acp.current_mode_update":       ClassDiagnostic,
	"acp.usage_update":              ClassDiagnostic,

	// Notes about the run that nothing reads back to decide state.
	"run.uncached":              ClassDiagnostic,
	"run.sandbox_swept":         ClassDiagnostic,
	"run.home_restored":         ClassDiagnostic,
	"run.instructions_restored": ClassDiagnostic,
	"run.mcp_server_missing":    ClassDiagnostic,
	"run.mcp_server_recovered":  ClassDiagnostic,
	"run.waiting_for_model":     ClassDiagnostic,
	"run.refusal":               ClassDiagnostic,
	"run.retry":                 ClassDiagnostic,
	"run.wrap_up_notice":        ClassDiagnostic,
	"run.summary":               ClassDiagnostic,
	"run.summary_failed":        ClassDiagnostic,
	"run.summary_refused":       ClassDiagnostic,
}

// ClassOf is the class of a run event kind; an unknown kind is business.
func ClassOf(kind string) EventClass {
	if c, ok := eventClasses[kind]; ok {
		return c
	}
	return ClassBusiness
}

// KnownEventKinds are the kinds the class table lists, for tests and docs.
func KnownEventKinds() map[string]EventClass {
	out := make(map[string]EventClass, len(eventClasses))
	for k, c := range eventClasses {
		out[k] = c
	}
	return out
}

// WriteFailure is why a run event could not be written: the reason label of the metric, from a
// fixed set (ADR 0023: never an ID or an error's text).
type WriteFailure string

const (
	// FailureTimeout: the write ran out of time (its context or the network's deadline).
	FailureTimeout WriteFailure = "timeout"
	// FailureConn: the connection to the database failed or was lost.
	FailureConn WriteFailure = "conn"
	// FailureTransient: the database refused for a while (serialization, deadlock, resources,
	// an administrator's restart).
	FailureTransient WriteFailure = "transient"
	// FailureCanceled: the write's context was canceled (the run or the worker is stopping).
	FailureCanceled WriteFailure = "canceled"
	// FailureOther: anything else.
	FailureOther WriteFailure = "other"
)

// WriteFailures is the reason label's whole set.
var WriteFailures = []WriteFailure{FailureTimeout, FailureConn, FailureTransient, FailureCanceled, FailureOther}

// PermanentSQLState reports whether a database error can never succeed on retry, and its SQLSTATE.
// The statement or its data is what the database refuses (cardinality violation 21, data exception
// 22, integrity constraint violation 23, syntax error or access rule violation 42, WITH CHECK
// OPTION violation 44, feature not supported 0A), not a moment it is unreachable or busy. Anything
// else, a connection, timeout, serialization or resource error and any error that is not the
// database's, may pass and is not permanent.
func PermanentSQLState(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return "", false
	}
	switch pgErr.Code[:2] {
	case "21", "22", "23", "42", "44", "0A":
		return pgErr.Code, true
	}
	return "", false
}

// failureOf classifies a write's error into the fixed set.
func failureOf(err error) WriteFailure {
	var pgErr *pgconn.PgError
	var netErr net.Error
	switch {
	// Before the timeout check: pgconn.Timeout reports a canceled context as a timeout too.
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	case errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err):
		return FailureTimeout
	case errors.As(err, &pgErr):
		switch {
		case pgErr.Code == "57014": // query_canceled: statement_timeout or a cancel request
			return FailureTimeout
		case len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "40" || pgErr.Code[:2] == "53"),
			len(pgErr.Code) >= 3 && pgErr.Code[:3] == "57P":
			return FailureTransient
		case len(pgErr.Code) >= 2 && pgErr.Code[:2] == "08":
			return FailureConn
		}
		return FailureOther
	case errors.As(err, &netErr):
		if netErr.Timeout() {
			return FailureTimeout
		}
		return FailureConn
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) || pgconn.SafeToRetry(err) {
		return FailureConn
	}
	return FailureOther
}
