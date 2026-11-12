// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// Repository epochs (ADR 0085 decision 13): a lost or leaked key-encryption key starts a new
// repository; the old one stays readable for the checkpoints written to it until it is deleted.
var _ = Describe("Repository passwords and epochs", Ordered, func() {
	var (
		pool  *pgxpool.Pool
		ring  *secretstore.Keyring
		pw    checkpoint.Passwords
		org   pgtype.UUID
		agent pgtype.UUID
	)

	BeforeAll(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		var err error
		ring, err = secretstore.NewKeyring(map[string][]byte{"kek-20260101-test": make([]byte, secretstore.KeySize)}, "kek-20260101-test")
		Expect(err).NotTo(HaveOccurred())
		pw = checkpoint.Passwords{Q: store.New(pool), Keys: ring, Begin: pool.Begin}
	}, NodeTimeout(3*time.Minute))

	BeforeEach(func(ctx SpecContext) {
		org, _, _ = pgtest.NewOrg(ctx, pool)
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, $2, 'opencode') RETURNING id`,
			org, pgtest.Uniq("a")).Scan(&agent)).To(Succeed())
	})

	It("creates epoch 0 on first use and returns the same repository after", func(ctx SpecContext) {
		_, err := pw.At(ctx, org, agent, 0)
		Expect(err).To(MatchError(checkpoint.ErrNoRepository), "At creates nothing")
		first, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Epoch).To(BeZero())
		Expect(first.Prefix).To(Equal(checkpoint.Prefix(org.String(), agent.String())))
		Expect(first.Password).NotTo(BeEmpty())
		again, err := pw.Get(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(first))
	})

	It("starts a new epoch with a new password and prefix, and keeps the old one readable", func(ctx SpecContext) {
		e0, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())

		e1, ok, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, 72*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(e1.Epoch).To(Equal(int32(1)))
		Expect(e1.Prefix).To(Equal(checkpoint.Prefix(org.String(), agent.String()) + "r1/"))
		Expect(e1.Password).NotTo(Equal(e0.Password))

		cur, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(cur).To(Equal(e1), "the new epoch is current")
		old, err := pw.At(ctx, org, agent, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(old).To(Equal(e0), "the retired repository still opens")

		By("epoch 0 is retired with its deletion deadline")
		var retired, deleteAfter pgtype.Timestamptz
		Expect(pool.QueryRow(ctx, `SELECT retired_at, delete_after FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, org, agent).
			Scan(&retired, &deleteAfter)).To(Succeed())
		Expect(retired.Valid).To(BeTrue())
		Expect(deleteAfter.Time).To(BeTemporally("~", time.Now().Add(72*time.Hour), time.Minute))

		By("a second epoch retires the first")
		e2, ok, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyCompromised, time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(e2.Epoch).To(Equal(int32(2)))
		Expect(e2.Prefix).To(HaveSuffix("/r2/"))
		var r1 pgtype.Timestamptz
		Expect(pool.QueryRow(ctx, `SELECT retired_at FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`, org, agent).
			Scan(&r1)).To(Succeed())
		Expect(r1.Valid).To(BeTrue())
		back, err := pw.At(ctx, org, agent, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(back).To(Equal(e1))
		_, err = pw.At(ctx, org, agent, 3)
		Expect(err).To(MatchError(checkpoint.ErrNoRepository))
	})

	It("has nothing to replace for an agent without a repository, and refuses an unknown reason", func(ctx SpecContext) {
		_, ok, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		_, err = pw.At(ctx, org, agent, 0)
		Expect(err).To(MatchError(checkpoint.ErrNoRepository), "nothing was created")
		_, _, err = pw.NewEpoch(ctx, org, agent, "rotation", time.Hour)
		Expect(err).To(MatchError(ContainSubstring("unknown repository epoch reason")))
	})

	It("binds each epoch's password to its own row: a copy in another row does not open", func(ctx SpecContext) {
		_, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		_, _, err = pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, time.Hour)
		Expect(err).NotTo(HaveOccurred())
		_, _, err = pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, time.Hour)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE home_repository_epochs SET password_envelope =
			(SELECT password_envelope FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1)
			WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 2`, org, agent)
		Expect(err).NotTo(HaveOccurred())
		_, err = pw.At(ctx, org, agent, 2)
		Expect(err).To(HaveOccurred())

		By("the binding names the epoch row")
		var raw []byte
		Expect(pool.QueryRow(ctx, `SELECT password_envelope FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`, org, agent).
			Scan(&raw)).To(Succeed())
		var env secretstore.Envelope
		Expect(json.Unmarshal(raw, &env)).To(Succeed())
		_, err = ring.Open(checkpoint.BindingAt(org, agent, 1), env)
		Expect(err).NotTo(HaveOccurred())
		_, err = ring.Open(checkpoint.Binding(org, agent), env)
		Expect(err).To(HaveOccurred(), "not the epoch-0 binding")
	})

	It("says a repository whose password was lost is lost, here and over HTTP", func(ctx SpecContext) {
		_, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		e1, _, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, time.Hour)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE home_repositories SET password_envelope = NULL, credential_lost_at = now() WHERE org_id = $1 AND agent_id = $2`, org, agent)
		Expect(err).NotTo(HaveOccurred())
		_, err = pw.At(ctx, org, agent, 0)
		Expect(err).To(MatchError(checkpoint.ErrRepositoryLost))
		Expect(pw.At(ctx, org, agent, 1)).To(Equal(e1), "the later epoch is unaffected")

		key := []byte("koinon_test_not-a-real-key")
		srv := httptest.NewServer(checkpoint.Server{Passwords: pw, Key: func(context.Context) ([]byte, error) { return key, nil }})
		DeferCleanup(srv.Close)
		_, err = checkpoint.Remote{BaseURL: srv.URL, Key: func() ([]byte, error) { return key, nil }}.At(ctx, org, agent, 0)
		Expect(err).To(MatchError(checkpoint.ErrRepositoryLost))

		By("the table keeps the envelope and the mark together")
		_, err = pool.Exec(ctx, `UPDATE home_repository_epochs SET password_envelope = NULL WHERE org_id = $1 AND agent_id = $2`, org, agent)
		Expect(err).To(MatchError(ContainSubstring("home_repository_epochs_credential")))
	})

	It("answers an older koinon-agentd, which names no epoch, only while epoch 0 is current", func(ctx SpecContext) {
		key := []byte("koinon_test_not-a-real-key")
		srv := httptest.NewServer(checkpoint.Server{Passwords: pw, Key: func(context.Context) ([]byte, error) { return key, nil }})
		DeferCleanup(srv.Close)
		old := func() (int, checkpoint.Repository) {
			GinkgoHelper()
			exp := time.Now().Add(20 * time.Second).Unix()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s%s?org=%s&agent=%s&exp=%d",
				srv.URL, checkpoint.RepositoryPath, org.String(), agent.String(), exp), nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+checkpoint.RepositoryToken(key, org.String(), agent.String(), exp))
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = res.Body.Close() }()
			var out checkpoint.Repository
			if res.StatusCode == http.StatusOK {
				var raw struct {
					Epoch            int32
					Prefix, Password string
				}
				Expect(json.NewDecoder(res.Body).Decode(&raw)).To(Succeed())
				out = checkpoint.Repository(raw)
			}
			return res.StatusCode, out
		}

		code, e0 := old()
		Expect(code).To(Equal(http.StatusOK))
		Expect(e0.Epoch).To(BeZero())
		e1, _, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyCompromised, time.Hour)
		Expect(err).NotTo(HaveOccurred())
		code, _ = old()
		Expect(code).To(Equal(http.StatusConflict), "it would write to epoch 1 and record epoch 0")

		remote := checkpoint.Remote{BaseURL: srv.URL, Key: func() ([]byte, error) { return key, nil }}
		Expect(remote.Get(ctx, org, agent)).To(Equal(e1), "a current koinon-agentd names the current repository")
	})

	It("lists the longest overdue repository first, so a row that keeps failing does not hold back the rest", func(ctx SpecContext) {
		first, second := pgtest.Uniq("a"), pgtest.Uniq("b")
		ids := map[string]pgtype.UUID{}
		for _, name := range []string{first, second} {
			var id pgtype.UUID
			Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, $2, 'opencode') RETURNING id`, org, name).Scan(&id)).To(Succeed())
			ids[name] = id
		}
		// Lost, so due without a later checkpoint; the higher agent ID is the longer overdue.
		lo, hi := ids[first], ids[second]
		if lo.String() > hi.String() {
			lo, hi = hi, lo
		}
		for id, age := range map[pgtype.UUID]string{lo: "1 hour", hi: "3 days"} {
			_, err := pool.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope, credential_lost_at, retired_at, delete_after)
				VALUES ($1, $2, $3, NULL, now(), now() - interval '10 days', now() - $4::interval)`, org, id, checkpoint.Prefix(org.String(), id.String()), age)
			Expect(err).NotTo(HaveOccurred())
		}
		due, err := store.New(pool).ListDueHomeRepositories(ctx, store.ListDueHomeRepositoriesParams{OrgID: org, MaxRows: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(due).To(HaveLen(1))
		Expect(due[0].AgentID).To(Equal(hi))
	})

	It("serves the current repository and a given epoch over HTTP", func(ctx SpecContext) {
		key := []byte("koinon_test_not-a-real-key")
		srv := httptest.NewServer(checkpoint.Server{Passwords: pw, Key: func(context.Context) ([]byte, error) { return key, nil }})
		DeferCleanup(srv.Close)
		remote := checkpoint.Remote{BaseURL: srv.URL, Key: func() ([]byte, error) { return key, nil }}

		e0, err := remote.Get(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(e0.Epoch).To(BeZero())
		e1, _, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, time.Hour)
		Expect(err).NotTo(HaveOccurred())

		Expect(remote.Get(ctx, org, agent)).To(Equal(e1))
		Expect(remote.At(ctx, org, agent, 0)).To(Equal(e0))
		Expect(remote.At(ctx, org, agent, 1)).To(Equal(e1))
		_, err = remote.At(ctx, org, agent, 2)
		Expect(err).To(MatchError(checkpoint.ErrNoRepository))
	})
})
