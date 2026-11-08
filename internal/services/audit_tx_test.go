// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var _ = Describe("inAuditTx", func() {
	It("stamps a request ID on an event written from a bare RPC context", func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		pool := pgtest.StartTemplate(ctx).NewDB(ctx)
		org, user, _ := pgtest.NewOrg(ctx, pool)
		s := &Server{pool: pool}

		// A bare context, as the agent RPCs pass it: no request ID of its own.
		Expect(audit.RequestID(context.Background())).To(BeEmpty())
		err := s.inAuditTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{Org: org, Actor: audit.User(user), Action: "agent.create", TargetKind: "agent", TargetID: "a"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		evs, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(evs).To(HaveLen(1))
		Expect(evs[0].RequestID).NotTo(BeEmpty())
	}, SpecTimeout(3*time.Minute))
})
