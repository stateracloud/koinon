// SPDX-License-Identifier: Apache-2.0

package tasks_test

import (
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	"github.com/stateracloud/koinon/internal/tasks"
)

// legacyFixture is one version 0 payload as its writer stores it today, and the event it reads as.
type legacyFixture struct {
	kind    string
	payload string
	want    *taskv1alpha1.TimelineEvent
}

var (
	at      = time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)
	atProto = timestamppb.New(at)
)

func ev(set func(*taskv1alpha1.TimelineEvent)) *taskv1alpha1.TimelineEvent {
	e := &taskv1alpha1.TimelineEvent{}
	set(e)
	return e
}

func structOf(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}

const (
	approvalBase = `"approval_id":"a1","tool":"create_pull_request","target":"acme/app","args_sha256":"abc"`
	scopeFlat    = `"scope_id":"s1","scope_kind":"task","scope_expires_at":"2026-11-26T09:30:00Z","scope_created_by":"u1","scope_task_id":"t1"`
)

var scope = &taskv1alpha1.ApprovalScope{Id: "s1", Kind: "task", ExpiresAt: atProto, CreatedBy: "u1", TaskId: "t1"}

func changed(extra func(*taskv1alpha1.ApprovalChanged)) *taskv1alpha1.ApprovalChanged {
	c := &taskv1alpha1.ApprovalChanged{ApprovalId: "a1", Tool: "create_pull_request", Target: "acme/app", ArgsSha256: "abc"}
	extra(c)
	return c
}

// fixtures has one payload per kind, with the keys that its writers set (the catalog of issue #735).
var fixtures = []legacyFixture{
	{"message", `{"text":"go on","source":"continuation","continuation":2,"of":3}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_Message{Message: &taskv1alpha1.TimelineMessage{Text: "go on", Source: "continuation", Continuation: 2, ContinuationMax: 3}}
	})},
	{"task.created", `{"title":"Fix CI","source":"direct_message","trigger":"tr1"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_TaskCreated{TaskCreated: &taskv1alpha1.TaskCreated{Title: "Fix CI", Source: "direct_message", TriggerId: "tr1"}}
	})},
	{"status.changed", `{"from":"working","to":"input_required","reason":"continuation_limit","continuations":3,"max_continuations":3}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_StatusChanged{StatusChanged: &taskv1alpha1.StatusChanged{From: "working", To: "input_required", Reason: "continuation_limit", Continuations: 3, MaxContinuations: 3}}
	})},
	{"run.queued", `{"max_wall_clock_seconds":1800,"max_model_requests":9007199254740993,"max_spend_usd":"2.50"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_RunQueued{RunQueued: &taskv1alpha1.RunQueued{MaxWallClockSeconds: 1800, MaxModelRequests: 9007199254740993, MaxSpendUsd: "2.50"}}
	})},
	{"run.parked", `{"reason":"approval","approvals":["a1","a2"],"questions":[]}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_RunParked{RunParked: &taskv1alpha1.RunParked{Reason: "approval", ApprovalIds: []string{"a1", "a2"}}}
	})},
	{"run.failed", `{"reason":"sandbox not ready","failureClass":"sandbox_unavailable","sandboxCause":"image_pull","waitedSeconds":300,"detail":"pull failed","causeSeenMs":1200}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_RunFailed{RunFailed: &taskv1alpha1.RunEnded{Reason: "sandbox not ready", FailureClass: "sandbox_unavailable", SandboxCause: "image_pull", WaitedSeconds: 300, Detail: "pull failed", CauseSeenMs: 1200}}
	})},
	{"run.canceled", `{"reason":"wall_clock_cap","message":"stopped","maxWallClockSeconds":600,"summary":"half done","summarySource":"reply","failureClass":"budget","limit":"run_spend","spend_limit_usd":"1","spent_usd":"1.2","model_requests":40,"requeueReason":"x","unknown_extra":{"a":1}}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_RunCanceled{RunCanceled: &taskv1alpha1.RunEnded{
			Reason: "wall_clock_cap", Message: "stopped", MaxWallClockSeconds: 600, Summary: "half done", SummarySource: "reply", FailureClass: "budget",
			Limit: "run_spend", SpendLimitUsd: "1", SpentUsd: "1.2", ModelRequests: 40, RequeueReason: "x",
		}}
	})},
	{"run.session_reset", `{"message":"new session","previousSessionId":"p1","sessionId":"s2"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_RunSessionReset{RunSessionReset: &taskv1alpha1.SessionChanged{Message: "new session", PreviousSessionId: "p1", SessionId: "s2"}}
	})},
	{"run.session_restarted", `{"message":"handoff","previousSessionId":"p1","sessionId":"s2"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_RunSessionRestarted{RunSessionRestarted: &taskv1alpha1.SessionChanged{Message: "handoff", PreviousSessionId: "p1", SessionId: "s2"}}
	})},
	{"sandbox.recreated", `{"message":"recreated"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_SandboxRecreated{SandboxRecreated: &taskv1alpha1.TaskNote{Message: "recreated"}}
	})},
	{"sandbox.work_lost", `{"message":"lost"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_SandboxWorkLost{SandboxWorkLost: &taskv1alpha1.TaskNote{Message: "lost"}}
	})},
	{"agent.unhealthy", `{"message":"unhealthy"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_AgentUnhealthy{AgentUnhealthy: &taskv1alpha1.TaskNote{Message: "unhealthy"}}
	})},
	{"approval.requested", `{` + approvalBase + `,"decision":"allow","risk":{"level":"low"},"approvals_required":2,` + scopeFlat + `}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalRequested{ApprovalRequested: &taskv1alpha1.ApprovalRequested{
			ApprovalId: "a1", Tool: "create_pull_request", Target: "acme/app", ArgsSha256: "abc", Decision: "allow",
			Risk: structpb.NewStructValue(structOf(map[string]any{"level": "low"})), ApprovalsRequired: 2, Scope: scope,
		}}
	})},
	{"approval.first_approved", `{` + approvalBase + `,"note":"one more"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalFirstApproved{ApprovalFirstApproved: changed(func(c *taskv1alpha1.ApprovalChanged) { c.Note = "one more" })}
	})},
	{"approval.approved", `{` + approvalBase + `,"note":"ok",` + scopeFlat + `}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalApproved{ApprovalApproved: changed(func(c *taskv1alpha1.ApprovalChanged) { c.Note, c.Scope = "ok", scope })}
	})},
	{"approval.denied", `{` + approvalBase + `,"note":"no"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalDenied{ApprovalDenied: changed(func(c *taskv1alpha1.ApprovalChanged) { c.Note = "no" })}
	})},
	{"approval.executed", `{` + approvalBase + `,"result":{"url":"https://example.invalid/pr/1","number":1}}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalExecuted{ApprovalExecuted: changed(func(c *taskv1alpha1.ApprovalChanged) {
			c.Result = structOf(map[string]any{"url": "https://example.invalid/pr/1", "number": 1})
		})}
	})},
	{"approval.failed", `{` + approvalBase + `,"error":"422"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalFailed{ApprovalFailed: changed(func(c *taskv1alpha1.ApprovalChanged) { c.Error = "422" })}
	})},
	{"approval.expired", `{` + approvalBase + `,"expires_at":"2026-11-26T10:30:00+01:00"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalExpired{ApprovalExpired: changed(func(c *taskv1alpha1.ApprovalChanged) { c.ExpiresAt = atProto })}
	})},
	{"approval.canceled", `{` + approvalBase + `,"reason":"run ended"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalCanceled{ApprovalCanceled: changed(func(c *taskv1alpha1.ApprovalChanged) { c.Reason = "run ended" })}
	})},
	{"question.asked", `{"question_id":"q1","question":"Which branch?","target":{"kind":"user","user":"u1","role":""},"to_label":"Ada","recipients":["u1"],"choices":["main","dev"],"has_draft":true,"default":"main","expires_at":"2026-11-26T09:30:00Z"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_QuestionAsked{QuestionAsked: &taskv1alpha1.QuestionAsked{
			QuestionId: "q1", Question: "Which branch?", Target: &taskv1alpha1.QuestionTarget{Kind: "user", User: "u1"}, ToLabel: "Ada",
			Recipients: []string{"u1"}, Choices: []string{"main", "dev"}, HasDraft: true, DefaultAnswer: "main", ExpiresAt: atProto,
		}}
	})},
	{"question.answered", `{"question_id":"q1","answer_kind":"choice","answer":"main","default_applied":false,"answered_by_name":"Ada"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_QuestionAnswered{QuestionAnswered: &taskv1alpha1.QuestionResolved{QuestionId: "q1", AnswerKind: "choice", Answer: "main", AnsweredByName: "Ada"}}
	})},
	{"question.dismissed", `{"question_id":"q1","answer_kind":"dismiss","answer":"","default_applied":false,"answered_by_name":"Ada"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_QuestionDismissed{QuestionDismissed: &taskv1alpha1.QuestionResolved{QuestionId: "q1", AnswerKind: "dismiss", AnsweredByName: "Ada"}}
	})},
	{"question.expired", `{"question_id":"q1","answer_kind":"default","answer":"main","default_applied":true}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_QuestionExpired{QuestionExpired: &taskv1alpha1.QuestionResolved{QuestionId: "q1", AnswerKind: "default", Answer: "main", DefaultApplied: true}}
	})},
	{"question.canceled", `{"question_id":"q1","reason":"run ended"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_QuestionCanceled{QuestionCanceled: &taskv1alpha1.QuestionCanceled{QuestionId: "q1", Reason: "run ended"}}
	})},
	{"question.retargeted", `{"question_id":"q1","to_label":"Owners","recipients":["u2"],"reason":"fallback","requestedTarget":{"kind":"user","user":"u1"},"fallbackReason":"user left"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_QuestionRetargeted{QuestionRetargeted: &taskv1alpha1.QuestionRetargeted{
			QuestionId: "q1", ToLabel: "Owners", Recipients: []string{"u2"}, Reason: "fallback",
			RequestedTarget: &taskv1alpha1.QuestionTarget{Kind: "user", User: "u1"}, FallbackReason: "user left",
		}}
	})},
}

// variants are other payloads of a kind, one per writer whose keys the fixture above does not have.
var variants = []legacyFixture{
	{"message", `{"text":"run the report","source":"schedule","fire":"manual","firedBy":"u1"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_Message{Message: &taskv1alpha1.TimelineMessage{Text: "run the report", Source: "schedule", Fire: "manual", FiredBy: "u1"}}
	})},
	{"message", `{"text":"Fix CI\n\nrun the checks","display":"Fix CI","source":"direct_message","message_id":"m1"}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_Message{Message: &taskv1alpha1.TimelineMessage{Text: "Fix CI\n\nrun the checks", Display: "Fix CI", Source: "direct_message", MessageId: "m1"}}
	})},
	{"run.canceled", `{"reason":"model request limit reached (50 of 50 requests)","limit":"requests","max_model_requests":50,"model_requests":50}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_RunCanceled{RunCanceled: &taskv1alpha1.RunEnded{
			Reason: "model request limit reached (50 of 50 requests)", Limit: "requests", MaxModelRequests: 50, ModelRequests: 50,
		}}
	})},
	// A scope without an expiry: the writer stores the zero time.Time.
	{"approval.approved", `{` + approvalBase + `,"scope_id":"s1","scope_kind":"org","scope_expires_at":"0001-01-01T00:00:00Z","scope_created_by":"u1","scope_task_id":""}`, ev(func(e *taskv1alpha1.TimelineEvent) {
		e.Event = &taskv1alpha1.TimelineEvent_ApprovalApproved{ApprovalApproved: changed(func(c *taskv1alpha1.ApprovalChanged) {
			c.Scope = &taskv1alpha1.ApprovalScope{Id: "s1", Kind: "org", ExpiresAt: timestamppb.New(time.Time{}), CreatedBy: "u1"}
		})}
	})},
}

func equalProto(want proto.Message) OmegaMatcher {
	return WithTransform(func(got proto.Message) bool { return proto.Equal(got, want) }, BeTrue())
}

var _ = Describe("Decode", func() {
	It("has one legacy fixture per kind of the catalog", func() {
		kinds := make([]string, 0, len(fixtures))
		for _, f := range fixtures {
			kinds = append(kinds, f.kind)
		}
		Expect(kinds).To(ConsistOf(tasks.Kinds()))
		Expect(fixtures).To(HaveLen(26))
	})

	legacy := make([]TableEntry, 0, len(fixtures))
	trips := make([]TableEntry, 0, len(fixtures))
	for _, f := range fixtures {
		legacy = append(legacy, Entry(f.kind, f))
		trips = append(trips, Entry(f.kind, f))
	}
	for i, f := range variants {
		legacy = append(legacy, Entry(f.kind+" variant "+strconv.Itoa(i), f))
		trips = append(trips, Entry(f.kind+" variant "+strconv.Itoa(i), f))
	}

	DescribeTable("reads a version 0 payload as its writer stores it",
		func(f legacyFixture) {
			got, err := tasks.Decode(f.kind, tasks.VersionLegacy, []byte(f.payload))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(equalProto(f.want), "got %v", got)
		}, legacy)

	DescribeTable("writes a version 1 payload that reads back as the same event",
		func(f legacyFixture) {
			kind, payload, err := tasks.Encode(f.want)
			Expect(err).NotTo(HaveOccurred())
			Expect(kind).To(Equal(f.kind))
			got, err := tasks.Decode(kind, tasks.VersionTyped, payload)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(equalProto(f.want), "payload %s", payload)
		}, trips)

	It("gives every member of the oneof a kind and a legacy decoder", func() {
		fields := (&taskv1alpha1.TimelineEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event").Fields()
		Expect(fields.Len()).To(BeNumerically(">", 0))
		seen := map[string]string{}
		for i := range fields.Len() {
			name := fields.Get(i).Name()
			kind, legacy, ok := tasks.CatalogEntry(name)
			Expect(ok).To(BeTrue(), "member %s has no row in the catalog", name)
			Expect(kind).NotTo(BeEmpty(), "member %s has no kind", name)
			Expect(legacy).To(BeTrue(), "member %s has no legacy decoder", name)
			Expect(seen).NotTo(HaveKey(kind), "members %s and %s share kind %q", seen[kind], name, kind)
			seen[kind] = string(name)
		}
		Expect(tasks.Kinds()).To(HaveLen(fields.Len()), "the catalog names a member that the oneof does not have")
	})

	It("reads an empty version 0 payload as an empty member", func() {
		got, err := tasks.Decode("sandbox.recreated", tasks.VersionLegacy, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetSandboxRecreated()).To(equalProto(&taskv1alpha1.TaskNote{}))
	})

	It("refuses a kind that the catalog does not name", func() {
		_, err := tasks.Decode("run.something_new", tasks.VersionLegacy, []byte(`{}`))
		Expect(err).To(MatchError(tasks.ErrUnknownKind))
	})

	It("refuses a payload version that it does not know", func() {
		_, err := tasks.Decode("message", 2, []byte(`{}`))
		Expect(err).To(MatchError(tasks.ErrUnknownVersion))
	})

	It("refuses a version 1 payload whose member is not the row's kind", func() {
		_, payload, err := tasks.Encode(fixtures[0].want)
		Expect(err).NotTo(HaveOccurred())
		_, err = tasks.Decode("task.created", tasks.VersionTyped, payload)
		Expect(err).To(MatchError(ContainSubstring(`holds a "message" event`)))
	})

	It("refuses a version 0 payload with a value of the wrong type", func() {
		_, err := tasks.Decode("message", tasks.VersionLegacy, []byte(`{"text":3}`))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("ExternalActorCanWrite", func() {
	It("lets an external actor write a message and no other kind of the catalog (ADR 0130)", func() {
		Expect(tasks.ExternalActorCanWrite("message")).To(BeTrue())
		for _, k := range tasks.Kinds() {
			if k != "message" {
				Expect(tasks.ExternalActorCanWrite(k)).To(BeFalse(), k)
			}
		}
		Expect(tasks.ExternalActorCanWrite("no.such.kind")).To(BeFalse())
	})
})
