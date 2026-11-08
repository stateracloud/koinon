// SPDX-License-Identifier: Apache-2.0

package scopesync_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/codehost/scopesync"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/sinkclass"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// Obviously fake tokens (AGENTS.md).
const (
	readTok  = "koinon_test_not-a-real-read-token"
	writeTok = "koinon_test_not-a-real-write-token"
)

var _ = Describe("the code-host scope refresh (ADR 0091, #115)", func() {
	var (
		q      *store.Queries
		scope  *orgsettings.Service
		keys   *secretstore.Keyring
		org    pgtype.UUID
		logBuf *bytes.Buffer
		now    = time.Now
	)
	BeforeEach(func(ctx SpecContext) {
		q, scope = store.New(pool), orgsettings.NewService(pool)
		org, _, _ = pgtest.NewOrg(ctx, pool)
		var err error
		keys, err = secretstore.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}, "k1")
		Expect(err).NotTo(HaveOccurred())
		logBuf = &bytes.Buffer{}
	})
	syncer := func(hosts *codehost.Registry) *scopesync.Syncer {
		return scopesync.New(pool, hosts, keys, slog.New(slog.NewTextHandler(logBuf, nil)))
	}
	addConn := func(ctx SpecContext, connector string, config []byte, fields map[string][]byte) store.Connection {
		GinkgoHelper()
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		env, err := connections.SealFields(keys, org, id, fields)
		Expect(err).NotTo(HaveOccurred())
		row, err := q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: connector, DisplayName: connector, Config: config, SecretEnvelope: env, Source: store.ConfigSourceUi,
		})
		Expect(err).NotTo(HaveOccurred())
		return row
	}
	githubConn := func(ctx SpecContext, f *githubtest.Fake) store.Connection {
		GinkgoHelper()
		cfg, err := json.Marshal(github.Config{AppID: f.AppID, InstallationID: f.InstallationID})
		Expect(err).NotTo(HaveOccurred())
		return addConn(ctx, github.HostID, cfg, map[string][]byte{github.PrivateKeyField: f.PEM})
	}
	giteaConn := func(ctx SpecContext, f *fakegitea.Fake) store.Connection {
		GinkgoHelper()
		raw, err := json.Marshal(gitea.NewConfig(codehost.Site{BaseURL: f.URL(), CABundle: f.CAPEM()}, []byte(readTok), []byte(writeTok), nil, time.Now()))
		Expect(err).NotTo(HaveOccurred())
		return addConn(ctx, gitea.HostID, raw, map[string][]byte{gitea.ReadTokenField: []byte(readTok), gitea.WriteTokenField: []byte(writeTok)})
	}
	// classify is the sink class of a code-host write to owner/repo through the connection.
	classify := func(ctx SpecContext, conn store.Connection, owner, repo string) sinkclass.Class {
		GinkgoHelper()
		l, err := scope.LeakGuard(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		d := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}
		return sinkclass.Classify(d, map[string]string{"owner": owner, "repo": repo}, conn.ID.String(), l.Scope(), nil, now())
	}

	Describe("a GitHub connection", func() {
		var (
			fake  *githubtest.Fake
			hosts *codehost.Registry
		)
		BeforeEach(func() {
			fake = githubtest.New()
			DeferCleanup(fake.Server.Close)
			fake.Repositories = []string{"secret", "open", "enterprise"}
			fake.Private = map[string]bool{"secret": true}
			fake.Internal = map[string]bool{"enterprise": true}
			hosts = providers.New(github.NewClient(github.Options{BaseURL: fake.URL()}))
		})

		It("is external-high before the first reading, then internal only for a private repository", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.ExternalHigh), "nothing read yet")

			Expect(syncer(hosts).Sync(ctx, conn, scopesync.Actor)).To(Succeed())

			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.Internal))
			Expect(classify(ctx, conn, "ACME", "Secret")).To(Equal(sinkclass.Internal), "names compare case-insensitively")
			Expect(classify(ctx, conn, "acme", "open")).To(Equal(sinkclass.ExternalHigh), "public")
			Expect(classify(ctx, conn, "acme", "enterprise")).To(Equal(sinkclass.ExternalHigh), "internal to the enterprise counts as public")
			Expect(classify(ctx, conn, "elsewhere", "secret")).To(Equal(sinkclass.ExternalHigh), "a foreign owner")
			l, err := scope.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			s := l.ScopeSummary(now())
			Expect(s.Repositories).To(Equal(3))
			Expect(s.Private).To(Equal(1))
			Expect(s.Public).To(Equal(1))
			Expect(s.Unknown).To(Equal(1), "GitHub's internal is stored as unknown, never as the scope's internal")
		})

		It("refuses a sync that took its credential before the secret was replaced (#244)", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			Expect(syncer(hosts).Sync(ctx, conn, scopesync.Actor)).To(Succeed())
			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.Internal))

			By("the secret is replaced, which drops the scope")
			_, err := pool.Exec(ctx, `UPDATE connections SET secret_fingerprint = 'SHA256:replaced' WHERE org_id = $1 AND id = $2`, conn.OrgID, conn.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(orgsettings.ClearCodeHostScope(ctx, q, conn.OrgID, conn.ID)).To(Succeed())

			By("the sync that holds the row as it was cannot write")
			Expect(syncer(hosts).Sync(ctx, conn, scopesync.Actor)).To(MatchError(orgsettings.ErrCredentialChanged))
			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.ExternalHigh))
		})

		It("keeps a connection syncing when the last page crosses the bound, truncating the list", func(ctx SpecContext) {
			fake.Repositories, fake.Private = nil, map[string]bool{}
			for i := range 200 { // two full pages of 100; the bound falls inside the second
				name := "repo-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
				fake.Repositories = append(fake.Repositories, name)
				fake.Private[name] = true
			}
			conn := githubConn(ctx, fake)
			s := syncer(hosts)
			scopesync.SetMaxRepos(s, 150)
			Expect(s.Sync(ctx, conn, scopesync.Actor)).To(Succeed())
			l, err := scope.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.ScopeSummary(now()).Repositories).To(Equal(150))
			Expect(logBuf.String()).To(ContainSubstring("code-host scope truncated"))
		})

		It("runs one round at a time across replicas, and a later replica does not read again (one leader per round)", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			var lists atomic.Int32
			reached, release := make(chan struct{}, 1), make(chan struct{})
			inner := fake.Server.Config.Handler
			fake.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/installation/repositories" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					lists.Add(1)
					select {
					case reached <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				inner.ServeHTTP(w, r)
			})
			a, b := syncer(hosts), syncer(hosts)
			done := make(chan error, 1)
			go func() { done <- a.RunOnce(ctx) }()
			Eventually(reached).Should(Receive(), "replica a reads the connection")

			By("replica b's round, while a's runs, reads nothing")
			bctx, cancel := context.WithTimeout(ctx, 3*time.Second) // a b that read would wait on a's release
			defer cancel()
			Expect(b.RunOnce(bctx)).To(Succeed())
			Expect(lists.Load()).To(BeEquivalentTo(1))

			close(release)
			Eventually(done).Should(Receive(BeNil()))
			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.Internal))

			By("a released the round lock: no session holds it (a leaked one would be reentrant for b on the same pooled connection)")
			var held int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks l, (SELECT hashtextextended($1, 0) AS k) key
				WHERE l.locktype = 'advisory' AND l.granted
				  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				  AND l.classid::bigint = (key.k >> 32) & 4294967295 AND l.objid::bigint = key.k & 4294967295`,
				scopesync.RoundLock).Scan(&held)).To(Succeed())
			Expect(held).To(BeZero())

			By("replica b's next round, a moment later, skips the connection a just read")
			Expect(b.RunOnce(ctx)).To(Succeed())
			Expect(lists.Load()).To(BeEquivalentTo(1))

			By("and that skip is MinAge, not a lock a left held: without MinAge, b's round reads")
			scopesync.SetMinAge(b, 0)
			Expect(b.RunOnce(ctx)).To(Succeed()) // a held lock would skip, not wait: try-lock
			Expect(lists.Load()).To(BeEquivalentTo(2))

			By("but a sync for one connection (create, test) always reads")
			Expect(b.Sync(ctx, conn, scopesync.Actor)).To(Succeed())
			Expect(lists.Load()).To(BeEquivalentTo(3))
		})

		It("bounds each connection's reading, so a hanging host does not hold up the others", func(ctx SpecContext) {
			stuck := fakegitea.NewTLS()
			DeferCleanup(stuck.Close)
			stuck.Server.Config.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			hung := giteaConn(ctx, stuck)
			conn := githubConn(ctx, fake)
			s := syncer(hosts)
			scopesync.SetTimeout(s, 500*time.Millisecond)
			start := time.Now()
			s.SyncOrg(ctx, org)
			Expect(time.Since(start)).To(BeNumerically("<", 10*time.Second))
			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.Internal), "the GitHub connection after the hanging one was read")
			Expect(logBuf.String()).To(ContainSubstring(hung.ID.String()))
		})

		It("audits a refresh only when the repositories change", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			s := syncer(hosts)
			replaces := func() int {
				GinkgoHelper()
				rows, err := q.ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 100})
				Expect(err).NotTo(HaveOccurred())
				n := 0
				for _, r := range rows {
					if r.Action == "org.code_host_scope.replace" && r.TargetID == conn.ID.String() {
						n++
					}
				}
				return n
			}
			s.SyncOrg(ctx, org)
			Expect(replaces()).To(Equal(1), "the first reading is a change")
			s.SyncOrg(ctx, org)
			Expect(replaces()).To(Equal(1), "the same list again is not")
			fake.Private = map[string]bool{}
			s.SyncOrg(ctx, org)
			Expect(replaces()).To(Equal(2), "secret turned public")
		})

		It("pages through every repository", func(ctx SpecContext) {
			fake.Repositories, fake.Private = nil, map[string]bool{}
			for i := range 230 { // three pages of 100
				name := "repo-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
				fake.Repositories = append(fake.Repositories, name)
				fake.Private[name] = true
			}
			conn := githubConn(ctx, fake)
			Expect(syncer(hosts).Sync(ctx, conn, scopesync.Actor)).To(Succeed())
			l, err := scope.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.ScopeSummary(now()).Private).To(Equal(230))
		})

		It("sees a repository turn public and one leave on the next refresh", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			Expect(syncer(hosts).Sync(ctx, conn, scopesync.Actor)).To(Succeed())
			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.Internal))

			fake.Private = map[string]bool{}
			fake.Repositories = []string{"secret", "open"}
			next := syncer(hosts)
			scopesync.SetMinAge(next, 0) // the next round, an interval later
			Expect(next.RunOnce(ctx)).To(Succeed())

			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.ExternalHigh), "now public")
			l, err := scope.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.ScopeSummary(now()).Repositories).To(Equal(2), "enterprise left the installation")
		})

		It("leaves the stored scope alone when the host cannot be read", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			Expect(syncer(hosts).Sync(ctx, conn, scopesync.Actor)).To(Succeed())
			fake.Server.Close()
			err := syncer(hosts).Sync(ctx, conn, scopesync.Actor)
			Expect(err).To(HaveOccurred())
			Expect(classify(ctx, conn, "acme", "secret")).To(Equal(sinkclass.Internal), "the reading ages out on its own")
		})

		It("does not keep a reading that is older than the freshness window", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			Expect(syncer(hosts).Sync(ctx, conn, scopesync.Actor)).To(Succeed())
			l, err := scope.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			later := now().Add(orgsettings.DefaultVisibilityMaxAge + time.Minute)
			d := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}
			got := sinkclass.Classify(d, map[string]string{"owner": "acme", "repo": "secret"}, conn.ID.String(), l.Scope(), nil, later)
			Expect(got).To(Equal(sinkclass.ExternalHigh))
		})

		It("skips a connection that is not a code host", func(ctx SpecContext) {
			row := addConn(ctx, "slack", []byte(`{}`), map[string][]byte{"token": []byte("koinon_test_not-a-real-token")})
			Expect(syncer(hosts).Sync(ctx, row, scopesync.Actor)).To(Succeed())
			Expect(syncer(hosts).RunOnce(ctx)).To(Succeed())
		})

		It("refuses without key-encryption keys", func(ctx SpecContext) {
			conn := githubConn(ctx, fake)
			Expect(scopesync.New(pool, hosts, nil, nil).Sync(ctx, conn, scopesync.Actor)).To(MatchError(connections.ErrNoKeyring))
		})
	})

	Describe("a Gitea connection", func() {
		var (
			fake  *fakegitea.Fake
			hosts *codehost.Registry
		)
		BeforeEach(func() {
			fake = fakegitea.NewTLS()
			DeferCleanup(fake.Close)
			fake.Tokens = map[string]string{}
			fake.AddScopedToken(readTok, "koinon-bot", "read")
			fake.AddScopedToken(writeTok, "koinon-bot", "write")
			for _, n := range []string{"app", "lib", "hub"} {
				fake.AddRepo(n)
				fake.Collaborate("koinon-bot", n, "write")
			}
			fake.SetVisibility("lib", "public")
			fake.SetVisibility("hub", "internal")
			hosts = providers.New(github.NewClient(github.Options{}))
		})

		It("classifies by the recorded visibility, public when unsure", func(ctx SpecContext) {
			conn := giteaConn(ctx, fake)
			Expect(classify(ctx, conn, "acme", "app")).To(Equal(sinkclass.ExternalHigh), "nothing read yet")

			Expect(syncer(hosts).RunOnce(ctx)).To(Succeed())

			Expect(classify(ctx, conn, "acme", "app")).To(Equal(sinkclass.Internal))
			Expect(classify(ctx, conn, "acme", "lib")).To(Equal(sinkclass.ExternalHigh), "public")
			Expect(classify(ctx, conn, "acme", "hub")).To(Equal(sinkclass.ExternalHigh), "internal to a private organization: public")
		})

		It("keeps each connection's list apart", func(ctx SpecContext) {
			a, b := giteaConn(ctx, fake), giteaConn(ctx, fake)
			Expect(syncer(hosts).Sync(ctx, a, scopesync.Actor)).To(Succeed())
			Expect(classify(ctx, a, "acme", "app")).To(Equal(sinkclass.Internal))
			Expect(classify(ctx, b, "acme", "app")).To(Equal(sinkclass.ExternalHigh), "b was not read")
		})
	})
})
