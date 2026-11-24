// SPDX-License-Identifier: Apache-2.0

package services

import (
	"github.com/stateracloud/koinon/internal/db/store"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// auditFn runs one table against the generated list of every value of its database enum. The
// list comes from sqlc (emit_all_enum_values), so a new enum value in a migration shows up here
// without anyone editing this test.
type auditFn func() (missing, zero, extra []string)

func audited[S ~string, P comparable](t enumTable[S, P], all []S) auditFn {
	return func() ([]string, []string, []string) { return t.audit(all) }
}

// Every database enum value maps to a value that is not the zero (UNSPECIFIED) value, and the
// table maps nothing else. A new value in a migration fails here until its table has a row.
var _ = Describe("the database enum to proto enum tables", func() {
	DescribeTable("map every value",
		func(run auditFn) {
			missing, zero, extra := run()
			Expect(missing).To(BeEmpty(), "values with no row")
			Expect(zero).To(BeEmpty(), "values that map to the zero value")
			Expect(extra).To(BeEmpty(), "rows for values the database does not have")
		},
		Entry("task state", audited(taskStateToProto, store.AllTaskStateValues())),
		Entry("task actor kind", audited(actorKindToProto, store.AllTaskActorKindValues())),
		Entry("task creator kind", audited(creatorKindToProto, store.AllTaskCreatorKindValues())),
		Entry("run state", audited(runStateToProto, store.AllRunStateValues())),
		Entry("run failure class", audited(failureClassToProto, store.AllRunFailureClassValues())),
		Entry("run park reason", audited(parkReasonToProto, store.AllRunParkReasonValues())),
		Entry("approval status", audited(approvalStatusToProto, store.AllApprovalStatusValues())),
		Entry("approval decision", audited(approvalDecisionToProto, store.AllApprovalDecisionValues())),
		Entry("approval scope kind", audited(scopeKindToProto, store.AllApprovalScopeKindValues())),
		Entry("agent status", audited(agentStatusToProto, store.AllAgentStatusValues())),
		Entry("question state", audited(questionStateToProto, store.AllQuestionStateValues())),
		Entry("question answer kind", audited(answerKindToProto, store.AllQuestionAnswerKindValues())),
		Entry("connection status", audited(connectionStatusToProto, store.AllConnectionStatusValues())),
	)

	It("reports a missing row, a zero row and an extra row", func() {
		t := enumTable[store.TaskState, int]{store.TaskStateWorking: 0, "gone": 1}
		missing, zero, extra := t.audit([]store.TaskState{store.TaskStateSubmitted, store.TaskStateWorking})
		Expect(missing).To(Equal([]string{"submitted"}))
		Expect(zero).To(Equal([]string{"working"}))
		Expect(extra).To(Equal([]string{"gone"}))
	})
})
