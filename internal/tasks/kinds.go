// SPDX-License-Identifier: Apache-2.0

// Package tasks owns the Task module (design note 01, issue #692). This file and decode.go are its
// catalog of Timeline events and the reader: each member of the TimelineEvent oneof is one event
// kind, and Decode reads a stored row of any payload version as a TimelineEvent.
package tasks

import (
	"maps"
	"slices"

	"google.golang.org/protobuf/reflect/protoreflect"

	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
)

// kindEntry is one row of the catalog: the kind string in task_events.kind and the function that
// gives a version 0 payload the shape of the member's message.
type kindEntry struct {
	kind   string
	legacy reshape
}

// catalog maps each member of the TimelineEvent oneof, by its proto field name, to its kind. A new
// member needs a row here; the suite walks the oneof and fails for a member without one.
var catalog = map[protoreflect.Name]kindEntry{
	"message":                 {"message", renamed(map[string]string{"of": "continuation_max"})},
	"task_created":            {"task.created", renamed(map[string]string{"trigger": "trigger_id"})},
	"status_changed":          {"status.changed", same},
	"run_queued":              {"run.queued", same},
	"run_parked":              {"run.parked", renamed(map[string]string{"approvals": "approval_ids", "questions": "question_ids"})},
	"run_failed":              {"run.failed", same},
	"run_canceled":            {"run.canceled", same},
	"run_session_reset":       {"run.session_reset", same},
	"run_session_restarted":   {"run.session_restarted", same},
	"sandbox_recreated":       {"sandbox.recreated", same},
	"sandbox_work_lost":       {"sandbox.work_lost", same},
	"agent_unhealthy":         {"agent.unhealthy", same},
	"approval_requested":      {"approval.requested", approvalScope},
	"approval_first_approved": {"approval.first_approved", approvalScope},
	"approval_approved":       {"approval.approved", approvalScope},
	"approval_denied":         {"approval.denied", approvalScope},
	"approval_executed":       {"approval.executed", approvalScope},
	"approval_failed":         {"approval.failed", approvalScope},
	"approval_expired":        {"approval.expired", approvalScope},
	"approval_canceled":       {"approval.canceled", approvalScope},
	"question_asked":          {"question.asked", renamed(map[string]string{"default": "default_answer"})},
	"question_answered":       {"question.answered", same},
	"question_dismissed":      {"question.dismissed", same},
	"question_expired":        {"question.expired", same},
	"question_canceled":       {"question.canceled", same},
	"question_retargeted":     {"question.retargeted", same},
}

// eventOneof is the TimelineEvent oneof.
var eventOneof = (&taskv1alpha1.TimelineEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")

// byKind maps a kind string to its oneof member.
var byKind = func() map[string]protoreflect.FieldDescriptor {
	out := make(map[string]protoreflect.FieldDescriptor, len(catalog))
	fields := eventOneof.Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		if e, ok := catalog[f.Name()]; ok {
			out[e.kind] = f
		}
	}
	return out
}()

// Kind returns the kind string of the event's member; false when no member is set or the catalog
// does not name it.
func Kind(ev *taskv1alpha1.TimelineEvent) (string, bool) {
	f := ev.ProtoReflect().WhichOneof(eventOneof)
	if f == nil {
		return "", false
	}
	e, ok := catalog[f.Name()]
	return e.kind, ok
}

// externalKinds are the kinds that an external actor (actor kind `external`, ADR 0130) can write:
// a person without a Koinon account can start a task and reply in its thread, and do nothing else.
// The check task_events_external_message_check (migration 00115) enforces the same rule.
var externalKinds = map[string]bool{"message": true}

// ExternalActorCanWrite says whether an event of this kind can have the actor kind `external`. Only
// a message can: an approval, an answer, a dismissal or a state change needs an org member.
func ExternalActorCanWrite(kind string) bool {
	return externalKinds[kind]
}

// Kinds returns every kind string in the catalog, sorted.
func Kinds() []string {
	out := make([]string, 0, len(catalog))
	for e := range maps.Values(catalog) {
		out = append(out, e.kind)
	}
	slices.Sort(out)
	return out
}
