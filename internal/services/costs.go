// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/stateracloud/koinon/internal/ledger"
)

// Spend totals from the spend ledger (plan item L1).

func costProto(requests, in, out int64, usd string, priced bool) *runv1alpha1.Cost {
	return &runv1alpha1.Cost{
		Requests: requests, InputTokens: in, OutputTokens: out, Usd: ledger.FormatUSD(usd), Priced: priced,
	}
}

func noCost() *runv1alpha1.Cost { return costProto(0, 0, 0, "0", true) }

// setRunCosts sets Cost on each run.
func setRunCosts(ctx context.Context, q *store.Queries, org pgtype.UUID, runs []*runv1alpha1.Run) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(runs))
	for _, r := range runs {
		var id pgtype.UUID
		if err := id.Scan(r.GetId()); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	rows, err := q.SumRunCosts(ctx, store.SumRunCostsParams{OrgID: org, RunIds: ids})
	if err != nil {
		return err
	}
	byID := map[string]*runv1alpha1.Cost{}
	for _, row := range rows {
		byID[idString(row.RunID)] = costProto(row.Requests, row.InputTokens, row.OutputTokens, row.CostUsd, row.Priced)
	}
	for _, r := range runs {
		if c, ok := byID[r.GetId()]; ok {
			r.Cost = c
		} else {
			r.Cost = noCost()
		}
	}
	return nil
}

// setTicketCosts sets Cost on each ticket.
func setTicketCosts(ctx context.Context, q *store.Queries, org pgtype.UUID, tickets []*ticketv1alpha1.Ticket) error {
	if len(tickets) == 0 {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(tickets))
	for _, t := range tickets {
		var id pgtype.UUID
		if err := id.Scan(t.GetId()); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	rows, err := q.SumTicketCosts(ctx, store.SumTicketCostsParams{OrgID: org, TicketIds: ids})
	if err != nil {
		return err
	}
	byID := map[string]*runv1alpha1.Cost{}
	for _, row := range rows {
		byID[idString(row.TicketID)] = costProto(row.Requests, row.InputTokens, row.OutputTokens, row.CostUsd, row.Priced)
	}
	for _, t := range tickets {
		if c, ok := byID[t.GetId()]; ok {
			t.Cost = c
		} else {
			t.Cost = noCost()
		}
	}
	return nil
}
