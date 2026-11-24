// SPDX-License-Identifier: Apache-2.0

package forgecredfile_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/forgecredfile"
	"github.com/stateracloud/koinon/internal/secretstore"
)

func TestForgecredfile(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Forgecredfile Suite")
}

func uuid(s string) pgtype.UUID {
	var u pgtype.UUID
	Expect(u.Scan(s)).To(Succeed())
	return u
}

var _ = Describe("forge credential file names", func() {
	org := "0b9f1a2c-1111-4c2d-8e3f-000000000001"
	agent := "0b9f1a2c-2222-4c2d-8e3f-000000000002"

	It("round-trips the org ID and the agent ID", func() {
		name := forgecredfile.SealedName(uuid(org), uuid(agent), []byte(`{"format":"v2"}`))
		Expect(forgecredfile.IsSealed(name)).To(BeTrue())
		o, a, ok := forgecredfile.ParseSealed(name)
		Expect(ok).To(BeTrue())
		Expect(o).To(Equal(org))
		Expect(a).To(Equal(agent))
	})

	It("changes the name when the envelope changes", func() {
		Expect(forgecredfile.SealedName(uuid(org), uuid(agent), []byte("a"))).
			NotTo(Equal(forgecredfile.SealedName(uuid(org), uuid(agent), []byte("b"))))
	})

	DescribeTable("refuses other names",
		func(name string) {
			_, _, ok := forgecredfile.ParseSealed(name)
			Expect(ok).To(BeFalse())
		},
		Entry("an MCP file", "mcpv2-"+org+"-"+agent+"-0a1b2c3d"),
		Entry("no digest", "forgev2-"+org+"-"+agent),
		Entry("a digest that is not hex", "forgev2-"+org+"-"+agent+"-0a1b2c3G"),
		Entry("an ID that is not a UUID", "forgev2-"+org+"-"+"zzzzzzzz-2222-4c2d-8e3f-000000000002"+"-0a1b2c3d"),
		Entry("a path", "../forgev2-"+org+"-"+agent+"-0a1b2c3d"),
	)

	It("binds the token to its org and agent", func() {
		Expect(forgecredfile.Binding(org, agent)).To(Equal(secretstore.Binding{
			OrgID: org, Table: "agents", RowID: agent, Column: "forge_token",
		}))
	})
})
