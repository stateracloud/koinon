// SPDX-License-Identifier: Apache-2.0

package properties_test

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/testing/propseed"
)

// event is one delivery: the same (source, key) may arrive many times (redelivery, a re-poll, a
// second replica).
type event struct{ source, key string }

var sources = []string{durable.SourceWebhook, durable.SourceSlack, durable.SourcePoll, durable.SourceDirectMessage, durable.SourceQuestion}

// genEvents draws a multiset of deliveries with duplicates; it returns the events and the number
// of distinct (source, key) pairs.
func genEvents(rng *rand.Rand) ([]event, int) {
	distinct := map[event]bool{}
	var evs []event
	for n := 5 + rng.Intn(40); n > 0; n-- {
		// A small key space makes duplicates and cross-source key reuse common.
		e := event{source: sources[rng.Intn(len(sources))], key: fmt.Sprintf("k%d", rng.Intn(12))}
		evs = append(evs, e)
		distinct[e] = true
	}
	return evs, len(distinct)
}

// outcome is what a world looks like after all deliveries: tasks per correlation key, by title.
func tasksByKey(ctx context.Context, org pgtype.UUID) map[string]int {
	GinkgoHelper()
	rows, err := pool.Query(ctx, `SELECT title FROM tasks WHERE org_id = $1`, org)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var t string
		Expect(rows.Scan(&t)).To(Succeed())
		got[t]++
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return got
}

// deliver runs one delivery the way every source does: durable.Once whose effect creates the
// task the event correlates to. It returns the task the event reached.
func deliver(ctx context.Context, org, user, agent pgtype.UUID, e event) (pgtype.UUID, error) {
	var out durable.Outcome
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		out, err = durable.Once(ctx, store.New(tx), org, e.source, e.key, func(q *store.Queries) (string, pgtype.UUID, error) {
			t, err := q.CreateTask(ctx, store.CreateTaskParams{
				OrgID: org, Title: e.source + "/" + e.key, AgentID: agent,
				CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user,
			})
			return "task", t.ID, err
		})
		return err
	})
	return out.EffectID, err
}

var _ = Describe("correlation (issue 26)", func() {
	// Property: each event reaches exactly one task by its correlation key, and the result is the
	// same for any arrival order of the same events, sequential or concurrent.
	It("gives the same result for any arrival order", func(ctx SpecContext) {
		propseed.ForEach(func(rng *rand.Rand) {
			evs, distinct := genEvents(rng)

			world := func() (org, user, agent pgtype.UUID) {
				org, user = newOrg(ctx)
				return org, user, newAgent(ctx, org, user)
			}

			// World A: one shuffled order, one at a time.
			orgA, userA, agentA := world()
			a := append([]event(nil), evs...)
			rng.Shuffle(len(a), func(i, j int) { a[i], a[j] = a[j], a[i] })
			reached := map[event]pgtype.UUID{}
			for _, e := range a {
				id, err := deliver(ctx, orgA, userA, agentA, e)
				Expect(err).NotTo(HaveOccurred())
				if prev, seen := reached[e]; seen {
					Expect(id).To(Equal(prev), "a replay of %v must reach the task of its first delivery", e)
				}
				reached[e] = id
			}

			// World B: another shuffle, all deliveries racing.
			orgB, userB, agentB := world()
			b := append([]event(nil), evs...)
			rng.Shuffle(len(b), func(i, j int) { b[i], b[j] = b[j], b[i] })
			var (
				wg  sync.WaitGroup
				mu  sync.Mutex
				ids = map[event]map[pgtype.UUID]bool{}
			)
			for _, e := range b {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					id, err := deliver(ctx, orgB, userB, agentB, e)
					Expect(err).NotTo(HaveOccurred())
					mu.Lock()
					defer mu.Unlock()
					if ids[e] == nil {
						ids[e] = map[pgtype.UUID]bool{}
					}
					ids[e][id] = true
				}()
			}
			wg.Wait()
			for e, set := range ids {
				Expect(set).To(HaveLen(1), "racing deliveries of %v reached different tasks", e)
			}

			gotA, gotB := tasksByKey(ctx, orgA), tasksByKey(ctx, orgB)
			Expect(gotA).To(HaveLen(distinct))
			for k, n := range gotA {
				Expect(n).To(Equal(1), "key %s reached %d tasks", k, n)
			}
			Expect(gotB).To(Equal(gotA), "arrival order changed the result: keys %v vs %v", keys(gotA), keys(gotB))
		})
	})
})

func keys(m map[string]int) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
