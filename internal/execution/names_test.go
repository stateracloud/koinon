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
