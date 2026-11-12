// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/testing/s3test"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// failingEpochs is a bucket whose listing fails.
type failingEpochs struct {
	agentd.HomeStore
	err error
}

func (f failingEpochs) HeldEpochs(context.Context, string, string) ([]int32, error) {
	return nil, f.err
}

// slowEpochs is a bucket whose listing takes a fixed time, as a remote one does.
type slowEpochs struct {
	agentd.HomeStore
	delay time.Duration
}

func (s slowEpochs) HeldEpochs(ctx context.Context, org, agent string) ([]int32, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.HomeStore.HeldEpochs(ctx, org, agent)
}

// blockingSnapshots is a bucket whose snapshot listing (a resync's) hangs until the spec ends; it
// closes listing when the first one starts.
type blockingSnapshots struct {
	agentd.HomeStore
	listing chan struct{}
	once    *sync.Once
}

func (b blockingSnapshots) Snapshots(ctx context.Context, _, _ string) ([]checkpoint.Snapshot, error) {
	b.once.Do(func() { close(b.listing) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// Storage probes of keys new-repositories and keys forget (#285, ADR 0032): koinon-agentd, which
// holds the bucket's credentials, checks the prefix of the epoch a rotation would start and answers
// in home_repository_probes; the keys command, which holds the KEKs, waits for the answer.
var _ = Describe("storage probes", func() {
	var (
		e      *env
		agent  pgtype.UUID
		store0 agentd.HomeStore
	)

	cfg := func(s agentd.HomeStore) func(*agentd.Config) {
		return func(c *agentd.Config) {
			c.Metrics = agentd.NewMetrics(nil)
			c.Checkpoints = &agentd.Checkpoints{
				Store: s, Repositories: fixedRepo{}, Interval: 24 * time.Hour, Poll: 50 * time.Millisecond, Sweep: time.Hour,
				Attempts: 2, Retry: 10 * time.Millisecond, Timeout: racescale.D(20 * time.Second), Lease: 20 * time.Minute,
			}
		}
	}
	request := func(ctx context.Context, epoch int32, timeoutSeconds int32) store.HomeRepositoryProbe {
		GinkgoHelper()
		p, err := q.RequestHomeRepositoryProbe(ctx, store.RequestHomeRepositoryProbeParams{
			OrgID: e.org, AgentID: agent, RepositoryEpoch: epoch, TimeoutSeconds: timeoutSeconds,
		})
		Expect(err).NotTo(HaveOccurred())
		return p
	}
	answer := func(ctx context.Context, p store.HomeRepositoryProbe) func() store.HomeRepositoryProbe {
		return func() store.HomeRepositoryProbe {
			got, err := q.ListHomeRepositoryProbes(ctx, store.ListHomeRepositoryProbesParams{OrgID: e.org, Ids: []pgtype.UUID{p.ID}})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(1))
			return got[0]
		}
	}
	answered := func(ctx context.Context, p store.HomeRepositoryProbe) func() bool {
		return func() bool { return answer(ctx, p)().AnsweredAt.Valid }
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, harness.OpenCode)
		store0 = &checkpoint.Writer{Storage: checkpoint.Dir(GinkgoT().TempDir()), Scratch: GinkgoT().TempDir()}
		Expect(db.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'probed', 'opencode') RETURNING id`, e.org).Scan(&agent)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
			e.org, agent, checkpoint.Prefix(e.org.String(), agent.String()))
		Expect(err).NotTo(HaveOccurred())
	})

	It("answers free for a prefix that holds nothing and taken for one that holds objects", func(ctx SpecContext) {
		w := store0.(*checkpoint.Writer)
		_, err := w.Backup(ctx, checkpoint.EpochPrefix(e.org.String(), agent.String(), 2), "koinon_test_not-a-real-token", tarOf(map[string]string{"a": "b"}), checkpoint.Meta{CheckpointID: "c"})
		Expect(err).NotTo(HaveOccurred())
		free, taken := request(ctx, 1, 60), request(ctx, 2, 60)
		stop := e.pool("agentd-a", cfg(store0))
		defer stop()

		Eventually(answered(ctx, free)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Eventually(answered(ctx, taken)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(answer(ctx, free)().Outcome.String).To(Equal(agentd.ProbeFree))
		got := answer(ctx, taken)()
		Expect(got.Outcome.String).To(Equal(agentd.ProbeTaken))
		Expect(got.Message.String).To(ContainSubstring("repository epoch 2"))

		By("the taken answer records the next free epoch, as a resync does, so the command run again probes 3")
		var floor pgtype.Int4
		var marked pgtype.Timestamptz
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least, prefix_taken_at FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, agent).
			Scan(&floor, &marked)).To(Succeed())
		Expect(floor).To(Equal(pgtype.Int4{Int32: 3, Valid: true}))
		Expect(marked.Valid).To(BeFalse(), "the agent's own repository is not marked taken")
		Expect(got.Message.String).To(ContainSubstring("recorded 3 as the next free epoch"))
		var audited int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2`, e.org, agentd.PrefixTakenAction).Scan(&audited)).To(Succeed())
		Expect(audited).To(Equal(1))
	})

	It("answers probes while a resync is slow in restore-hold mode", func(ctx SpecContext) {
		// The resync of another agent blocks in its listing; the probe is answered beside it.
		var other pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'resynced', 'opencode') RETURNING id`, e.org).Scan(&other)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
			e.org, other, checkpoint.Prefix(e.org.String(), other.String()))
		Expect(err).NotTo(HaveOccurred())
		_, err = q.RequestHomeCheckpointResync(ctx, store.RequestHomeCheckpointResyncParams{OrgID: e.org, AgentID: other})
		Expect(err).NotTo(HaveOccurred())
		listing := make(chan struct{})
		stop := e.heldPool("agentd-held", cfg(blockingSnapshots{HomeStore: store0, listing: listing, once: &sync.Once{}}))
		defer stop()
		Eventually(listing).WithTimeout(racescale.D(20*time.Second)).Should(BeClosed(), "the resync is in its listing")
		p := request(ctx, 1, 60)
		Eventually(answered(ctx, p)).WithTimeout(racescale.D(5 * time.Second)).Should(BeTrue())
		Expect(answer(ctx, p)().Outcome.String).To(Equal(agentd.ProbeFree))
	})

	It("answers error with a message that carries no endpoint detail when the listing fails", func(ctx SpecContext) {
		p := request(ctx, 1, 60)
		stop := e.pool("agentd-a", cfg(failingEpochs{HomeStore: store0, err: errors.New("dial tcp s3.internal.example:9000: koinon_test_secret-detail")}))
		defer stop()
		Eventually(answered(ctx, p)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		got := answer(ctx, p)()
		Expect(got.Outcome.String).To(Equal(agentd.ProbeError))
		Expect(got.Message.String).NotTo(ContainSubstring("s3.internal.example"))
		Expect(got.Message.String).NotTo(ContainSubstring("secret-detail"))
	})

	It("does not answer a probe that has expired: the keys command gave up", func(ctx SpecContext) {
		p := request(ctx, 1, 60)
		_, err := db.Exec(ctx, `UPDATE home_repository_probes SET expires_at = now() - interval '1 second' WHERE org_id = $1 AND id = $2`, e.org, p.ID)
		Expect(err).NotTo(HaveOccurred())
		fresh := request(ctx, 2, 60)
		stop := e.pool("agentd-a", cfg(store0))
		defer stop()
		Eventually(answered(ctx, fresh)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(answer(ctx, p)().AnsweredAt.Valid).To(BeFalse())
	})

	It("polls only the orgs with probe work: a due probe or an old row, in a live org", func(ctx SpecContext) {
		keep := int32(time.Hour.Seconds())
		listed := func() []pgtype.UUID {
			orgs, err := q.ListOrgIDsWithProbeWork(ctx, keep)
			Expect(err).NotTo(HaveOccurred())
			return orgs
		}
		Expect(listed()).NotTo(ContainElement(e.org), "no probe")

		p := request(ctx, 1, 60)
		Expect(listed()).To(ContainElement(e.org), "a due probe")
		_, err := db.Exec(ctx, `UPDATE home_repository_probes SET probe_after = now() + interval '1 minute' WHERE org_id = $1 AND id = $2`, e.org, p.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed()).NotTo(ContainElement(e.org), "a claimed probe under its lease")
		_, err = db.Exec(ctx, `UPDATE home_repository_probes SET probe_after = now(), answered_at = now(), outcome = 'free' WHERE org_id = $1 AND id = $2`, e.org, p.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed()).NotTo(ContainElement(e.org), "an answered probe the command has still to read")
		_, err = db.Exec(ctx, `UPDATE home_repository_probes SET expires_at = now() - interval '30 minutes' WHERE org_id = $1 AND id = $2`, e.org, p.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed()).NotTo(ContainElement(e.org), "expired, within the grace")
		_, err = db.Exec(ctx, `UPDATE home_repository_probes SET expires_at = now() - interval '2 hours' WHERE org_id = $1 AND id = $2`, e.org, p.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed()).To(ContainElement(e.org), "a row to delete")

		_, err = db.Exec(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, e.org)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed()).NotTo(ContainElement(e.org), "a deleted org")
	})

	It("deletes answered and expired rows a while after they expire, and keeps the recent ones", func(ctx SpecContext) {
		old, recent := request(ctx, 1, 60), request(ctx, 2, 60)
		_, err := db.Exec(ctx, `UPDATE home_repository_probes SET expires_at = now() - interval '2 hours', answered_at = now() - interval '2 hours',
			outcome = 'free' WHERE org_id = $1 AND id = $2`, e.org, old.ID)
		Expect(err).NotTo(HaveOccurred())
		stale := request(ctx, 3, 60) // never answered, expired long ago
		_, err = db.Exec(ctx, `UPDATE home_repository_probes SET expires_at = now() - interval '2 hours' WHERE org_id = $1 AND id = $2`, e.org, stale.ID)
		Expect(err).NotTo(HaveOccurred())
		stop := e.pool("agentd-a", cfg(store0))
		defer stop()
		count := func() int {
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM home_repository_probes WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
			return n
		}
		Eventually(count).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(1))
		Eventually(answered(ctx, recent)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
	})

	It("answers many probes concurrently: a rotation of a large install finishes within the wait", func(ctx SpecContext) {
		// 48 agents at 250ms a listing would take 12s one by one; the workers answer them in a fraction of that.
		const agents = 48
		var probes []store.HomeRepositoryProbe
		for i := 0; i < agents; i++ {
			var a pgtype.UUID
			Expect(db.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'bulk', 'opencode') RETURNING id`, e.org).Scan(&a)).To(Succeed())
			_, err := db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
				e.org, a, checkpoint.Prefix(e.org.String(), a.String()))
			Expect(err).NotTo(HaveOccurred())
			p, err := q.RequestHomeRepositoryProbe(ctx, store.RequestHomeRepositoryProbeParams{OrgID: e.org, AgentID: a, RepositoryEpoch: 1, TimeoutSeconds: 120})
			Expect(err).NotTo(HaveOccurred())
			probes = append(probes, p)
		}
		stop := e.pool("agentd-a", cfg(slowEpochs{HomeStore: store0, delay: 250 * time.Millisecond}))
		defer stop()
		start := time.Now()
		for _, p := range probes {
			Eventually(answered(ctx, p)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		}
		Expect(time.Since(start)).To(BeNumerically("<", racescale.D(6*time.Second)))
	})

	It("answers in restore-hold mode too, where the restore runbook runs keys forget", func(ctx SpecContext) {
		p := request(ctx, 1, 60)
		stop := e.heldPool("agentd-held", cfg(store0))
		defer stop()
		Eventually(answered(ctx, p)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(answer(ctx, p)().Outcome.String).To(Equal(agentd.ProbeFree))
	})

	// The whole path against a real S3 server: the keys command and koinon-agentd share only the
	// database. The command never gets the bucket's credentials; agentd never gets a KEK.
	Describe("against a real bucket", Ordered, func() {
		var (
			s3     checkpoint.S3
			real   *checkpoint.Writer
			tool   *keyscli.Tool
			out    *bytes.Buffer
			passwd checkpoint.Passwords
			kube   *fake.Clientset
		)
		BeforeAll(func(ctx SpecContext) {
			testenv.Require(testenv.Docker)
			var err error
			s3, err = s3test.Start(ctx, func(f func(context.Context) error) { DeferCleanup(f) })
			Expect(err).NotTo(HaveOccurred())
			real = &checkpoint.Writer{Storage: s3, Scratch: GinkgoT().TempDir()}
		}, NodeTimeout(4*time.Minute))

		BeforeEach(func(ctx SpecContext) {
			const ns = "koinon"
			key := make([]byte, secretstore.KeySize)
			_, _ = rand.Read(key)
			kube = fake.NewClientset()
			_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
				Data:       map[string][]byte{"kek-20260101-aaaa": secretstore.EncodeKey(key), "active": []byte("kek-20260101-aaaa")},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ks := keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek"}
			set, err := ks.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			ring, err := secretstore.NewKeyring(set.Keys, set.Active)
			Expect(err).NotTo(HaveOccurred())
			passwd = checkpoint.Passwords{Q: q, Keys: ring, Begin: db.Begin}
			// The agent's repository row is the real one: sealed with the KEK the command holds.
			_, err = db.Exec(ctx, `DELETE FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, agent)
			Expect(err).NotTo(HaveOccurred())
			_, err = passwd.Current(ctx, e.org, agent)
			Expect(err).NotTo(HaveOccurred())
			out = &bytes.Buffer{}
			tool = &keyscli.Tool{Pool: db, Store: ks, Out: out, Yes: true, ProbeTimeout: 30 * time.Second, ProbePoll: 50 * time.Millisecond}
		})

		epochs := func(ctx context.Context) int {
			GinkgoHelper()
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&n)).To(Succeed())
			return n
		}

		It("refuses a rotation onto a prefix that holds a repository, before anything is written, and rotates once it is free", func(ctx SpecContext) {
			By("a repository at r1/ that this database has no row for, as after a restore from an older dump")
			_, err := real.Backup(ctx, checkpoint.EpochPrefix(e.org.String(), agent.String(), 1), "koinon_test_unknown-to-the-database", tarOf(map[string]string{"a": "b"}), checkpoint.Meta{CheckpointID: "c"})
			Expect(err).NotTo(HaveOccurred())
			stop := e.pool("agentd-a", cfg(real))
			defer stop()

			err = tool.NewRepositories(ctx, false, []string{agent.String()})
			Expect(err).To(MatchError(And(ContainSubstring("nothing was changed"), ContainSubstring("repository epoch 1 already holds a repository"))))
			Expect(epochs(ctx)).To(BeZero(), "no epoch, no seed request")
			var audited int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2`, e.org, keyscli.NewRepositoriesAction).Scan(&audited)).To(Succeed())
			Expect(audited).To(BeZero())

			Expect(err).To(MatchError(ContainSubstring("koinon-agentd recorded the next free epoch")))

			By("the second try: agentd recorded the floor with its answer, so the probe asks about 2, which is free")
			Expect(tool.NewRepositories(ctx, false, []string{agent.String()})).To(Succeed())
			Expect(epochs(ctx)).To(Equal(1))
			var epoch int32
			Expect(db.QueryRow(ctx, `SELECT max(repository_epoch) FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&epoch)).To(Succeed())
			Expect(epoch).To(Equal(int32(2)))
		}, SpecTimeout(3*time.Minute))

		It("keys forget: refuses an epoch-0 agent's rotation onto a taken r1/ with taken, marks nothing lost, and rotates on the second try", func(ctx SpecContext) {
			By("a repository at r1/ that this database has no row for, and the KEK of epoch 0 lost")
			_, err := real.Backup(ctx, checkpoint.EpochPrefix(e.org.String(), agent.String(), 1), "koinon_test_unknown-to-the-database", tarOf(map[string]string{"a": "b"}), checkpoint.Meta{CheckpointID: "c"})
			Expect(err).NotTo(HaveOccurred())
			const lost, next = "kek-20260101-aaaa", "kek-20260102-bbbb"
			key := make([]byte, secretstore.KeySize)
			_, _ = rand.Read(key)
			sec, err := kube.CoreV1().Secrets("koinon").Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(sec.Data, lost)
			sec.Data[next] = secretstore.EncodeKey(key)
			sec.Data["active"] = []byte(next)
			_, err = kube.CoreV1().Secrets("koinon").Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// Other specs of this process leave placeholder envelopes ('{}') that name no KEK, and keys
			// forget counts every envelope of the install (Tool.Usage): name a KEK this spec does not use.
			for _, c := range keyscli.Columns {
				_, err = db.Exec(ctx, fmt.Sprintf(`UPDATE %[1]s SET %[2]s = jsonb_set(%[2]s, '{kek_id}', '"kek-20250101-0000"') WHERE %[2]s IS NOT NULL AND %[2]s->>'kek_id' IS NULL`, c.Table, c.Column))
				Expect(err).NotTo(HaveOccurred(), c.Table)
			}
			stop := e.pool("agentd-a", cfg(real))
			defer stop()
			repoLost := func() bool {
				GinkgoHelper()
				var l bool
				Expect(db.QueryRow(ctx, `SELECT credential_lost_at IS NOT NULL FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&l)).To(Succeed())
				return l
			}

			By("the first try: agentd answers taken (the org's transaction holds no lock agentd needs) and records the floor")
			err = tool.Forget(ctx, lost)
			Expect(err).To(MatchError(And(ContainSubstring("nothing was changed"), ContainSubstring("repository epoch 1 already holds a repository"))))
			Expect(err).NotTo(MatchError(ContainSubstring("did not check the storage")))
			Expect(repoLost()).To(BeFalse(), "nothing is marked lost")
			Expect(epochs(ctx)).To(BeZero())

			By("the second try probes epoch 2, which is free, and rotates there")
			Expect(tool.Forget(ctx, lost)).To(Succeed())
			Expect(repoLost()).To(BeTrue())
			var epoch int32
			Expect(db.QueryRow(ctx, `SELECT max(repository_epoch) FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&epoch)).To(Succeed())
			Expect(epoch).To(Equal(int32(2)))
		}, SpecTimeout(3*time.Minute))

		It("refuses when the bucket cannot be listed, and when no agentd runs", func(ctx SpecContext) {
			stop := e.pool("agentd-a", cfg(failingEpochs{HomeStore: real, err: errors.New("the bucket is down")}))
			Expect(tool.NewRepositories(ctx, false, []string{agent.String()})).To(MatchError(ContainSubstring("could not check the storage prefix")))
			Expect(epochs(ctx)).To(BeZero())
			stop()

			tool.ProbeTimeout = 2 * time.Second
			Expect(tool.NewRepositories(ctx, false, []string{agent.String()})).To(MatchError(ContainSubstring("did not check the storage within 2s")))
			Expect(epochs(ctx)).To(BeZero())
		}, SpecTimeout(3*time.Minute))
	})
})
