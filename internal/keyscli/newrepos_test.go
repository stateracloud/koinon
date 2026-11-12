// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// keys new-repositories (issue #86, ADR 0085 decision 14): after a compromise, every agent named
// gets a new home repository that koinon-agentd seeds with its newest checkpoint.
var _ = Describe("koinon-api keys new-repositories", func() {
	const oldKEK = "kek-20260101-aaaa"
	var (
		newKEK string
		kube   *fake.Clientset
		tool   *keyscli.Tool
		out    *bytes.Buffer
		now    time.Time
		q      *store.Queries
		pw     checkpoint.Passwords
	)

	ring := func(ctx SpecContext) *secretstore.Keyring {
		GinkgoHelper()
		set, err := tool.Store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		r, err := secretstore.NewKeyring(set.Keys, set.Active)
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	usage := func(ctx SpecContext) map[string]int {
		GinkgoHelper()
		u, err := tool.Usage(ctx)
		Expect(err).NotTo(HaveOccurred())
		return u
	}
	newAgent := func(ctx SpecContext, org pgtype.UUID) pgtype.UUID {
		GinkgoHelper()
		var a pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, $2, 'opencode') RETURNING id`, org, pgtest.Uniq("a")).Scan(&a)).To(Succeed())
		return a
	}
	// withRepository makes an agent with a home repository (epoch 0) under the active KEK.
	withRepository := func(ctx SpecContext, org pgtype.UUID) pgtype.UUID {
		GinkgoHelper()
		a := newAgent(ctx, org)
		_, err := pw.Current(ctx, org, a)
		Expect(err).NotTo(HaveOccurred())
		return a
	}
	latest := func(ctx SpecContext, org, agent pgtype.UUID) store.HomeRepositoryEpoch {
		GinkgoHelper()
		e, err := q.LatestHomeRepositoryEpoch(ctx, store.LatestHomeRepositoryEpochParams{OrgID: org, AgentID: agent})
		Expect(err).NotTo(HaveOccurred())
		return e
	}
	epochs := func(ctx SpecContext) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM home_repository_epochs`).Scan(&n)).To(Succeed())
		return n
	}
	// compromise activates a new KEK; the old one stays in the key set (it leaked, it is not lost).
	compromise := func(ctx SpecContext) {
		GinkgoHelper()
		Expect(tool.Add(ctx, newKEK)).To(Succeed())
		Expect(tool.Activate(ctx, newKEK)).To(Succeed())
		now = now.Add(6 * time.Minute)
		pw.Keys = ring(ctx)
	}

	BeforeEach(func(ctx SpecContext) {
		pgtest.TruncateOrgs(ctx, pool)
		_, err := pool.Exec(ctx, `DELETE FROM kek_fingerprints`)
		Expect(err).NotTo(HaveOccurred())
		now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		kube = fake.NewClientset()
		key := make([]byte, secretstore.KeySize)
		_, _ = rand.Read(key)
		_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
			Data:       map[string][]byte{oldKEK: secretstore.EncodeKey(key), "active": []byte(oldKEK)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		out = &bytes.Buffer{}
		newKEK = freshKEK("n")
		tool = &keyscli.Tool{Pool: pool, Store: keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek"}, Out: out, Now: func() time.Time { return now }, ProbeTimeout: 5 * time.Second, ProbePoll: 10 * time.Millisecond}
		q = store.New(pool)
		pw = checkpoint.Passwords{Q: q, Keys: ring(ctx), Begin: pool.Begin}
		freeStorage()
	})

	It("wants exactly one of --all and --agent, valid agent IDs that have a repository, and a database", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		agent := withRepository(ctx, org)
		bare := newAgent(ctx, org)
		tool.Yes = true

		Expect(tool.NewRepositories(ctx, false, nil)).To(MatchError(ContainSubstring("--agent, or pass --all")))
		Expect(tool.NewRepositories(ctx, true, []string{agent.String()})).To(MatchError(ContainSubstring("--agent, or pass --all")))
		Expect(tool.NewRepositories(ctx, false, []string{"nope"})).To(MatchError(ContainSubstring(`"nope" is not an agent ID`)))
		Expect(tool.NewRepositories(ctx, false, []string{agent.String(), bare.String()})).To(MatchError(And(
			ContainSubstring("no home repository for agent(s) "+bare.String()), ContainSubstring("nothing was changed"))))
		Expect(epochs(ctx)).To(BeZero(), "one agent without a repository refuses the whole command")

		noDB := *tool
		noDB.Pool = nil
		Expect(noDB.NewRepositories(ctx, true, nil)).To(MatchError(ContainSubstring("needs --database-url")))
		Expect(epochs(ctx)).To(BeZero())
	})

	It("has nothing to replace when no agent has a repository", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		newAgent(ctx, org)
		Expect(tool.NewRepositories(ctx, true, nil)).To(Succeed(), "nothing to confirm, so no --yes needed")
		Expect(out.String()).To(ContainSubstring("No agent has a home repository; nothing to replace."))
		Expect(epochs(ctx)).To(BeZero())
	})

	It("starts a new epoch for every agent with a repository, sealed with the active KEK, asks for its seed and audits each agent", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		other, _, _ := pgtest.NewOrg(ctx, pool)
		a1, a2 := withRepository(ctx, org), withRepository(ctx, org)
		_, _, err := pw.NewEpoch(ctx, org, a2, checkpoint.ReasonKeyLost, checkpoint.DefaultRetain) // a2 is at epoch 1
		Expect(err).NotTo(HaveOccurred())
		b := withRepository(ctx, other)
		bare := newAgent(ctx, org)
		Expect(usage(ctx)).To(Equal(map[string]int{oldKEK: 4}))

		compromise(ctx)
		tool.Yes = true
		start := time.Now()
		Expect(tool.NewRepositories(ctx, true, nil)).To(Succeed())
		Expect(out.String()).To(And(
			ContainSubstring("Started a new home repository for 3 agent(s) in 2 org(s)."),
			ContainSubstring(`koinon_agentd_home_repository_seeds{state="pending"}`),
			ContainSubstring("Rotate the checkpoint bucket's S3 key"),
		))
		Expect(usage(ctx)).To(Equal(map[string]int{oldKEK: 4, newKEK: 3}), "each new password is sealed with the active KEK")

		By("each agent's new epoch asks to be seeded from the one before, which is retired with a deadline of now")
		for _, c := range []struct {
			org, agent pgtype.UUID
			epoch      int32
		}{{org, a1, 1}, {org, a2, 2}, {other, b, 1}} {
			e := latest(ctx, c.org, c.agent)
			Expect(e.RepositoryEpoch).To(Equal(c.epoch))
			Expect(e.Reason).To(Equal(checkpoint.ReasonKeyCompromised))
			Expect(e.SeedFromEpoch).To(Equal(pgtype.Int4{Int32: c.epoch - 1, Valid: true}))
			Expect(e.SeedAfter.Valid).To(BeTrue())
			Expect(e.SeedAfter.Time).To(BeTemporally("<=", time.Now()))
			Expect(e.SeededAt.Valid || e.SeedFailedAt.Valid).To(BeFalse())
			Expect(e.SeedAttempts).To(BeZero())
			cur, err := pw.Current(ctx, c.org, c.agent)
			Expect(err).NotTo(HaveOccurred(), "the new password opens with the key set")
			Expect(cur.Epoch).To(Equal(c.epoch))

			var deleteAfter time.Time
			if c.epoch == 1 {
				h, err := q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: c.org, AgentID: c.agent})
				Expect(err).NotTo(HaveOccurred())
				deleteAfter = h.DeleteAfter.Time
			} else {
				Expect(pool.QueryRow(ctx, `SELECT delete_after FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = $3`,
					c.org, c.agent, c.epoch-1).Scan(&deleteAfter)).To(Succeed())
			}
			Expect(deleteAfter).To(BeTemporally("~", start, time.Minute), "no retention: the sweeper's guard keeps it until the seed is there")
		}
		h, err := q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: a2})
		Expect(err).NotTo(HaveOccurred())
		Expect(h.DeleteAfter.Time).To(BeTemporally("~", start, time.Minute),
			"a2's epoch 0, retired earlier with a retention, may be under an exposed password too: due now as well")
		_, err = q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: bare})
		Expect(err).To(HaveOccurred(), "an agent without a repository gets its first one under the active KEK, later")

		By("one audit event per agent, in the agent's org, by the cli actor")
		rows, err := pool.Query(ctx, `SELECT org_id, actor_kind, actor_id, target_kind, target_id, summary::text FROM audit_events WHERE action = $1 ORDER BY target_id`, keyscli.NewRepositoriesAction)
		Expect(err).NotTo(HaveOccurred())
		got := map[string]string{}
		for rows.Next() {
			var o pgtype.UUID
			var kind, actor, targetKind, target, summary string
			Expect(rows.Scan(&o, &kind, &actor, &targetKind, &target, &summary)).To(Succeed())
			Expect(kind).To(Equal(audit.KindCLI))
			Expect(actor).To(Equal("keys new-repositories"))
			Expect(targetKind).To(Equal("agent"))
			if target == b.String() {
				Expect(o).To(Equal(other))
			} else {
				Expect(o).To(Equal(org))
			}
			got[target] = summary
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(3))
		Expect(got[a2.String()]).To(And(ContainSubstring(`"seedFromEpoch": 1`), ContainSubstring(`"repositoryEpoch": 2`), ContainSubstring(checkpoint.ReasonKeyCompromised)))
		for _, s := range got {
			Expect(s).NotTo(ContainSubstring("password"))
		}

		By("running it again replaces the replacements: a seed that has not run yet moves on to the newest epoch")
		out.Reset()
		Expect(tool.NewRepositories(ctx, false, []string{a1.String()})).To(Succeed())
		Expect(out.String()).To(ContainSubstring("Started a new home repository for 1 agent(s) in 1 org(s)."))
		e := latest(ctx, org, a1)
		Expect(e.RepositoryEpoch).To(Equal(int32(2)))
		Expect(e.SeedFromEpoch).To(Equal(pgtype.Int4{Int32: 1, Valid: true}))
		Expect(latest(ctx, org, a2).RepositoryEpoch).To(Equal(int32(2)), "only the agent named")
	})

	It("makes every older retired repository of the agent due now, and leaves deleted ones and other agents alone", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		a, other := withRepository(ctx, org), withRepository(ctx, org)
		// a's epochs 0 and 1 are retired with a retention, as keys forget leaves them (forget.go:
		// the org's HomeBackupRetention); epoch 2 is current. other's epoch 0 is retired likewise.
		for range 2 {
			_, _, err := pw.NewEpoch(ctx, org, a, checkpoint.ReasonKeyLost, checkpoint.DefaultRetain)
			Expect(err).NotTo(HaveOccurred())
		}
		_, _, err := pw.NewEpoch(ctx, org, other, checkpoint.ReasonKeyLost, checkpoint.DefaultRetain)
		Expect(err).NotTo(HaveOccurred())
		// a's epoch 0 is deleted already: its row is history and stays as it is.
		var gone time.Time
		Expect(pool.QueryRow(ctx, `UPDATE home_repositories SET deleted_at = now() WHERE org_id = $1 AND agent_id = $2 RETURNING delete_after`, org, a).Scan(&gone)).To(Succeed())
		Expect(gone).To(BeTemporally(">", time.Now().Add(time.Hour)), "the fixture's retention is in the future")

		compromise(ctx)
		tool.Yes = true
		start := time.Now()
		Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(Succeed())

		deadline := func(agent pgtype.UUID, epoch int32) time.Time {
			GinkgoHelper()
			var t time.Time
			if epoch == 0 {
				Expect(pool.QueryRow(ctx, `SELECT delete_after FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, org, agent).Scan(&t)).To(Succeed())
			} else {
				Expect(pool.QueryRow(ctx, `SELECT delete_after FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = $3`,
					org, agent, epoch).Scan(&t)).To(Succeed())
			}
			return t
		}
		Expect(latest(ctx, org, a).RepositoryEpoch).To(Equal(int32(3)))
		Expect(deadline(a, 1)).To(BeTemporally("~", start, time.Minute), "retired earlier, under a retention: due now")
		Expect(deadline(a, 2)).To(BeTemporally("~", start, time.Minute), "the epoch just replaced")
		Expect(deadline(a, 0)).To(BeTemporally("==", gone), "deleted already")
		Expect(deadline(other, 0)).To(BeTemporally(">", time.Now().Add(time.Hour)), "another agent's retention is kept")
	})

	It("skips the epochs whose prefix koinon-agentd found taken, and seeds from the epoch it replaced (#262)", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		a := withRepository(ctx, org)
		_, _, err := pw.NewEpoch(ctx, org, a, checkpoint.ReasonKeyLost, checkpoint.DefaultRetain) // epoch 1
		Expect(err).NotTo(HaveOccurred())
		// After a database restore, r2/ and r3/ hold repositories started after the dump: agentd
		// raised the floor when it found them (checkpoints resync or a backup into r2/).
		_, err = pool.Exec(ctx, `UPDATE home_repositories SET next_epoch_at_least = 4 WHERE org_id = $1 AND agent_id = $2`, org, a)
		Expect(err).NotTo(HaveOccurred())

		compromise(ctx)
		tool.Yes = true
		Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(Succeed())
		e := latest(ctx, org, a)
		Expect(e.RepositoryEpoch).To(Equal(int32(4)))
		Expect(e.Prefix).To(Equal(checkpoint.EpochPrefix(org.String(), a.String(), 4)))
		Expect(e.SeedFromEpoch).To(Equal(pgtype.Int4{Int32: 1, Valid: true}), "the epoch replaced, not 3, which this database never had")
		var summary string
		Expect(pool.QueryRow(ctx, `SELECT summary::text FROM audit_events WHERE action = $1 AND target_id = $2`, keyscli.NewRepositoriesAction, a.String()).Scan(&summary)).To(Succeed())
		Expect(summary).To(And(ContainSubstring(`"repositoryEpoch": 4`), ContainSubstring(`"seedFromEpoch": 1`)))
		r, err := pw.At(ctx, org, a, 4)
		Expect(err).NotTo(HaveOccurred(), "the password is bound to epoch 4")
		Expect(r.Password).NotTo(BeEmpty())

		By("the next epoch after that follows on: the floor is below it now")
		Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(Succeed())
		Expect(latest(ctx, org, a).RepositoryEpoch).To(Equal(int32(5)))
		Expect(latest(ctx, org, a).SeedFromEpoch).To(Equal(pgtype.Int4{Int32: 4, Valid: true}))
	})

	Describe("the storage probe (#285)", func() {
		// rows counts what a refused rotation must not have written.
		type written struct{ epochs, seeds, audited, probes int }
		count := func(ctx SpecContext) written {
			GinkgoHelper()
			var w written
			Expect(pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM home_repository_epochs),
				(SELECT count(*) FROM home_repository_epochs WHERE seed_from_epoch IS NOT NULL),
				(SELECT count(*) FROM audit_events WHERE action = $1),
				(SELECT count(*) FROM home_repository_probes)`, keyscli.NewRepositoriesAction).Scan(&w.epochs, &w.seeds, &w.audited, &w.probes)).To(Succeed())
			return w
		}

		It("asks about the epoch it would start, at the recorded floor when that is higher, and starts it when the prefix is free", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			_, err := pool.Exec(ctx, `UPDATE home_repositories SET next_epoch_at_least = 3 WHERE org_id = $1 AND agent_id = $2`, org, a)
			Expect(err).NotTo(HaveOccurred())
			var mu sync.Mutex
			var probed []int32
			answerWith(func(p store.HomeRepositoryProbe) (string, string) {
				mu.Lock()
				defer mu.Unlock()
				probed = append(probed, p.RepositoryEpoch)
				return "free", ""
			})
			compromise(ctx)
			tool.Yes = true
			Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(Succeed())
			mu.Lock()
			Expect(probed).To(Equal([]int32{3}))
			mu.Unlock()
			Expect(latest(ctx, org, a).RepositoryEpoch).To(Equal(int32(3)))
			Expect(count(ctx).probes).To(BeZero(), "the command deletes the probe it read")
		})

		It("refuses a prefix that holds a repository, before anything is written", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			b := withRepository(ctx, org)
			answerWith(func(p store.HomeRepositoryProbe) (string, string) {
				if p.AgentID == a {
					return "taken", "the prefix of repository epoch 1 already holds objects"
				}
				return "free", ""
			})
			compromise(ctx)
			tool.Yes = true
			before := count(ctx)
			err := tool.NewRepositories(ctx, false, []string{a.String(), b.String()})
			Expect(err).To(MatchError(And(
				ContainSubstring("refusing to start a new repository, nothing was changed"),
				ContainSubstring("agent "+a.String()), ContainSubstring("already holds a repository the database does not record"))))
			Expect(count(ctx)).To(Equal(before), "no epoch, no seed request, no audit event, not even for the agent whose prefix is free")
			Expect(out.String()).NotTo(ContainSubstring("Started a new home repository"))
		})

		It("refuses when the check errors, and writes nothing", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			answerWith(func(store.HomeRepositoryProbe) (string, string) {
				return "error", "koinon-agentd could not list the checkpoint bucket (see its log)"
			})
			compromise(ctx)
			tool.Yes = true
			before := count(ctx)
			Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(MatchError(And(
				ContainSubstring("nothing was changed"), ContainSubstring("could not check the storage prefix of repository epoch 1"),
				ContainSubstring("could not list the checkpoint bucket"))))
			Expect(count(ctx)).To(Equal(before))
		})

		It("refuses when koinon-agentd does not answer in time, and writes nothing", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			answerWith(nil)
			compromise(ctx)
			tool.Yes = true
			tool.ProbeTimeout = time.Second
			before := count(ctx)
			started := time.Now()
			Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(MatchError(And(
				ContainSubstring("nothing was changed"), ContainSubstring("did not check the storage within 1s"))))
			Expect(time.Since(started)).To(BeNumerically("<", 4*time.Second))
			Expect(count(ctx)).To(Equal(before), "the probe row is deleted too")
		})

		It("refuses when the epoch changed between the probe and the transaction", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			answerWith(func(p store.HomeRepositoryProbe) (string, string) {
				// koinon-agentd raised the floor meanwhile: the epoch probed is not the one that would start.
				_, _ = pool.Exec(ctx, `UPDATE home_repositories SET next_epoch_at_least = 5 WHERE org_id = $1 AND agent_id = $2`, org, a)
				return "free", ""
			})
			compromise(ctx)
			tool.Yes = true
			before := count(ctx)
			Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(MatchError(ContainSubstring("changed while the storage was checked")))
			Expect(count(ctx)).To(Equal(before))
		})

		It("is not asked for an agent without a repository", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			newAgent(ctx, org)
			answerWith(nil)
			Expect(tool.NewRepositories(ctx, true, nil)).To(Succeed())
			Expect(count(ctx).probes).To(BeZero())
		})

		It("skips a target whose repository was deleted after the targets were read, and probes the rest", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			gone := withRepository(ctx, org)
			_, err := pool.Exec(ctx, `DELETE FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, org, gone)
			Expect(err).NotTo(HaveOccurred())
			var mu sync.Mutex
			var asked []pgtype.UUID
			answerWith(func(p store.HomeRepositoryProbe) (string, string) {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, p.AgentID)
				return "free", ""
			})
			got, err := tool.ProbeEpochs(ctx, [][2]pgtype.UUID{{org, gone}, {org, a}})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(map[[2]pgtype.UUID]int32{{org, a}: 1}), "nothing in the map for the deleted one")
			mu.Lock()
			Expect(asked).To(Equal([]pgtype.UUID{a}), "no probe requested for it")
			mu.Unlock()
		})
	})

	Describe("without a terminal", func() {
		It("refuses without --yes, because the older checkpoints are dropped", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(MatchError(And(ContainSubstring("older checkpoints"), ContainSubstring("--yes"))))
			Expect(epochs(ctx)).To(BeZero())
			tool.Yes = true
			Expect(tool.NewRepositories(ctx, false, []string{a.String()})).To(Succeed())
			Expect(epochs(ctx)).To(Equal(1))
		})
	})

	Describe("on a terminal", func() {
		var asked *bytes.Buffer
		ask := func(input string) {
			asked = &bytes.Buffer{}
			tool.Prompt = &cli.Prompter{In: strings.NewReader(input), Out: asked, Accessible: true}
		}

		It("wants the word typed", func(ctx SpecContext) {
			org, _, _ := pgtest.NewOrg(ctx, pool)
			a := withRepository(ctx, org)
			ask("")
			Expect(tool.NewRepositories(ctx, true, nil)).To(MatchError(cli.ErrCancelled))
			Expect(epochs(ctx)).To(BeZero())

			ask("yes\nreplace\n")
			Expect(tool.NewRepositories(ctx, true, nil)).To(Succeed())
			Expect(asked.String()).To(And(
				ContainSubstring(`Replace the home repositories of 1 agent(s)? Type "replace" to continue`),
				ContainSubstring("type replace to continue"),
				ContainSubstring("older checkpoints are deleted"),
			))
			Expect(latest(ctx, org, a).RepositoryEpoch).To(Equal(int32(1)))
		})
	})
})
