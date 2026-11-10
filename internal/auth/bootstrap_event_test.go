// SPDX-License-Identifier: Apache-2.0

package auth

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5/pgtype"
)

var _ = Describe("deferredRevocationEvent", func() {
	It("marks the event in its after part, where the summary keeps it", func() {
		e := deferredRevocationEvent(pgtype.UUID{Valid: true}, 2)
		Expect(e.Action).To(Equal("org.bootstrap_sessions.revoked"))
		Expect(e.Before).To(BeNil())
		Expect(e.After).To(Equal(map[string]any{"revoked": int64(2), "installEventDeferred": true}))
	})
})
