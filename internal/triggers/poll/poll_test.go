// SPDX-License-Identifier: Apache-2.0

package poll_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/triggers/poll"
)

const (
	readTok  = "koinon_test_not-a-real-read-token"
	writeTok = "koinon_test_not-a-real-write-token"
)

// delivery is one subscription that got one event.
type delivery struct{ Trigger, Key string }

// recorder is the Deliverer and the subscription source of a spec.
type recorder struct {
	mu   sync.Mutex
	subs []poll.Subscription
	got  []delivery
	runs int // Deliver calls
}

func (r *recorder) All(context.Context) ([]poll.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]poll.Subscription(nil), r.subs...), nil
}

func (r *recorder) Deliver(_ context.Context, _ *store.Queries, ev poll.Event, subs []poll.Subscription) (string, pgtype.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs++
	for _, s := range subs {
		r.got = append(r.got, delivery{Trigger: s.Trigger, Key: ev.Key})
	}
	return "", pgtype.UUID{}, nil
}

func (r *recorder) subscribe(s ...poll.Subscription) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, s...)
}

func (r *recorder) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

// replica is one koinon-api's poller with its DBOS runtime.
type replica struct {
	rt *durable.Runtime
	p  *poll.Poller
}

func startReplica(ctx context.Context, cfg poll.Config, rec *recorder, open poll.Opener) *replica {
	GinkgoHelper()
	rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1"})
	work, err := durable.NewOrgWork(rt, durable.OrgQueueConfig{
		Defaults:        durable.OrgLimits{Concurrency: 50, RatePerWindow: 100000},
		PollingInterval: 100 * time.Millisecond,
	})
	Expect(err).NotTo(HaveOccurred())
	p := poll.New(cfg, pool, work, rec)
	Expect(rt.Register(p.Registrar())).To(Succeed())
	Expect(rt.Launch(ctx)).To(Succeed())
	p.Bind(open, rec)
	r := &replica{rt: rt, p: p}
	DeferCleanup(r.stop)
	return r
}

func (r *replica) stop() { _ = r.rt.Shutdown(5 * time.Second) }

func (r *replica) pass(ctx context.Context, at time.Time) {
	GinkgoHelper()
	_, err := r.p.PollDue(ctx, at)
	Expect(err).NotTo(HaveOccurred())
}

func pollDedupes(ctx context.Context, org pgtype.UUID) int {
	GinkgoHelper()
	var n int
	Expect(pool.QueryRow(ctx, `SELECT count(*) FROM dedupe_records WHERE org_id = $1 AND source = $2`, org, durable.SourcePoll).Scan(&n)).To(Succeed())
	return n
}

var _ = Describe("shared code-host pollers (#25)", func() {
	var (
		q    *store.Queries
		org  pgtype.UUID
		keys *secretstore.Keyring
		rec  *recorder
		t0   time.Time
	)
	BeforeEach(func(ctx SpecContext) {
		q = store.New(pool)
		org, _, _ = pgtest.NewOrg(ctx, pool)
		var err error
		keys, err = secretstore.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}, "k1")
		Expect(err).NotTo(HaveOccurred())
		rec = &recorder{}
		t0 = time.Now().UTC().Truncate(time.Second)
	})
	addConn := func(ctx SpecContext, connector string, config []byte, fields map[string][]byte) pgtype.UUID {
		GinkgoHelper()
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		env, err := connections.SealFields(keys, org, id, fields)
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: connector, DisplayName: pgtest.Uniq(connector), Config: config, SecretEnvelope: env, Source: store.ConfigSourceUi,
		})
		Expect(err).NotTo(HaveOccurred())
		return id
	}
	sub := func(conn pgtype.UUID, res codehost.Resource, trigger string) poll.Subscription {
		return poll.Subscription{Org: org, Connection: conn, Resource: res, Agent: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Trigger: trigger}
	}
	row := func(ctx SpecContext, conn pgtype.UUID, res codehost.Resource) store.CodeHostPoller {
		GinkgoHelper()
		r, err := q.GetCodeHostPoller(ctx, store.GetCodeHostPollerParams{OrgID: org, ConnectionID: conn, Resource: res.String()})
		Expect(err).NotTo(HaveOccurred())
		return r
	}

	Describe("on GitHub", func() {
		var (
			fake *githubtest.Fake
			open poll.Opener
			conn pgtype.UUID
			app  codehost.Resource
			lib  codehost.Resource
		)
		BeforeEach(func(ctx SpecContext) {
			fake = githubtest.New()
			DeferCleanup(fake.Server.Close)
			fake.Repositories = []string{"app", "lib"}
			open = poll.ConnectionOpener{Hosts: providers.New(github.NewClient(github.Options{BaseURL: fake.URL()})), Keys: keys}
			cfg, err := json.Marshal(github.Config{AppID: fake.AppID, InstallationID: fake.InstallationID})
			Expect(err).NotTo(HaveOccurred())
			conn = addConn(ctx, github.HostID, cfg, map[string][]byte{github.PrivateKeyField: fake.PEM})
			app = codehost.Resource{Kind: codehost.ResourceRepoEvents, Repo: codehost.Repo{Owner: "acme", Name: "app"}}
			lib = codehost.Resource{Kind: codehost.ResourceRepoEvents, Repo: codehost.Repo{Owner: "acme", Name: "lib"}}
		})

		It("shares one poller between two triggers on one resource: one request per interval, and both get the event", func(ctx SpecContext) {
			rec.subscribe(sub(conn, app, "first"), sub(conn, app, "second"))
			r := startReplica(ctx, poll.Config{}, rec, open)

			fake.AddEvent("app", "PushEvent") // before the baseline: not an event for the triggers
			r.pass(ctx, t0)
			pollers, err := q.ListCodeHostPollers(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(pollers).To(HaveLen(1), "one poller for both triggers")
			gets, _ := fake.PollGets("app")
			Expect(gets).To(Equal(1))

			id := fake.AddEvent("app", "PullRequestEvent")
			r.pass(ctx, t0.Add(30*time.Second))
			gets, _ = fake.PollGets("app")
			Expect(gets).To(Equal(1), "not due within the interval")

			r.pass(ctx, t0.Add(time.Minute))
			gets, _ = fake.PollGets("app")
			Expect(gets).To(Equal(2), "one request for both triggers")
			Eventually(rec.deliveries).Should(ConsistOf(
				delivery{Trigger: "first", Key: "event:" + id},
				delivery{Trigger: "second", Key: "event:" + id},
			))
			Expect(rec.calls()).To(Equal(1), "one delivery for the poller")
			Expect(row(ctx, conn, app).Cursor).To(Equal(id))
		})

		It("does no trigger work on a 304: no dedupe record and no delivery", func(ctx SpecContext) {
			rec.subscribe(sub(conn, app, "watch"))
			r := startReplica(ctx, poll.Config{}, rec, open)
			fake.AddEvent("app", "PushEvent")
			r.pass(ctx, t0)
			r.pass(ctx, t0.Add(time.Minute))

			gets, notModified := fake.PollGets("app")
			Expect(gets).To(Equal(2))
			Expect(notModified).To(Equal(1), "the second poll sent the ETag and got a 304")
			got := row(ctx, conn, app)
			Expect(got.LastStatus).To(Equal(poll.OutcomeNotModified))
			Expect(got.ValidatorKind).To(Equal(codehost.ValidatorETag))
			Consistently(rec.calls, 500*time.Millisecond).Should(BeZero())
			Expect(pollDedupes(ctx, org)).To(BeZero())
		})

		It("follows each resource's X-Poll-Interval, never faster, and keeps the intervals apart", func(ctx SpecContext) {
			rec.subscribe(sub(conn, app, "slow"), sub(conn, lib, "fast"))
			fake.SetPollInterval("app", 120)
			fake.SetPollInterval("lib", 60)
			r := startReplica(ctx, poll.Config{Floor: 10 * time.Second}, rec, open)

			r.pass(ctx, t0)
			Expect(row(ctx, conn, app).NextPollAt.Time).To(BeTemporally("==", t0.Add(120*time.Second)))
			Expect(row(ctx, conn, lib).NextPollAt.Time).To(BeTemporally("==", t0.Add(60*time.Second)))

			r.pass(ctx, t0.Add(60*time.Second))
			appGets, _ := fake.PollGets("app")
			libGets, _ := fake.PollGets("lib")
			Expect([]int{appGets, libGets}).To(Equal([]int{1, 2}), "the key told 60 s does not pull the one told 120 s along")

			r.pass(ctx, t0.Add(119*time.Second))
			appGets, _ = fake.PollGets("app")
			Expect(appGets).To(Equal(1), "never faster than the provider asked")

			r.pass(ctx, t0.Add(120*time.Second))
			appGets, _ = fake.PollGets("app")
			libGets, _ = fake.PollGets("lib")
			Expect([]int{appGets, libGets}).To(Equal([]int{2, 3}))
		})

		It("does not let a provider interval below the floor shorten it", func(ctx SpecContext) {
			rec.subscribe(sub(conn, app, "watch"))
			fake.SetPollInterval("app", 30)
			r := startReplica(ctx, poll.Config{Floors: map[string]time.Duration{github.HostID: time.Minute}}, rec, open)
			r.pass(ctx, t0)
			got := row(ctx, conn, app)
			Expect(got.ProviderIntervalS).To(BeEquivalentTo(30))
			Expect(got.NextPollAt.Time).To(BeTemporally("==", t0.Add(time.Minute)))
			r.pass(ctx, t0.Add(30*time.Second))
			gets, _ := fake.PollGets("app")
			Expect(gets).To(Equal(1))
		})

		It("resumes from the stored cursor after a restart and delivers each item once", func(ctx SpecContext) {
			rec.subscribe(sub(conn, app, "watch"))
			first := startReplica(ctx, poll.Config{}, rec, open)
			first.pass(ctx, t0)
			e1 := fake.AddEvent("app", "IssuesEvent")
			first.pass(ctx, t0.Add(time.Minute))
			Eventually(rec.deliveries).Should(ConsistOf(delivery{Trigger: "watch", Key: "event:" + e1}))
			first.stop()

			e2 := fake.AddEvent("app", "IssuesEvent")
			second := startReplica(ctx, poll.Config{}, rec, open)
			second.pass(ctx, t0.Add(2*time.Minute))
			Eventually(rec.deliveries).Should(ConsistOf(
				delivery{Trigger: "watch", Key: "event:" + e1},
				delivery{Trigger: "watch", Key: "event:" + e2},
			))
			Consistently(rec.deliveries, 500*time.Millisecond).Should(HaveLen(2))
			Expect(row(ctx, conn, app).Cursor).To(Equal(e2))
		})

		It("waits out a rate limit at the time the provider names, without an error count", func(ctx SpecContext) {
			rec.subscribe(sub(conn, app, "watch"))
			r := startReplica(ctx, poll.Config{}, rec, open)
			r.pass(ctx, t0)
			fake.RateLimitNextPoll("app", 600)
			r.pass(ctx, t0.Add(time.Minute))
			got := row(ctx, conn, app)
			Expect(got.LastStatus).To(Equal(poll.OutcomeRateLimited))
			Expect(got.ConsecutiveFailures).To(BeZero())
			Expect(got.NextPollAt.Time).To(BeTemporally("~", time.Now().Add(600*time.Second), 5*time.Second))
		})

		It("drops the poller when no trigger needs it any more", func(ctx SpecContext) {
			rec.subscribe(sub(conn, app, "watch"))
			r := startReplica(ctx, poll.Config{}, rec, open)
			r.pass(ctx, t0)
			rec.mu.Lock()
			rec.subs = nil
			rec.mu.Unlock()
			r.pass(ctx, t0.Add(time.Minute))
			pollers, err := q.ListCodeHostPollers(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(pollers).To(BeEmpty())
		})
	})

	Describe("on Gitea, which sends no ETag", func() {
		var (
			fake *fakegitea.Fake
			open poll.Opener
			conn pgtype.UUID
			res  codehost.Resource
		)
		BeforeEach(func(ctx SpecContext) {
			fake = fakegitea.NewTLS()
			DeferCleanup(fake.Close)
			fake.Tokens = map[string]string{}
			fake.AddScopedToken(readTok, "koinon-bot", "read")
			fake.AddScopedToken(writeTok, "koinon-bot", "write")
			fake.AddRepo("app")
			fake.Collaborate("koinon-bot", "app", "write")
			open = poll.ConnectionOpener{Hosts: providers.New(github.NewClient(github.Options{})), Keys: keys}
			raw, err := json.Marshal(gitea.NewConfig(codehost.Site{BaseURL: fake.URL(), CABundle: fake.CAPEM()}, []byte(readTok), []byte(writeTok), nil, time.Now()))
			Expect(err).NotTo(HaveOccurred())
			conn = addConn(ctx, gitea.HostID, raw, map[string][]byte{gitea.ReadTokenField: []byte(readTok), gitea.WriteTokenField: []byte(writeTok)})
			res = codehost.Resource{Kind: codehost.ResourceIssueComments, Repo: codehost.Repo{Owner: "acme", Name: "app"}}
		})

		It("reports an identical page as unchanged and delivers a new comment once", func(ctx SpecContext) {
			rec.subscribe(sub(conn, res, "watch"))
			issue := fake.AddIssue("app", "a bug")
			fake.AddComment("app", issue, "before the trigger")
			r := startReplica(ctx, poll.Config{}, rec, open)
			r.pass(ctx, t0)
			Expect(row(ctx, conn, res).ValidatorKind).To(Equal(codehost.ValidatorPageHash))

			r.pass(ctx, t0.Add(time.Minute))
			got := row(ctx, conn, res)
			Expect(got.LastStatus).To(Equal(poll.OutcomeNotModified), "the same page")
			Expect(fake.Snapshot("app").Calls["GET repo comments"]).To(Equal(2))
			Consistently(rec.calls, 300*time.Millisecond).Should(BeZero())
			Expect(pollDedupes(ctx, org)).To(BeZero())

			id := fake.AddComment("app", issue, "after")
			r.pass(ctx, t0.Add(2*time.Minute))
			Expect(row(ctx, conn, res).LastStatus).To(Equal(poll.OutcomeChanged))
			Eventually(rec.deliveries).Should(ConsistOf(delivery{Trigger: "watch", Key: "comment:" + itoa(id)}))
			Expect(pollDedupes(ctx, org)).To(Equal(1))
		})
	})
})

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
