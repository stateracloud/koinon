// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tracedest"
)

// keys forget (issue #216, ADR 0085 decision 13): a KEK of which no copy exists any more.
var _ = Describe("koinon-api keys forget", func() {
	const (
		lostKEK = "kek-20260101-aaaa"
		nextKEK = "kek-20261023-bbbb"
	)
	var (
		kube *fake.Clientset
		ks   keyscli.KubeStore
		tool *keyscli.Tool
		out  *bytes.Buffer
		now  time.Time
		// lostKey is the lost KEK's bytes, for a spec that restores it from "escrow".
		lostKey []byte
	)

	ring := func(ctx SpecContext) *secretstore.Keyring {
		GinkgoHelper()
		set, err := ks.Load(ctx)
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
	// rotateAndLose activates a second KEK and then loses the first: the Secret no longer holds it.
	rotateAndLose := func(ctx SpecContext) {
		GinkgoHelper()
		Expect(tool.Add(ctx, nextKEK)).To(Succeed())
		Expect(tool.Activate(ctx, nextKEK)).To(Succeed())
		now = now.Add(6 * time.Minute)
		sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		delete(sec.Data, lostKEK)
		_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	// saveWork stores a saved work of agent in org, sealed with r, that expires in 14 days.
	saveWork := func(ctx SpecContext, r *secretstore.Keyring, org, user, agent pgtype.UUID) pgtype.UUID {
		GinkgoHelper()
		var version, task, run, work pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agent_versions (org_id, agent_id, version, config) VALUES ($1, $2, 1, '{}') RETURNING id`, org, agent).Scan(&version)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_user_id) VALUES ($1, 't', $2, 'user', $3) RETURNING id`, org, agent, user).Scan(&task)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, ready_at, max_tokens) VALUES ($1, $2, $3, $4, now(), 1000) RETURNING id`, org, agent, version, task).Scan(&run)).To(Succeed())
		Expect(pool.QueryRow(ctx, `SELECT uuidv7()`).Scan(&work)).To(Succeed())
		sealedWork, err := savedwork.SealPacked(r, org.String(), work.String(), []byte("packed"))
		Expect(err).NotTo(HaveOccurred())
		rawWork, err := json.Marshal(sealedWork)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO saved_work (org_id, id, task_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, envelope, expires_at)
			VALUES ($1, $2, $3, $4, $5, 'acme/widgets', $6, 'koinon/wip/abc-1', '{}', 6, $7, now() + interval '14 days')`,
			org, work, task, run, agent, strings.Repeat("a", 40), rawWork)
		Expect(err).NotTo(HaveOccurred())
		return work
	}
	// sealPlatform stores the platform's trace destination, its credentials sealed with the active KEK.
	sealPlatform := func(ctx SpecContext) {
		GinkgoHelper()
		e, err := ring(ctx).Seal(tracedest.PlatformBinding(), []byte(`{"api_key":"koinon_test_not-a-real-token"}`))
		Expect(err).NotTo(HaveOccurred())
		raw, err := json.Marshal(e)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url, credentials) VALUES ('datadog', 'https://otlp.example.com', $1)
			ON CONFLICT (singleton) DO UPDATE SET credentials = EXCLUDED.credentials, credential_lost_at = NULL`, raw)
		Expect(err).NotTo(HaveOccurred())
	}
	// holdKeyLock takes the key events' advisory lock in a transaction of its own; the caller rolls it back.
	holdKeyLock := func(ctx SpecContext) pgx.Tx {
		GinkgoHelper()
		lock, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = lock.Rollback(ctx) })
		_, err = lock.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('koinon.keys_observed'))`)
		Expect(err).NotTo(HaveOccurred())
		return lock
	}
	// orgRows are the values of one org sealed with the lost KEK: a provider key and a saved work.
	type orgRows struct {
		org, work pgtype.UUID
		provider  string
	}
	sealOrg := func(ctx SpecContext) orgRows {
		GinkgoHelper()
		org, user, _ := pgtest.NewOrg(ctx, pool)
		r := ring(ctx)
		p, err := catalog.New(pool, r, nil).CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
			Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-a",
		})
		Expect(err).NotTo(HaveOccurred())
		var agent pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, flavor) VALUES ($1, $2, 'opencode') RETURNING id`, org, pgtest.Uniq("a")).Scan(&agent)).To(Succeed())
		return orgRows{org: org, provider: p.ID, work: saveWork(ctx, r, org, user, agent)}
	}
	// orgState is whether the org's provider key is lost and its saved work's status.
	orgState := func(ctx SpecContext, o orgRows) (bool, string) {
		GinkgoHelper()
		var lost bool
		var status string
		Expect(pool.QueryRow(ctx, `SELECT credential_lost_at IS NOT NULL FROM providers WHERE id = $1`, o.provider).Scan(&lost)).To(Succeed())
		Expect(pool.QueryRow(ctx, `SELECT status FROM saved_work WHERE org_id = $1 AND id = $2`, o.org, o.work).Scan(&status)).To(Succeed())
		return lost, status
	}
	platformLost := func(ctx SpecContext) bool {
		GinkgoHelper()
		var lost bool
		Expect(pool.QueryRow(ctx, `SELECT credential_lost_at IS NOT NULL FROM platform_trace_destination`).Scan(&lost)).To(Succeed())
		return lost
	}

	BeforeEach(func(ctx SpecContext) {
		pgtest.TruncateOrgs(ctx, pool)
		_, err := pool.Exec(ctx, `DELETE FROM kek_fingerprints`)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _, _ = pool.Exec(ctx, `DELETE FROM platform_trace_destination`) })
		now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		kube = fake.NewClientset()
		lostKey = make([]byte, secretstore.KeySize)
		_, _ = rand.Read(lostKey)
		_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
			Data:       map[string][]byte{lostKEK: secretstore.EncodeKey(lostKey), "active": []byte(lostKEK)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ks = keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek"}
		out = &bytes.Buffer{}
		tool = &keyscli.Tool{Pool: pool, Store: ks, Out: out, Now: func() time.Time { return now }, ProbeTimeout: 5 * time.Second, ProbePoll: 10 * time.Millisecond}
		freeStorage()
	})

	It("refuses a KEK still in the key set and an invalid ID, and has nothing to forget for an unused KEK", func(ctx SpecContext) {
		org, user, _ := pgtest.NewOrg(ctx, pool)
		_, err := catalog.New(pool, ring(ctx), nil).CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
			Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-a",
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(tool.Forget(ctx, lostKEK)).To(MatchError(And(ContainSubstring("is in the key set"), ContainSubstring("keys rewrap"))))
		Expect(tool.Forget(ctx, "active")).To(MatchError(ContainSubstring("not a valid KEK ID")))
		Expect(tool.Forget(ctx, "")).To(MatchError("a KEK ID is required"), "without a prompter nothing is picked")
		Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 1}), "nothing changed")

		Expect(tool.Forget(ctx, "kek-20250101-ffff")).To(Succeed())
		Expect(out.String()).To(ContainSubstring("No stored value is sealed with KEK kek-20250101-ffff; nothing to forget."))
	})

	It("marks every value the lost KEK sealed lost, starts new home repositories, and audits each org", func(ctx SpecContext) {
		org, user, _ := pgtest.NewOrg(ctx, pool)
		other, otherUser, _ := pgtest.NewOrg(ctx, pool)
		settings := orgsettings.NewService(pool)
		three := 3
		Expect(settings.SetHomeBackupRetention(ctx, org, audit.User(user), &three)).To(Succeed())
		r := ring(ctx)
		q := store.New(pool)

		By("sealing a value of every kind under the KEK that will be lost")
		provider, err := catalog.New(pool, r, nil).CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
			Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-a",
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = catalog.New(pool, r, nil).CreateProvider(ctx, other, audit.User(otherUser), otherUser, catalog.ProviderInput{
			Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-b",
		})
		Expect(err).NotTo(HaveOccurred())
		mcp, err := mcpservers.New(pool, r, nil).WithURLPolicy(catalog.URLPolicy{Namespace: ns}).Create(ctx, org, audit.User(user), user, mcpservers.Input{
			Name: pgtest.Uniq("m"), URL: "https://mcp.example.com/mcp", CredentialPrefix: "Bearer ", CredentialValue: "fake-mcp-token",
		})
		Expect(err).NotTo(HaveOccurred())
		connID := pgtype.UUID{Bytes: [16]byte{1, 2, 3}, Valid: true}
		env, err := connections.SealFields(r, org, connID, map[string][]byte{connections.GitHubPrivateKeyField: []byte("pem")})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: connID, Connector: "github", DisplayName: "c", Config: []byte(`{}`), SecretEnvelope: env, Source: store.ConfigSourceUi,
		})
		Expect(err).NotTo(HaveOccurred())
		seal := func(b secretstore.Binding) []byte {
			GinkgoHelper()
			e, err := r.Seal(b, []byte(`{"api_key":"koinon_test_not-a-real-token"}`))
			Expect(err).NotTo(HaveOccurred())
			raw, err := json.Marshal(e)
			Expect(err).NotTo(HaveOccurred())
			return raw
		}
		var dest pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO trace_destinations (org_id, name, backend, endpoint_url) VALUES ($1, 'dd', 'datadog', 'https://otlp.example.com') RETURNING id`, org).Scan(&dest)).To(Succeed())
		_, err = pool.Exec(ctx, `UPDATE trace_destinations SET credentials = $3 WHERE org_id = $1 AND id = $2`, org, dest, seal(tracedest.Binding(org, dest)))
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url, credentials) VALUES ('datadog', 'https://otlp.example.com', $1)
			ON CONFLICT (singleton) DO UPDATE SET credentials = EXCLUDED.credentials`, seal(tracedest.PlatformBinding()))
		Expect(err).NotTo(HaveOccurred())

		newAgent := func() pgtype.UUID {
			GinkgoHelper()
			var a pgtype.UUID
			Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, flavor) VALUES ($1, $2, 'opencode') RETURNING id`, org, pgtest.Uniq("a")).Scan(&a)).To(Succeed())
			return a
		}
		agent1, agent2, agent3 := newAgent(), newAgent(), newAgent()
		work := saveWork(ctx, r, org, user, agent1)

		pw := checkpoint.Passwords{Q: q, Keys: r, Begin: pool.Begin}
		_, err = pw.Current(ctx, org, agent1) // epoch 0 only
		Expect(err).NotTo(HaveOccurred())
		_, err = pw.Current(ctx, org, agent2)
		Expect(err).NotTo(HaveOccurred())
		_, _, err = pw.NewEpoch(ctx, org, agent2, checkpoint.ReasonKeyLost, checkpoint.DefaultRetain) // epochs 0 and 1
		Expect(err).NotTo(HaveOccurred())
		_, err = pw.Current(ctx, org, agent3) // epoch 0 now, epoch 1 under the next KEK below
		Expect(err).NotTo(HaveOccurred())
		Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 11}))

		rotateAndLose(ctx)
		pw.Keys = ring(ctx)
		e3, _, err := pw.NewEpoch(ctx, org, agent3, checkpoint.ReasonKeyLost, checkpoint.DefaultRetain)
		Expect(err).NotTo(HaveOccurred())
		Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 11, nextKEK: 1}))
		// agent2's r2/ holds a repository this database has no password for (started after a dump
		// it was restored from): koinon-agentd found it and raised the floor (#262).
		_, err = pool.Exec(ctx, `UPDATE home_repositories SET next_epoch_at_least = 3 WHERE org_id = $1 AND agent_id = $2`, org, agent2)
		Expect(err).NotTo(HaveOccurred())

		tool.Yes = true // no terminal: --yes is what lets it run
		Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
		Expect(out.String()).To(And(
			ContainSubstring("Forgot KEK "+lostKEK+": 11 value(s) marked lost in 2 org(s)."),
			ContainSubstring("2 agent(s) start a new home repository"),
			ContainSubstring("platform's trace destination lost its credentials"),
		))
		Expect(usage(ctx)).To(Equal(map[string]int{nextKEK: 3}), "only the new repositories' and agent3's passwords remain")

		By("each credential is lost and asks to be entered again; saved work expired")
		lost := func(table, where string, args ...any) bool {
			GinkgoHelper()
			var ok bool
			Expect(pool.QueryRow(ctx, `SELECT credential_lost_at IS NOT NULL FROM `+table+` WHERE `+where, args...).Scan(&ok)).To(Succeed())
			return ok
		}
		Expect(lost("providers", "id = $1", provider.ID)).To(BeTrue())
		Expect(lost("connections", "org_id = $1 AND id = $2", org, connID)).To(BeTrue())
		Expect(lost("trace_destinations", "org_id = $1 AND id = $2", org, dest)).To(BeTrue())
		Expect(lost("platform_trace_destination", "true")).To(BeTrue())
		Expect(lost("mcp_servers", "org_id = $1 AND name = $2", org, mcp.Name)).To(BeTrue())
		var prefix string
		var header *string
		Expect(pool.QueryRow(ctx, `SELECT credential_prefix, credential_header FROM mcp_servers WHERE org_id = $1 AND name = $2`, org, mcp.Name).Scan(&prefix, &header)).To(Succeed())
		Expect(prefix).To(Equal("Bearer "), "the form asks for the value only")
		Expect(header).NotTo(BeNil())
		var status string
		var envNull bool
		Expect(pool.QueryRow(ctx, `SELECT status, envelope IS NULL FROM saved_work WHERE org_id = $1 AND id = $2`, org, work).Scan(&status, &envNull)).To(Succeed())
		Expect(status).To(Equal("expired"))
		Expect(envNull).To(BeTrue())

		By("agents whose current repository was lost start a new one, sealed with the active KEK")
		latest := func(agent pgtype.UUID) store.HomeRepositoryEpoch {
			GinkgoHelper()
			e, err := q.LatestHomeRepositoryEpoch(ctx, store.LatestHomeRepositoryEpochParams{OrgID: org, AgentID: agent})
			Expect(err).NotTo(HaveOccurred())
			return e
		}
		Expect(latest(agent1).RepositoryEpoch).To(Equal(int32(1)))
		Expect(latest(agent2).RepositoryEpoch).To(Equal(int32(3)), "r2/ is taken: skipped")
		Expect(latest(agent3).RepositoryEpoch).To(Equal(int32(1)), "its current repository still opens")
		for _, a := range []pgtype.UUID{agent1, agent2} {
			cur, err := pw.Current(ctx, org, a)
			Expect(err).NotTo(HaveOccurred())
			Expect(cur.Epoch).To(Equal(latest(a).RepositoryEpoch))
		}
		Expect(pw.Current(ctx, org, agent3)).To(Equal(e3))

		By("the lost repositories are deleted after the org's retention, not the default")
		var h store.HomeRepository
		h, err = q.GetHomeRepository(ctx, store.GetHomeRepositoryParams{OrgID: org, AgentID: agent1})
		Expect(err).NotTo(HaveOccurred())
		Expect(h.CredentialLostAt.Valid).To(BeTrue())
		Expect(h.DeleteAfter.Time).To(BeTemporally("~", time.Now().Add(72*time.Hour), time.Minute))
		var after time.Time
		Expect(pool.QueryRow(ctx, `SELECT delete_after FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`, org, agent2).Scan(&after)).To(Succeed())
		Expect(after).To(BeTemporally("~", time.Now().Add(72*time.Hour), time.Minute))

		By("one audit event per org, by the cli actor")
		type event struct {
			org             pgtype.UUID
			kind, id, actor string
			summary         []byte
		}
		rows, err := pool.Query(ctx, `SELECT org_id, actor_kind, actor_id, target_id, summary FROM audit_events WHERE action = $1 AND org_id IN ($2, $3) ORDER BY org_id`, keyscli.ForgetAction, org, other)
		Expect(err).NotTo(HaveOccurred())
		var events []event
		for rows.Next() {
			var e event
			Expect(rows.Scan(&e.org, &e.kind, &e.actor, &e.id, &e.summary)).To(Succeed())
			events = append(events, e)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(events).To(HaveLen(2))
		for _, e := range events {
			Expect(e.kind).To(Equal(audit.KindCLI))
			Expect(e.actor).To(Equal("keys forget"))
			Expect(e.id).To(Equal(lostKEK))
			Expect(string(e.summary)).NotTo(ContainSubstring("not-a-real-token"))
			if e.org == org {
				Expect(string(e.summary)).To(And(ContainSubstring(`"retentionDays": 3`), ContainSubstring("newRepositoryEpochs"),
					ContainSubstring(checkpoint.EpochRow(agent2.String(), 3))))
			}
		}

		By("the platform's trace destination is one install event, and in no org")
		var platformSummary []byte
		var kind, actorID string
		Expect(pool.QueryRow(ctx, `SELECT actor_kind, actor_id, summary FROM install_audit_events WHERE action = $1 AND target_id = $2 ORDER BY chain_seq DESC LIMIT 1`,
			keyscli.ForgetAction, lostKEK).Scan(&kind, &actorID, &platformSummary)).To(Succeed())
		Expect(kind).To(Equal(audit.KindCLI))
		Expect(actorID).To(Equal("keys forget"))
		Expect(string(platformSummary)).To(ContainSubstring("platformTraceDestination"))

		By("running it again finds nothing left")
		out.Reset()
		Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("nothing to forget"))
	})

	Describe("the platform part, in the key audit pattern", func() {
		BeforeEach(func(ctx SpecContext) {
			sealPlatform(ctx)
			tool.Yes = true
		})

		It("writes keys.observed first when the key set changed without a recorded event", func(ctx SpecContext) {
			rotateAndLose(ctx) // add and activate are recorded; the lost KEK's removal from the Secret is not
			mark := auditMark(ctx)
			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())

			rows, err := pool.Query(ctx, `SELECT action, actor_id FROM install_audit_events WHERE chain_seq > $1 ORDER BY chain_seq`, mark)
			Expect(err).NotTo(HaveOccurred())
			defer rows.Close()
			var got []string
			for rows.Next() {
				var action, actor string
				Expect(rows.Scan(&action, &actor)).To(Succeed())
				Expect(actor).To(Equal("keys forget"), "the command that found the gap writes both")
				got = append(got, action)
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(got).To(Equal([]string{keyscli.ActionObserved, keyscli.ForgetAction}))
		})

		It("holds the key events' advisory lock from before the first change, the orgs' included", func(ctx SpecContext) {
			o := sealOrg(ctx)
			rotateAndLose(ctx)
			_, workBefore := orgState(ctx, o)
			lock := holdKeyLock(ctx)

			done := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				done <- tool.Forget(ctx, lostKEK)
			}()
			Eventually(func() bool { return advisoryWaiting(ctx) }).Should(BeTrue(), "forget waits for the lock")
			Consistently(done, 300*time.Millisecond).ShouldNot(Receive())
			Expect(platformLost(ctx)).To(BeFalse(), "nothing is changed before the lock is held")
			lost, work := orgState(ctx, o)
			Expect(lost).To(BeFalse(), "the org part waits for the lock too")
			Expect(work).To(Equal(workBefore))

			Expect(lock.Rollback(ctx)).To(Succeed())
			Eventually(done).Should(Receive(BeNil()))
			Expect(platformLost(ctx)).To(BeTrue())
			lost, work = orgState(ctx, o)
			Expect(lost).To(BeTrue())
			Expect(work).To(Equal("expired"))
		})

		It("keeps keys.observed and changes nothing of the platform when its update fails", func(ctx SpecContext) {
			rotateAndLose(ctx) // a removal without an event: forget finds it and writes keys.observed
			var before []byte
			Expect(pool.QueryRow(ctx, `SELECT credentials FROM platform_trace_destination`).Scan(&before)).To(Succeed())
			// The fault is in the database: every update of the platform row fails, inside the savepoint.
			_, err := pool.Exec(ctx, `CREATE FUNCTION koinon_test_refuse_update() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'koinon test: platform update refused'; END $$;
				CREATE TRIGGER koinon_test_refuse_update BEFORE UPDATE ON platform_trace_destination
				FOR EACH ROW EXECUTE FUNCTION koinon_test_refuse_update()`)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) {
				_, err := pool.Exec(ctx, `DROP TRIGGER koinon_test_refuse_update ON platform_trace_destination; DROP FUNCTION koinon_test_refuse_update()`)
				Expect(err).NotTo(HaveOccurred())
			})
			mark := auditMark(ctx)

			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(And(
				ContainSubstring("platform trace destination"), ContainSubstring("platform update refused"))))

			Expect(keyEventsSince(ctx, mark)).To(ConsistOf(HaveField("Action", keyscli.ActionObserved)), "the keys.observed it found is kept")
			var forgets int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE chain_seq > $1 AND action = $2`, mark, keyscli.ForgetAction).Scan(&forgets)).To(Succeed())
			Expect(forgets).To(BeZero(), "no keys.forget without its change")
			var after []byte
			var lostAt *time.Time
			Expect(pool.QueryRow(ctx, `SELECT credentials, credential_lost_at FROM platform_trace_destination`).Scan(&after, &lostAt)).To(Succeed())
			Expect(after).To(MatchJSON(before), "the platform row is unchanged")
			Expect(lostAt).To(BeNil())
		})
	})

	Describe("the storage probe of a new repository epoch (#285)", func() {
		// withLostRepository seals an org's values and an agent's home repository under the KEK that is
		// then lost, so that forget has an epoch to start.
		withLostRepository := func(ctx SpecContext) (orgRows, pgtype.UUID) {
			GinkgoHelper()
			o := sealOrg(ctx)
			var agent pgtype.UUID
			Expect(pool.QueryRow(ctx, `SELECT id FROM agents WHERE org_id = $1`, o.org).Scan(&agent)).To(Succeed())
			_, err := checkpoint.Passwords{Q: store.New(pool), Keys: ring(ctx), Begin: pool.Begin}.Current(ctx, o.org, agent)
			Expect(err).NotTo(HaveOccurred())
			rotateAndLose(ctx)
			tool.Yes = true
			return o, agent
		}
		unchanged := func(ctx SpecContext, o orgRows, agent pgtype.UUID) {
			GinkgoHelper()
			lost, work := orgState(ctx, o)
			Expect(lost).To(BeFalse(), "nothing of the org is marked lost")
			Expect(work).NotTo(Equal("expired"))
			var epochs, probes int
			Expect(pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM home_repository_epochs WHERE agent_id = $1), (SELECT count(*) FROM home_repository_probes)`, agent).Scan(&epochs, &probes)).To(Succeed())
			Expect(epochs).To(BeZero(), "no repository epoch was started")
			Expect(probes).To(BeZero(), "the probe is deleted")
		}

		It("starts the epoch when the prefix is free", func(ctx SpecContext) {
			o, agent := withLostRepository(ctx)
			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
			var epoch int32
			Expect(pool.QueryRow(ctx, `SELECT max(repository_epoch) FROM home_repository_epochs WHERE agent_id = $1`, agent).Scan(&epoch)).To(Succeed())
			Expect(epoch).To(Equal(int32(1)))
			lost, _ := orgState(ctx, o)
			Expect(lost).To(BeTrue())
		})

		It("refuses a prefix that holds a repository, and marks nothing lost", func(ctx SpecContext) {
			o, agent := withLostRepository(ctx)
			answerWith(func(store.HomeRepositoryProbe) (string, string) { return "taken", "" })
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(And(
				ContainSubstring("nothing was changed"), ContainSubstring("already holds a repository the database does not record"))))
			unchanged(ctx, o, agent)
		})

		It("refuses when the check errors, and marks nothing lost", func(ctx SpecContext) {
			o, agent := withLostRepository(ctx)
			answerWith(func(store.HomeRepositoryProbe) (string, string) { return "error", "could not list" })
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(ContainSubstring("could not check the storage prefix")))
			unchanged(ctx, o, agent)
		})

		It("refuses when koinon-agentd does not answer in time, and marks nothing lost", func(ctx SpecContext) {
			o, agent := withLostRepository(ctx)
			answerWith(nil)
			tool.ProbeTimeout = time.Second
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(ContainSubstring("did not check the storage within 1s")))
			unchanged(ctx, o, agent)
		})
	})

	Describe("with a database that limits the run", func() {
		// poolWith opens a pool on the test database with the connection limit and the server's
		// idle-in-transaction timeout (0 for none) given.
		poolWith := func(ctx SpecContext, maxConns int32, idle time.Duration) *pgxpool.Pool {
			GinkgoHelper()
			cfg := pool.Config().Copy()
			cfg.MaxConns = maxConns
			if idle > 0 {
				cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = fmt.Sprint(idle.Milliseconds())
			}
			p, err := pgxpool.NewWithConfig(ctx, cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			return p
		}

		It("refuses a pool of one connection before asking and before the lock, and changes nothing", func(ctx SpecContext) {
			o := sealOrg(ctx)
			rotateAndLose(ctx)
			tool.Pool = poolWith(ctx, 1, 0)
			tool.Yes = true
			lock := holdKeyLock(ctx) // a run that got past the check would wait here, not fail
			defer func() { _ = lock.Rollback(ctx) }()

			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(And(
				ContainSubstring("at least 2 connections"), ContainSubstring("pool_max_conns=1"))))
			lost, work := orgState(ctx, o)
			Expect(lost).To(BeFalse())
			Expect(work).NotTo(Equal("expired"))
		}, SpecTimeout(10*time.Second))

		It("finishes with a pool of two connections when an agent's repository needs a new epoch", func(ctx SpecContext) {
			// The probe runs before the org's transaction (#285): the Change holds one connection, and
			// the probe and then the transaction use the other.
			o := sealOrg(ctx)
			var agent pgtype.UUID
			Expect(pool.QueryRow(ctx, `SELECT id FROM agents WHERE org_id = $1`, o.org).Scan(&agent)).To(Succeed())
			_, err := checkpoint.Passwords{Q: store.New(pool), Keys: ring(ctx), Begin: pool.Begin}.Current(ctx, o.org, agent)
			Expect(err).NotTo(HaveOccurred())
			rotateAndLose(ctx)
			tool.Pool = poolWith(ctx, 2, 0)
			tool.Yes = true

			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
			var epoch int32
			Expect(pool.QueryRow(ctx, `SELECT max(repository_epoch) FROM home_repository_epochs WHERE agent_id = $1`, agent).Scan(&epoch)).To(Succeed())
			Expect(epoch).To(Equal(int32(1)))
			lost, _ := orgState(ctx, o)
			Expect(lost).To(BeTrue())
		}, SpecTimeout(30*time.Second))

		It("finishes when the server ends idle transactions sooner than the run takes", func(ctx SpecContext) {
			o := sealOrg(ctx)
			rotateAndLose(ctx)
			tool.Pool = poolWith(ctx, 4, 300*time.Millisecond)
			tool.Yes = true
			// Each provider update takes longer than the timeout, so the lock's transaction idles past it.
			_, err := pool.Exec(ctx, `CREATE FUNCTION koinon_test_slow_update() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_sleep(0.8); RETURN NEW; END $$;
				CREATE TRIGGER koinon_test_slow_update BEFORE UPDATE ON providers
				FOR EACH ROW EXECUTE FUNCTION koinon_test_slow_update()`)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) {
				_, err := pool.Exec(ctx, `DROP TRIGGER koinon_test_slow_update ON providers; DROP FUNCTION koinon_test_slow_update()`)
				Expect(err).NotTo(HaveOccurred())
			})

			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
			lost, work := orgState(ctx, o)
			Expect(lost).To(BeTrue())
			Expect(work).To(Equal("expired"))
		}, SpecTimeout(30*time.Second))
	})

	Describe("when the KEK is restored after the first check", func() {
		BeforeEach(func(ctx SpecContext) {
			sealPlatform(ctx)
			tool.Yes = true
		})

		// restoreWhileWaiting runs Forget while the key events' lock is held elsewhere, restores the KEK
		// (keys add from escrow) while it waits, after its first check, and then releases the lock.
		restoreWhileWaiting := func(ctx SpecContext) error {
			GinkgoHelper()
			lock := holdKeyLock(ctx)
			done := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				done <- tool.Forget(ctx, lostKEK)
			}()
			Eventually(func() bool { return advisoryWaiting(ctx) }).Should(BeTrue(), "forget waits for the lock")

			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			sec.Data[lostKEK] = secretstore.EncodeKey(lostKey)
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(lock.Rollback(ctx)).To(Succeed())
			var got error
			Eventually(done).Should(Receive(&got))
			return got
		}
		// forgetEvents counts the keys.forget events written after mark: in the install chain, and in the
		// chains of orgs.
		forgetEvents := func(ctx SpecContext, mark int64, orgs ...pgtype.UUID) (install, org int) {
			GinkgoHelper()
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE chain_seq > $1 AND action = $2`, mark, keyscli.ForgetAction).Scan(&install)).To(Succeed())
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = $1 AND org_id = ANY($2)`, keyscli.ForgetAction, orgs).Scan(&org)).To(Succeed())
			return install, org
		}

		It("is refused under the lock, changes nothing and writes no forget event", func(ctx SpecContext) {
			rotateAndLose(ctx)
			mark := auditMark(ctx)
			Expect(restoreWhileWaiting(ctx)).To(MatchError(ContainSubstring("in the key set again, so it is not lost; nothing was changed")))

			Expect(platformLost(ctx)).To(BeFalse(), "the platform row is not marked lost")
			Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 1}), "nothing changed")
			install, _ := forgetEvents(ctx, mark)
			Expect(install).To(BeZero())
		})

		It("is refused before any org part: no credential is lost, no saved work expires, no org event", func(ctx SpecContext) {
			a, b := sealOrg(ctx), sealOrg(ctx)
			rotateAndLose(ctx)
			_, workBefore := orgState(ctx, a)
			Expect(workBefore).NotTo(Equal("expired"))
			mark := auditMark(ctx)
			Expect(restoreWhileWaiting(ctx)).To(MatchError(ContainSubstring("in the key set again, so it is not lost; nothing was changed")))

			for _, o := range []orgRows{a, b} {
				lost, work := orgState(ctx, o)
				Expect(lost).To(BeFalse(), "the org's provider key is not marked lost")
				Expect(work).To(Equal(workBefore), "the org's saved work did not expire")
			}
			Expect(platformLost(ctx)).To(BeFalse())
			Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 5}), "two providers, two saved works and the platform: nothing changed")
			install, org := forgetEvents(ctx, mark, a.org, b.org)
			Expect(install).To(BeZero(), "no install keys.forget")
			Expect(org).To(BeZero(), "no org keys.forget")
			Expect(out.String()).NotTo(ContainSubstring("Forgot KEK"))
		})
	})

	Describe("without a terminal", func() {
		var org pgtype.UUID
		BeforeEach(func(ctx SpecContext) {
			var user pgtype.UUID
			org, user, _ = pgtest.NewOrg(ctx, pool)
			_, err := catalog.New(pool, ring(ctx), nil).CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
				Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-a",
			})
			Expect(err).NotTo(HaveOccurred())
			rotateAndLose(ctx)
		})

		It("refuses without --yes, because it cannot be undone", func(ctx SpecContext) {
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(And(ContainSubstring("cannot be undone"), ContainSubstring("--yes"))))
			Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 1}), "nothing changed")
			tool.Yes = true
			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
			Expect(usage(ctx)).To(BeEmpty())
		})

		It("records the platform part as an install event even with no platform admin", func(ctx SpecContext) {
			_, err := pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url) VALUES ('datadog', 'https://otlp.example.com')
				ON CONFLICT (singleton) DO NOTHING`)
			Expect(err).NotTo(HaveOccurred())
			// Sealed with the lost KEK: copy a provider's envelope's kek_id into a stand-in row.
			_, err = pool.Exec(ctx, `UPDATE platform_trace_destination SET credentials = jsonb_build_object('kek_id', $1::text)`, lostKEK)
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `DELETE FROM platform_admins`)
			Expect(err).NotTo(HaveOccurred())
			count := func() (n int) {
				Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = $1 AND target_id = $2 AND actor_kind = 'cli'`,
					keyscli.ForgetAction, lostKEK).Scan(&n)).To(Succeed())
				return n
			}
			before := count()
			tool.Yes = true
			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
			Expect(usage(ctx)).To(BeEmpty())
			Expect(count()).To(Equal(before+1), "one install event for the platform part")
		})
	})

	// Issue #322: IDs are never reused on any add path, not only in the recovery from a lost active KEK.
	Describe("adding a KEK under an ID that database envelopes still carry", func() {
		It("is refused by keys add and by the key-admin Job, with or without the recovery flag, and changes nothing", func(ctx SpecContext) {
			const removed, freshKEK = "kek-20251201-eeee", "kek-20261201-ffff"
			_, err := pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url) VALUES ('datadog', 'https://otlp.example.com')
				ON CONFLICT (singleton) DO NOTHING`)
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE platform_trace_destination SET credentials = jsonb_build_object('kek_id', $1::text)`, removed)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) {
				_, err := pool.Exec(ctx, `UPDATE platform_trace_destination SET credentials = NULL`)
				Expect(err).NotTo(HaveOccurred())
			})
			Expect(keyscli.UsedID(ctx, pool, removed)).To(BeTrue())

			kms := &secretstoretest.FakeKMS{}
			admin, err := secretstore.NewKeyAdminWrappers(secretstoretest.BoundOnlyUnset, kms)
			Expect(err).NotTo(HaveOccurred())
			admin = admin.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
			adminStore := keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek", Wrappers: admin}
			probe := func(ctx context.Context, id string) (bool, error) { return keyscli.UsedID(ctx, pool, id) }
			job := func(add string, recovery bool, used func(context.Context, string) (bool, error)) keyscli.KeyAdmin {
				return keyscli.KeyAdmin{Store: adminStore, AddID: add, ActivateIfNoActive: recovery, UsedID: used, Out: out, Now: func() time.Time { return now }}
			}
			snapshot := func() map[string][]byte {
				sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return sec.Data
			}
			before := snapshot()

			By("keys add")
			Expect(tool.Add(ctx, removed)).To(MatchError(ContainSubstring("seals values in the database")))
			By("the Job's plain add")
			Expect(job(removed, false, probe).Run(ctx)).To(MatchError(ContainSubstring("seals values in the database")))
			By("the Job's recovery flag while the active KEK works")
			Expect(job(removed, true, probe).Run(ctx)).To(MatchError(ContainSubstring("seals values in the database")))
			By("the Job without a probe fails closed")
			Expect(job(removed, false, nil).Run(ctx)).To(MatchError(ContainSubstring("check that the new KEK ID is unused")))
			Expect(snapshot()).To(Equal(before), "the Secret is unchanged")
			Expect(kms.Wraps()).To(BeZero(), "nothing was wrapped")

			By("keys add fails closed without a database")
			Expect((&keyscli.Tool{Store: ks, Out: out, Now: func() time.Time { return now }}).Add(ctx, freshKEK)).To(MatchError(ContainSubstring("check that KEK ID")))
			Expect(snapshot()).To(Equal(before))

			By("an unused ID still adds")
			Expect(job(freshKEK, false, probe).Run(ctx)).To(Succeed())
			Expect(snapshot()).To(HaveKey(freshKEK))
		})
	})

	// Issue #255: the KMS key that wrapped the active KEK is gone for good, so the key set does not load
	// (the active KEK is not among the keys). The key-admin Job adds a new active KEK, then keys forget runs.
	Describe("when the lost KEK was the active one", func() {
		It("goes from an active KEK that cannot be unwrapped to a new active KEK to keys forget", func(ctx SpecContext) {
			org, user, _ := pgtest.NewOrg(ctx, pool)
			providers := catalog.New(pool, ring(ctx), nil)
			p, err := providers.CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
				Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-a",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 1}))

			By("the operator removes the lost KEK's entry; `active` still names it, so nothing loads")
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(sec.Data, lostKEK)
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			tool.Yes = true
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(ContainSubstring("is not among the keys")))

			By("the usage probe sees the envelopes under the removed ID, and none under a new one")
			Expect(keyscli.UsedID(ctx, pool, lostKEK)).To(BeTrue())
			Expect(keyscli.UsedID(ctx, pool, nextKEK)).To(BeFalse())

			By("the key-admin Job adds a KEK and makes it active")
			kms := &secretstoretest.FakeKMS{}
			admin, err := secretstore.NewKeyAdminWrappers(secretstoretest.BoundOnlyUnset, kms)
			Expect(err).NotTo(HaveOccurred())
			admin = admin.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
			ks = keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek", Wrappers: admin}

			By("it refuses the ID of another removed KEK that still seals a value and is not the active one")
			const otherRemoved = "kek-20251201-eeee"
			_, err = pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url) VALUES ('datadog', 'https://otlp.example.com')
				ON CONFLICT (singleton) DO NOTHING`)
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE platform_trace_destination SET credentials = jsonb_build_object('kek_id', $1::text)`, otherRemoved)
			Expect(err).NotTo(HaveOccurred())
			before, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect((keyscli.KeyAdmin{
				Store: ks, AddID: otherRemoved, ActivateIfNoActive: true, Out: out,
				UsedID: func(ctx context.Context, id string) (bool, error) { return keyscli.UsedID(ctx, pool, id) }, Now: func() time.Time { return now },
			}).Run(ctx)).To(MatchError(ContainSubstring("seals values in the database")))
			after, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(after.Data).To(Equal(before.Data), "the Secret is unchanged")
			Expect(after.Data).NotTo(HaveKey(otherRemoved))
			Expect(string(after.Data["active"])).To(Equal(lostKEK), "the active entry still names the lost KEK")
			Expect(kms.Wraps()).To(BeZero())
			_, err = pool.Exec(ctx, `UPDATE platform_trace_destination SET credentials = NULL`)
			Expect(err).NotTo(HaveOccurred())

			By("with a new ID the recovery goes through")
			Expect((keyscli.KeyAdmin{
				Store: ks, AddID: nextKEK, ActivateIfNoActive: true, Out: out,
				UsedID: func(ctx context.Context, id string) (bool, error) { return keyscli.UsedID(ctx, pool, id) }, Now: func() time.Time { return now },
			}).Run(ctx)).To(Succeed())
			set, err := ks.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(set.Active).To(Equal(nextKEK))

			By("keys status and keys forget now run")
			tool = &keyscli.Tool{Pool: pool, Store: ks, Out: out, Now: func() time.Time { return now }, Yes: true, ProbeTimeout: 5 * time.Second, ProbePoll: 10 * time.Millisecond}
			Expect(tool.Status(ctx)).To(Succeed())
			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
			Expect(usage(ctx)).To(BeEmpty())
			var markedLost, sealed bool
			Expect(pool.QueryRow(ctx, `SELECT credential_lost_at IS NOT NULL, api_key IS NOT NULL FROM providers WHERE org_id = $1 AND id = $2::uuid`, org, p.ID).Scan(&markedLost, &sealed)).To(Succeed())
			Expect(markedLost).To(BeTrue(), "the provider key is marked lost")
			Expect(sealed).To(BeFalse())

			By("new values are sealed with the new active KEK")
			r := ring(ctx)
			_, err = catalog.New(pool, r, nil).CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
				Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-b",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(usage(ctx)).To(Equal(map[string]int{nextKEK: 1}))
		})
	})

	Describe("on a terminal", func() {
		var asked *bytes.Buffer
		ask := func(input string) {
			asked = &bytes.Buffer{}
			tool.Prompt = &cli.Prompter{In: strings.NewReader(input), Out: asked, Accessible: true}
		}
		var org pgtype.UUID
		BeforeEach(func(ctx SpecContext) {
			var user pgtype.UUID
			org, user, _ = pgtest.NewOrg(ctx, pool)
			_, err := catalog.New(pool, ring(ctx), nil).CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
				Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-a",
			})
			Expect(err).NotTo(HaveOccurred())
			rotateAndLose(ctx)
		})

		It("wants the KEK ID typed; without an ID it offers the KEKs missing from the key set", func(ctx SpecContext) {
			ask("")
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(cli.ErrCancelled))
			Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 1}), "nothing changed")

			ask("1\nnope\n" + lostKEK + "\n")
			Expect(tool.Forget(ctx, "")).To(Succeed())
			Expect(asked.String()).To(And(
				ContainSubstring(lostKEK+"  seals 1 value(s), not in the key set"),
				ContainSubstring("Type the KEK ID to continue"),
				ContainSubstring("type "+lostKEK+" to continue"),
				ContainSubstring("backup escrow"),
			))
			Expect(asked.String()).NotTo(ContainSubstring(nextKEK+"  seals"), "a KEK in the key set is not offered")
			Expect(usage(ctx)).To(BeEmpty())
		})

		It("changes nothing when the KEK is restored while the question is open", func(ctx SpecContext) {
			restore := func() {
				sec, err := kube.CoreV1().Secrets(ns).Get(context.Background(), "koinon-kek", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				sec.Data[lostKEK] = secretstore.EncodeKey(lostKey)
				_, err = kube.CoreV1().Secrets(ns).Update(context.Background(), sec, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
			asked = &bytes.Buffer{}
			tool.Prompt = &cli.Prompter{In: &onRead{r: strings.NewReader(lostKEK + "\n"), first: restore}, Out: asked, Accessible: true}
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(ContainSubstring("in the key set again")))
			Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 1}), "nothing changed")
		})

		It("has nothing to pick when every KEK in use is in the key set", func(ctx SpecContext) {
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			sec.Data[lostKEK] = secretstore.EncodeKey(lostKey) // restored from escrow after all
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ask("1\n")
			Expect(tool.Forget(ctx, "")).To(MatchError(ContainSubstring("nothing is lost")))
		})
	})
})

// onRead calls first before its first read: what happens while a question waits for its answer.
type onRead struct {
	r     io.Reader
	first func()
	done  bool
}

func (o *onRead) Read(p []byte) (int, error) {
	if !o.done {
		o.done = true
		o.first()
	}
	return o.r.Read(p)
}
