// SPDX-License-Identifier: Apache-2.0

package properties_test

import (
	"errors"
	"math/rand"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runpark"
	"github.com/stateracloud/koinon/internal/testing/propseed"
)

// phase is where the model puts a run.
type phase int

const (
	queued   phase = iota // waiting for a first claim
	held                  // an agentd owns it (starting or running)
	parked                // parked, nothing resolved it yet
	resolved              // parked and resolved: claimable once
	done
)

type parkRun struct {
	id           pgtype.UUID
	phase        phase
	run          store.Run // the owner's latest view
	parks        int
	resumes      int
	lastEpoch    int64
	resolveCalls int
}

var _ = Describe("park and resume (issue 26)", func() {
	// Property: for any interleaving of parks, resolutions (repeated, racing), owner restarts and
	// claims, a parked run is claimable only after its park resolved, and each park resumes the
	// run exactly once, under a strictly newer epoch.
	It("resumes each run exactly once across random restarts", func(ctx SpecContext) {
		propseed.ForEach(func(rng *rand.Rand) {
			org, user := newOrg(ctx)
			runs := map[pgtype.UUID]*parkRun{}
			var order []*parkRun
			for i := 1 + rng.Intn(4); i > 0; i-- {
				agent := newAgent(ctx, org, user)
				task := newTask(ctx, org, user, agent)
				r, err := q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: agent, TaskID: task.ID, MaxWallClockSeconds: 1800})
				Expect(err).NotTo(HaveOccurred())
				pr := &parkRun{id: r.ID, phase: queued}
				runs[r.ID] = pr
				order = append(order, pr)
			}
			pick := func(ph phase) *parkRun {
				var c []*parkRun
				for _, r := range order {
					if r.phase == ph {
						c = append(c, r)
					}
				}
				if len(c) == 0 {
					return nil
				}
				return c[rng.Intn(len(c))]
			}

			claim := func() {
				GinkgoHelper()
				run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "a", LeaseSeconds: 60})
				if errors.Is(err, pgx.ErrNoRows) {
					// Nothing is claimable: the model must agree.
					Expect(pick(queued)).To(BeNil(), "a queued run was not claimable")
					Expect(pick(resolved)).To(BeNil(), "a resolved park was not claimable")
					return
				}
				Expect(err).NotTo(HaveOccurred())
				pr := runs[run.ID]
				Expect(pr).NotTo(BeNil())
				Expect(pr.phase).To(Or(Equal(queued), Equal(resolved)), "claimed a run in phase %d: a parked run resumed before its park resolved, or a held run was claimed twice", pr.phase)
				if pr.phase == resolved {
					pr.resumes++
				}
				Expect(run.Epoch).To(BeNumerically(">", pr.lastEpoch))
				run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: run.ID, Epoch: run.Epoch})
				Expect(err).NotTo(HaveOccurred())
				pr.phase, pr.run, pr.lastEpoch = held, run, run.Epoch
			}
			park := func(pr *parkRun) {
				GinkgoHelper()
				reason := []store.RunParkReason{store.RunParkReasonApproval, store.RunParkReasonInputRequired}[rng.Intn(2)]
				_, err := q.ParkRun(ctx, store.ParkRunParams{OrgID: org, ID: pr.id, Epoch: pr.run.Epoch, ParkReason: reason})
				Expect(err).NotTo(HaveOccurred())
				pr.phase, pr.parks = parked, pr.parks+1
			}
			resolve := func() {
				GinkgoHelper()
				// Resolve every run, parked or not, some of them more than once and racing: the
				// approval and the question service both resolve a park, and either may repeat.
				var wg sync.WaitGroup
				for _, pr := range order {
					for n := 1 + rng.Intn(3); n > 0; n-- {
						wg.Add(1)
						go func(id pgtype.UUID) {
							defer GinkgoRecover()
							defer wg.Done()
							Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
								return runpark.Resolve(ctx, store.New(tx), org, id)
							})).To(Succeed())
						}(pr.id)
					}
				}
				wg.Wait()
				for _, pr := range order {
					if pr.phase == parked {
						pr.phase = resolved
					}
					got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: pr.id})
					Expect(err).NotTo(HaveOccurred())
					Expect(got.ReadyAt.Valid).To(Equal(pr.phase == queued || pr.phase == resolved), "run in phase %d has ready_at set = %v", pr.phase, got.ReadyAt.Valid)
				}
			}
			restart := func(pr *parkRun) {
				GinkgoHelper()
				_, err := q.ReleaseRunLease(ctx, store.ReleaseRunLeaseParams{OrgID: org, ID: pr.id, Epoch: pr.run.Epoch})
				Expect(err).NotTo(HaveOccurred())
				taken, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: org, OwnerID: "b", LeaseSeconds: 60})
				Expect(err).NotTo(HaveOccurred())
				Expect(taken.ID).To(Equal(pr.id))
				Expect(taken.Epoch).To(BeNumerically(">", pr.lastEpoch))
				pr.run, pr.lastEpoch = taken, taken.Epoch
			}
			finish := func(pr *parkRun) {
				GinkgoHelper()
				_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: org, ID: pr.id, Epoch: pr.run.Epoch, Outcome: store.RunStateSucceeded})
				Expect(err).NotTo(HaveOccurred())
				pr.phase = done
			}

			for step := 0; step < 25+rng.Intn(40); step++ {
				switch op := rng.Intn(10); {
				case op < 3:
					claim()
				case op < 5:
					if pr := pick(held); pr != nil {
						park(pr)
					}
				case op < 7:
					resolve()
				case op < 9:
					if pr := pick(held); pr != nil {
						restart(pr)
					}
				default:
					if pr := pick(held); pr != nil {
						finish(pr)
					}
				}
			}

			// Drain: resolve everything, end what is held, and claim until nothing waits.
			for {
				resolve()
				waiting := 0
				for _, pr := range order {
					switch pr.phase {
					case held:
						finish(pr)
					case queued, resolved:
						waiting++
					}
				}
				if waiting == 0 {
					break
				}
				claim()
			}
			for _, pr := range order {
				Expect(pr.phase).To(Equal(done))
				Expect(pr.resumes).To(Equal(pr.parks), "run parked %d times, resumed %d", pr.parks, pr.resumes)
				got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: pr.id})
				Expect(err).NotTo(HaveOccurred())
				Expect(got.State).To(Equal(store.RunStateSucceeded))
			}
		})
	})
})
