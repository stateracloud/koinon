// SPDX-License-Identifier: Apache-2.0

package execution_test

import (
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
)

var _ = Describe("UUIDString", func() {
	It("formats a UUID in canonical lower-case form", func() {
		var u pgtype.UUID
		Expect(u.Scan("0198A1B2-C3D4-7E5F-8A9B-0C1D2E3F4A5B")).To(Succeed())
		Expect(execution.UUIDString(u.Bytes)).To(Equal("0198a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"))
		Expect(execution.UUIDString([16]byte{})).To(Equal("00000000-0000-0000-0000-000000000000"))
	})
})

var _ = Describe("TaskSandboxName", func() {
	It("is stable per task, differs per task and agent, and fits OpenShell's name limit", func() {
		org, agent := [16]byte{1}, [16]byte{2}
		a, b := [16]byte{3}, [16]byte{4}
		Expect(execution.TaskSandboxName(org, agent, a)).To(Equal(execution.TaskSandboxName(org, agent, a)))
		Expect(execution.TaskSandboxName(org, agent, a)).NotTo(Equal(execution.TaskSandboxName(org, agent, b)))
		Expect(execution.TaskSandboxName(org, agent, a)).NotTo(Equal(execution.TaskSandboxName(org, [16]byte{9}, a)))
		Expect(execution.TaskSandboxName(org, agent, a)).NotTo(Equal(execution.AgentSandboxName(org, agent)))
		Expect(execution.TaskSandboxName(org, agent, a)).To(HaveLen(execution.MaxSandboxName))
	})
})
