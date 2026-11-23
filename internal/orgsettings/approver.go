// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The org setting "the approver must not be the requester" (issue #1175, ADR 0005 amendment
// 2026-11-23). It is off by default. When it is on, the person who created a task cannot approve the
// task's calls, and an approval scope does not cover the calls of tasks that the scope's creator
// requested (package approvals reads it). A second-person rule on the code host, such as two
// approvals on a protected branch, stays the org's own code-host setting.

// ApproverNotRequester reports whether the org has the setting on. It reads through q, so a caller
// in a transaction sees the value of that transaction.
func ApproverNotRequester(ctx context.Context, q *store.Queries, org pgtype.UUID) (bool, error) {
	st, err := Get(ctx, q, org, Defaults())
	if err != nil {
		return false, err
	}
	return st.ApproverNotRequester, nil
}

// ApproverNotRequester returns the org's setting.
func (s *Service) ApproverNotRequester(ctx context.Context, org pgtype.UUID) (bool, error) {
	return ApproverNotRequester(ctx, store.New(s.pool), org)
}

// SetApproverNotRequester turns the setting on or off. A change is audited as
// org.approver_not_requester.set; setting the current value again records nothing.
func (s *Service) SetApproverNotRequester(ctx context.Context, org pgtype.UUID, actor audit.Actor, on bool) error {
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		before, err := ApproverNotRequester(ctx, q, org)
		if err != nil {
			return err
		}
		if err := q.SetOrgApproverNotRequester(ctx, store.SetOrgApproverNotRequesterParams{OrgID: org, ApproverNotRequester: on}); err != nil {
			return err
		}
		if before == on {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "org.approver_not_requester.set", TargetKind: "org", TargetID: org.String(),
			Before: map[string]any{"on": before}, After: map[string]any{"on": on},
		})
		return nil
	})
}
