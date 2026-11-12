// SPDX-License-Identifier: Apache-2.0

package properties_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

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
	parked                // parked with unanswered questions, or not yet resolved
	resolved              // parked and resolved: claimable once
	done
)

type parkRun struct {
	id        pgtype.UUID
	agent     pgtype.UUID
	task      pgtype.UUID
	phase     phase
	run       store.Run // the owner's latest view
	parks     int
	resumes   int
	lastEpoch int64
	open      map[pgtype.UUID]bool // questions of this park nobody answered yet
}

var farFuture = time.Now().Add(24 * time.Hour)

var _ = Describe("park and resume (issue 26)", func() {
	// Property: for any interleaving of parks, answers (repeated, racing), owner restarts and
	// claims, a parked run is claimable only after the last question of its park is answered, each
	// park resumes the run exactly once under a newer epoch, a stale owner changes nothing, and
	// every run ends.
	It("resumes each run exactly once across random restarts", func(ctx SpecContext) {
		propseed.ForEach(func(rng *rand.Rand) {
			org, user := newOrg(ctx)
			var order []*parkRun
			byID := map[pgtype.UUID]*parkRun{}
			var agents []pgtype.UUID
			for i := 1 + rng.Intn(5); i > 0; i-- {
				// Some runs share an agent: an agent runs one run at a time.
				if len(agents) == 0 || rng.Intn(3) > 0 {
					agents = append(agents, newAgent(ctx, org, user))
				}
				agent := agents[rng.Intn(len(agents))]
				task := newTask(ctx, org, user, agent)
				r, err := q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: agent, TaskID: task.ID, MaxWallClockSeconds: 1800})
				Expect(err).NotTo(HaveOccurred())
				pr := &parkRun{id: r.ID, agent: agent, task: task.ID, phase: queued, open: map[pgtype.UUID]bool{}}
				byID[r.ID] = pr
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
			// claimable counts the runs a claim may take: waiting, and their agent holds no run.
			claimable := func() int {
				busy := map[pgtype.UUID]bool{}
				for _, r := range order {
					if r.phase == held {
						busy[r.agent] = true
					}
				}
				n := 0
				for _, r := range order {
					if (r.phase == queued || r.phase == resolved) && !busy[r.agent] {
						n++
					}
				}
				return n
			}

			claim := func() bool {
				GinkgoHelper()
				run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "a", LeaseSeconds: 60})
				if errors.Is(err, pgx.ErrNoRows) {
					Expect(claimable()).To(BeZero(), "a claimable run was not claimed")
					return false
				}
				Expect(err).NotTo(HaveOccurred())
				pr := byID[run.ID]
				Expect(pr).NotTo(BeNil())
				Expect(claimable()).To(BeNumerically(">", 0))
				Expect(pr.phase).To(Or(Equal(queued), Equal(resolved)), "claimed a run in phase %d: a parked run resumed before its questions were answered, or a held run was claimed twice", pr.phase)
				if pr.phase == resolved {
					pr.resumes++
				}
				Expect(run.Epoch).To(BeNumerically(">", pr.lastEpoch))
				run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: run.ID, Epoch: run.Epoch})
				Expect(err).NotTo(HaveOccurred())
				pr.phase, pr.run, pr.lastEpoch = held, run, run.Epoch
				return true
			}
			// park parks the run with 0-3 questions for a person (reason input_required; with none
			// the run is resolvable at once, as an approval-only park with nothing open is).
			park := func(pr *parkRun) {
				GinkgoHelper()
				reason := store.RunParkReasonInputRequired
				if rng.Intn(3) == 0 {
					reason = store.RunParkReasonApproval
				}
				for n := rng.Intn(4); n > 0; n-- {
					sum := sha256.Sum256([]byte(fmt.Sprintf("%x/%d/%d", pr.id.Bytes, pr.parks, n)))
					qu, err := q.CreateQuestion(ctx, store.CreateQuestionParams{
						OrgID: org, TaskID: pr.task, RunID: pr.id, AgentID: pr.agent,
						TargetKind: store.NotifyTargetKindUser, TargetUserID: user, Question: "q?", Choices: []byte(`[]`),
						DefaultAnswer: "d", CallSha256: hex.EncodeToString(sum[:]),
						ExpiresAt: pgtype.Timestamptz{Time: farFuture, Valid: true},
					})
					Expect(err).NotTo(HaveOccurred())
					// asked -> open, as the notifier does once it reached a person.
					_, err = pool.Exec(ctx, `UPDATE questions SET state = 'open', opened_at = now() WHERE org_id = $1 AND id = $2`, org, qu.ID)
					Expect(err).NotTo(HaveOccurred())
					pr.open[qu.ID] = true
				}
				_, err := q.ParkRun(ctx, store.ParkRunParams{OrgID: org, ID: pr.id, Epoch: pr.run.Epoch, ParkReason: reason})
				Expect(err).NotTo(HaveOccurred())
				pr.phase, pr.parks = parked, pr.parks+1
			}
			// answer answers a random subset of the open questions, some twice, from racing
			// transactions that each call runpark.Resolve as the question service does; it also
			// calls Resolve on every run, as the approval service may.
			answer := func(all bool) {
				GinkgoHelper()
				type job struct {
					run *parkRun
					qid pgtype.UUID // zero: just Resolve
				}
				var jobs []job
				for _, pr := range order {
					jobs = append(jobs, job{run: pr})
					for qid := range pr.open {
						if all || rng.Intn(2) == 0 {
							for n := 1 + rng.Intn(2); n > 0; n-- {
								jobs = append(jobs, job{run: pr, qid: qid})
							}
							delete(pr.open, qid)
						}
					}
				}
				rng.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
				var (
					wg   sync.WaitGroup
					mu   sync.Mutex
					errs []error
				)
				for _, j := range jobs {
					wg.Add(1)
					go func() {
						defer wg.Done()
						err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
							tq := store.New(tx)
							if j.qid.Valid {
								// Lock the run first, as the services do, then answer; a second answer
								// of the same question matches no row.
								if _, err := tq.LockRun(ctx, store.LockRunParams{OrgID: org, ID: j.run.id}); err != nil {
									return err
								}
								_, err := tq.ResolveQuestion(ctx, store.ResolveQuestionParams{
									OrgID: org, ID: j.qid, State: store.QuestionStateAnswered,
									Answer: pgtype.Text{String: "a", Valid: true}, AnswerKind: store.QuestionAnswerKindText, AnsweredBy: user,
								})
								if err != nil && !errors.Is(err, pgx.ErrNoRows) {
									return err
								}
							}
							return runpark.Resolve(ctx, tq, org, j.run.id)
						})
						if err != nil {
							mu.Lock()
							errs = append(errs, err)
							mu.Unlock()
						}
					}()
				}
				wg.Wait()
				Expect(errs).To(BeEmpty())
				for _, pr := range order {
					if pr.phase == parked && len(pr.open) == 0 {
						pr.phase = resolved
					}
					got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: pr.id})
					Expect(err).NotTo(HaveOccurred())
					Expect(got.ReadyAt.Valid).To(Equal(pr.phase == queued || pr.phase == resolved),
						"run in phase %d with %d open questions has ready_at set = %v", pr.phase, len(pr.open), got.ReadyAt.Valid)
				}
			}
			restart := func(pr *parkRun) {
				GinkgoHelper()
				old := pr.run
				_, err := q.ReleaseRunLease(ctx, store.ReleaseRunLeaseParams{OrgID: org, ID: pr.id, Epoch: old.Epoch})
				Expect(err).NotTo(HaveOccurred())
				taken, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: org, OwnerID: "b", LeaseSeconds: 60})
				Expect(err).NotTo(HaveOccurred())
				Expect(taken.ID).To(Equal(pr.id))
				Expect(taken.Epoch).To(BeNumerically(">", pr.lastEpoch))
				expectFenced(ctx, org, old)
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
					answer(false)
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

			// Drain: answer everything, end what is held, and claim until nothing waits.
			for {
				answer(true)
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
