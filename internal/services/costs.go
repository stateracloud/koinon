// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	"github.com/stateracloud/koinon/internal/ledger"
)

// Spend totals from the spend ledger (plan item L1).

// tokenSplit is the tokens of a total by type (ADR 0081): InputTokens of the proto stays the total
// input, cache reads and writes included.
type tokenSplit struct{ cacheRead, cacheWrite, reasoning int64 }

func costProto(requests, in, out int64, split tokenSplit, usd string, priced bool, estimated int64) *runv1alpha1.Cost {
	return &runv1alpha1.Cost{
		Requests: requests, InputTokens: in, OutputTokens: out, Usd: ledger.FormatUSD(usd), Priced: priced, EstimatedRequests: estimated,
		CacheReadTokens: split.cacheRead, CacheWriteTokens: split.cacheWrite, ReasoningTokens: split.reasoning,
	}
}

func noCost() *runv1alpha1.Cost { return costProto(0, 0, 0, tokenSplit{}, "0", true, 0) }

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
		byID[idString(row.RunID)] = costProto(row.Requests, row.InputTokens, row.OutputTokens, tokenSplit{row.CacheReadTokens, row.CacheWriteTokens, row.ReasoningTokens}, row.CostUsd, row.Priced, row.EstimatedRequests)
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

// sumTaskCosts returns the spend of each task in ids that has some, by task ID.
func sumTaskCosts(ctx context.Context, q *store.Queries, org pgtype.UUID, ids []pgtype.UUID) (map[string]*runv1alpha1.Cost, error) {
	byID := map[string]*runv1alpha1.Cost{}
	if len(ids) == 0 {
		return byID, nil
	}
	rows, err := q.SumTaskCosts(ctx, store.SumTaskCostsParams{OrgID: org, TaskIds: ids})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		byID[idString(row.TaskID)] = costProto(row.Requests, row.InputTokens, row.OutputTokens, tokenSplit{row.CacheReadTokens, row.CacheWriteTokens, row.ReasoningTokens}, row.CostUsd, row.Priced, row.EstimatedRequests)
	}
	return byID, nil
}

// costOrNone is the cost of id, or a zero cost.
func costOrNone(costs map[string]*runv1alpha1.Cost, id string) *runv1alpha1.Cost {
	if c, ok := costs[id]; ok {
		return c
	}
	return noCost()
}
