// SPDX-License-Identifier: Apache-2.0

package services

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// enumValues returns the values of the database enum the store package declares as the Go type
// typeName. sqlc generates these constants from the migrations, so a new enum value shows up here
// without anyone editing this test.
func enumValues(typeName string) []string {
	GinkgoHelper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../db/store/models.go", nil, 0)
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != typeName || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			Expect(ok).To(BeTrue(), "%s: constant %s is not a literal", typeName, vs.Names[0].Name)
			v, err := strconv.Unquote(lit.Value)
			Expect(err).NotTo(HaveOccurred())
			out = append(out, v)
		}
	}
	Expect(out).NotTo(BeEmpty(), "no constants of type %s in the store package", typeName)
	return out
}

// Every database enum value maps to a proto value that is not the zero (UNSPECIFIED) value, and the
// table maps nothing else. A new value in a migration fails here until its table has a row.
var _ = Describe("the database enum to proto enum tables", func() {
	DescribeTable("map every value",
		func(typeName string, table any) {
			t := reflect.ValueOf(table)
			want := enumValues(typeName)
			got := map[string]reflect.Value{}
			for _, k := range t.MapKeys() {
				got[k.String()] = t.MapIndex(k)
			}
			for _, v := range want {
				m, ok := got[v]
				Expect(ok).To(BeTrue(), "%s value %q has no row", typeName, v)
				Expect(m.IsZero()).To(BeFalse(), "%s value %q maps to the zero value (%v)", typeName, v, m.Interface())
				delete(got, v)
			}
			Expect(got).To(BeEmpty(), "%s table has rows for values the database does not have", typeName)
		},
		Entry("task state", "TaskState", taskStateToProto),
		Entry("task actor kind", "TaskActorKind", actorKindToProto),
		Entry("task creator kind", "TaskCreatorKind", creatorKindToProto),
		Entry("run state", "RunState", runStateToProto),
		Entry("run failure class", "RunFailureClass", failureClassToProto),
		Entry("run park reason", "RunParkReason", parkReasonToProto),
		Entry("approval status", "ApprovalStatus", approvalStatusToProto),
		Entry("approval decision", "ApprovalDecision", approvalDecisionToProto),
		Entry("approval scope kind", "ApprovalScopeKind", scopeKindToProto),
		Entry("agent status", "AgentStatus", agentStatusToProto),
		Entry("question state", "QuestionState", questionStateToProto),
		Entry("question answer kind", "QuestionAnswerKind", answerKindToProto),
		Entry("connection status", "ConnectionStatus", connectionStatusToProto),
		Entry("ticket column", "TaskState", ticketColumn),
		Entry("ticket column name", "TaskState", ticketColumnName),
		Entry("ticket actor kind", "TaskActorKind", ticketActorKind),
	)
})
