// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
		tool = &keyscli.Tool{Pool: pool, Store: ks, Out: out, Now: func() time.Time { return now }}
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
		// A platform admin of their own, whose personal org holds no sealed value.
		personal, admin, _ := pgtest.NewOrg(ctx, pool)
		_, err := pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, admin)
		Expect(err).NotTo(HaveOccurred())
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
		var version, task, run pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agent_versions (org_id, agent_id, version, config) VALUES ($1, $2, 1, '{}') RETURNING id`, org, agent1).Scan(&version)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_user_id) VALUES ($1, 't', $2, 'user', $3) RETURNING id`, org, agent1, user).Scan(&task)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, ready_at, max_tokens) VALUES ($1, $2, $3, $4, now(), 1000) RETURNING id`, org, agent1, version, task).Scan(&run)).To(Succeed())
		var work pgtype.UUID
		Expect(pool.QueryRow(ctx, `SELECT uuidv7()`).Scan(&work)).To(Succeed())
		sealedWork, err := savedwork.SealPacked(r, org.String(), work.String(), []byte("packed"))
		Expect(err).NotTo(HaveOccurred())
		rawWork, err := json.Marshal(sealedWork)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO saved_work (org_id, id, task_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, envelope, expires_at)
			VALUES ($1, $2, $3, $4, $5, 'acme/widgets', $6, 'koinon/wip/abc-1', '{}', 6, $7, now() + interval '14 days')`,
			org, work, task, run, agent1, strings.Repeat("a", 40), rawWork)
		Expect(err).NotTo(HaveOccurred())

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
		rows, err := pool.Query(ctx, `SELECT org_id, actor_kind, actor_id, target_id, summary FROM audit_events WHERE action = $1 AND org_id <> $2 ORDER BY org_id`, keyscli.ForgetAction, personal)
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

		By("the platform's trace destination is audited in the platform admin's personal org")
		var platformSummary []byte
		var kind string
		Expect(pool.QueryRow(ctx, `SELECT actor_kind, summary FROM audit_events WHERE action = $1 AND org_id = $2`, keyscli.ForgetAction, personal).Scan(&kind, &platformSummary)).To(Succeed())
		Expect(kind).To(Equal(audit.KindCLI))
		Expect(string(platformSummary)).To(ContainSubstring("platformTraceDestination"))

		By("running it again finds nothing left")
		out.Reset()
		Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("nothing to forget"))
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

		It("changes nothing of the platform without a platform admin to audit it in, and finishes once there is one", func(ctx SpecContext) {
			_, err := pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url) VALUES ('datadog', 'https://otlp.example.com')
				ON CONFLICT (singleton) DO NOTHING`)
			Expect(err).NotTo(HaveOccurred())
			// Sealed with the lost KEK: copy a provider's envelope's kek_id into a stand-in row.
			_, err = pool.Exec(ctx, `UPDATE platform_trace_destination SET credentials = jsonb_build_object('kek_id', $1::text)`, lostKEK)
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `DELETE FROM platform_admins`)
			Expect(err).NotTo(HaveOccurred())
			tool.Yes = true
			Expect(tool.Forget(ctx, lostKEK)).To(MatchError(ContainSubstring("no platform admin exists")))
			Expect(usage(ctx)).To(Equal(map[string]int{lostKEK: 1}), "the org's values are done; the platform's is left as it was")

			var user pgtype.UUID
			Expect(pool.QueryRow(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&user)).To(Succeed())
			_, err = pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, user)
			Expect(err).NotTo(HaveOccurred())
			Expect(tool.Forget(ctx, lostKEK)).To(Succeed())
			Expect(usage(ctx)).To(BeEmpty())
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
			tool = &keyscli.Tool{Pool: pool, Store: ks, Out: out, Now: func() time.Time { return now }, Yes: true}
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
			Expect(tool.Add(ctx, lostKEK)).To(Succeed()) // restored from escrow after all
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
