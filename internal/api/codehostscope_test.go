// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

var _ = Describe("the code-host scope through ConnectionService (ADR 0091, #115)", func() {
	var (
		e     *env
		gh    *githubtest.Fake
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		gh.Repositories = []string{"handbook", "website"}
		gh.Private = map[string]bool{"handbook": true}
		e = newEnv(ctx, envOptions{
			keys:      secretstoretest.Keyring(),
			github:    github.NewClient(github.Options{BaseURL: gh.URL()}),
			ghFake:    gh,
			scopeSync: true,
		})
		org, owner = e.newTeamOrg(ctx)
	})
	repos := func(ctx SpecContext) func() []sinkclass.Repo {
		return func() []sinkclass.Repo {
			l, err := orgsettings.NewService(e.pool).LeakGuard(ctx, mustUUID(org))
			Expect(err).NotTo(HaveOccurred())
			return l.CodeHostRepos
		}
	}
	replaces := func(ctx SpecContext) []store.AuditEvent {
		GinkgoHelper()
		rows, err := store.New(e.pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: mustUUID(org), PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		var out []store.AuditEvent
		for _, r := range rows {
			if r.Action == "org.code_host_scope.replace" {
				out = append(out, r)
			}
		}
		return out
	}

	It("fills the scope when a connection is created, in the background, audited as the person", func(ctx SpecContext) {
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "acme", AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
		})
		Expect(err).NotTo(HaveOccurred())
		conn := res.GetConnection().GetId()
		Eventually(repos(ctx)).WithTimeout(10 * time.Second).Should(HaveLen(2))
		l, err := orgsettings.NewService(e.pool).LeakGuard(ctx, mustUUID(org))
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Scope().Internal(conn, "acme", "handbook", time.Now())).To(BeTrue())
		Expect(l.Scope().Internal(conn, "acme", "website", time.Now())).To(BeFalse())
		ev := replaces(ctx)
		Expect(ev).To(HaveLen(1))
		Expect(ev[0].ActorKind).To(Equal(audit.KindUser))

		By("testing it reads again, and a person's sync is audited even when nothing changed")
		_, err = owner.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: conn})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []store.AuditEvent { return replaces(ctx) }).WithTimeout(10 * time.Second).Should(HaveLen(2))
	})

	It("audits the reading under the request ID of the call that asked for it", func(ctx SpecContext) {
		events := func(action string) []store.AuditEvent {
			GinkgoHelper()
			rows, err := store.New(e.pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: mustUUID(org), PageSize: 100})
			Expect(err).NotTo(HaveOccurred())
			var out []store.AuditEvent
			for _, r := range rows {
				if r.Action == action {
					out = append(out, r)
				}
			}
			return out
		}
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "acme", AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []store.AuditEvent { return replaces(ctx) }).WithTimeout(10 * time.Second).Should(HaveLen(1))
		created := events("connection.create")
		Expect(created).To(HaveLen(1))
		Expect(created[0].RequestID).NotTo(BeEmpty())
		Expect(replaces(ctx)[0].RequestID).To(Equal(created[0].RequestID), "the reading shares the create's request ID")

		_, err = owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
			OrgId: org, ConnectionId: res.GetConnection().GetId(),
			Secret: &connectionv1alpha1.SetConnectionSecretRequest_Github{Github: &connectionv1alpha1.GitHubConnectionSecret{PrivateKey: string(gh.PEM)}},
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []store.AuditEvent { return replaces(ctx) }).WithTimeout(10 * time.Second).Should(HaveLen(2))
		rekeyed := events("connection.secret.set")
		Expect(rekeyed).To(HaveLen(1))
		Expect(rekeyed[0].RequestID).NotTo(BeEmpty())
		Expect(rekeyed[0].RequestID).NotTo(Equal(created[0].RequestID))
		ids := []string{}
		for _, r := range replaces(ctx) {
			ids = append(ids, r.RequestID)
		}
		Expect(ids).To(ContainElement(rekeyed[0].RequestID), "the re-key's reading shares the re-key's request ID")
	})

	It("drops what the old credential read when the secret is replaced", func(ctx SpecContext) {
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "acme", AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(repos(ctx)).WithTimeout(10 * time.Second).Should(HaveLen(2))

		By("the new credential cannot be read (the host is down): the old reading must not stay")
		gh.Server.Close()
		_, err = owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
			OrgId: org, ConnectionId: res.GetConnection().GetId(),
			Secret: &connectionv1alpha1.SetConnectionSecretRequest_Github{Github: &connectionv1alpha1.GitHubConnectionSecret{PrivateKey: string(gh.PEM)}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(repos(ctx)()).To(BeEmpty(), "deleted with the secret change, before any reading")
		Consistently(repos(ctx)).WithTimeout(time.Second).Should(BeEmpty())
	})
})

// The periodic refresh reads through the services' queue, as koinon-api wires it (SetQueue): one
// reading per connection at a time, and Drain waits for it.
var _ = Describe("the periodic code-host scope refresh beside a person's reading (#245)", func() {
	var (
		e     *env
		gh    *githubtest.Fake
		org   string
		owner *client
		conn  string

		mu       sync.Mutex
		inFlight int
		maxSeen  int
		hold     chan struct{} // while non-nil, a listing of the installation's repositories waits on it
	)
	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		gh.Repositories = []string{"handbook", "website"}
		gh.Private = map[string]bool{"handbook": true}
		next := gh.Server.Config.Handler
		gh.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/installation/repositories") {
				next.ServeHTTP(w, r)
				return
			}
			mu.Lock()
			inFlight++
			maxSeen = max(maxSeen, inFlight)
			wait := hold
			mu.Unlock()
			defer func() { mu.Lock(); inFlight--; mu.Unlock() }()
			if wait != nil {
				select {
				case <-wait:
				case <-r.Context().Done():
				}
			}
			next.ServeHTTP(w, r)
		})
		e = newEnv(ctx, envOptions{
			keys:      secretstoretest.Keyring(),
			github:    github.NewClient(github.Options{BaseURL: gh.URL()}),
			ghFake:    gh,
			scopeSync: true,
		})
		org, owner = e.newTeamOrg(ctx)
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "acme", AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
		})
		Expect(err).NotTo(HaveOccurred())
		conn = res.GetConnection().GetId()
		Eventually(func() int { return len(scopeReplaces(ctx, e, org)) }).WithTimeout(10 * time.Second).Should(Equal(1))
		Eventually(func() int { mu.Lock(); defer mu.Unlock(); return inFlight }).Should(BeZero())
		mu.Lock()
		hold, maxSeen = make(chan struct{}), 0
		mu.Unlock()
	})
	releaseListings := func() {
		mu.Lock()
		defer mu.Unlock()
		if hold != nil {
			close(hold)
			hold = nil
		}
	}
	listing := func() int { mu.Lock(); defer mu.Unlock(); return inFlight }
	// startRefresh runs the periodic refresh of the org (the path Run takes) in the background.
	startRefresh := func(ctx context.Context) <-chan struct{} {
		done := make(chan struct{})
		go func() { defer GinkgoRecover(); defer close(done); e.syncer.SyncOrg(ctx, mustUUID(org)) }()
		return done
	}

	It("never lists one connection twice at once, whoever asks", func(ctx SpecContext) {
		DeferCleanup(releaseListings)
		refresh := startRefresh(ctx)
		Eventually(listing).Should(Equal(1))

		_, err := owner.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: conn})
		Expect(err).NotTo(HaveOccurred())
		Consistently(listing).WithTimeout(500*time.Millisecond).Should(Equal(1), "the person's reading waits for the refresh's")

		releaseListings()
		Eventually(refresh).WithTimeout(10 * time.Second).Should(BeClosed())
		Eventually(func() int { return len(scopeReplaces(ctx, e, org)) }).WithTimeout(10*time.Second).Should(Equal(2), "the person's reading ran after it")
		mu.Lock()
		defer mu.Unlock()
		Expect(maxSeen).To(Equal(1))
	})

	It("is waited for by Drain", func(ctx SpecContext) {
		DeferCleanup(releaseListings)
		refresh := startRefresh(ctx)
		Eventually(listing).Should(Equal(1))

		drained := make(chan struct{})
		go func() { defer GinkgoRecover(); defer close(drained); e.rpc.Drain(context.Background()) }()
		Consistently(drained).WithTimeout(500 * time.Millisecond).ShouldNot(BeClosed())

		releaseListings()
		Eventually(drained).WithTimeout(10 * time.Second).Should(BeClosed())
		Eventually(refresh).WithTimeout(10 * time.Second).Should(BeClosed())
	})
})

// scopeReplaces are the org's code-host scope replacement audit events.
func scopeReplaces(ctx context.Context, e *env, org string) []store.AuditEvent {
	GinkgoHelper()
	rows, err := store.New(e.pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: mustUUID(org), PageSize: 100})
	Expect(err).NotTo(HaveOccurred())
	var out []store.AuditEvent
	for _, r := range rows {
		if r.Action == "org.code_host_scope.replace" {
			out = append(out, r)
		}
	}
	return out
}

// A replacement of a connection's scope and a change of the connection itself (its secret, or
// deleting it) lock the same things: the connection row, the connection's scope lock and its
// repository rows. They must take them in that order, or the two wait on each other and Postgres
// kills one with a deadlock (40P01), failing the person's request.
var _ = Describe("the code-host scope beside changes of its connection (#115)", func() {
	var (
		e     *env
		gh    *githubtest.Fake
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		e = newEnv(ctx, envOptions{
			keys:   secretstoretest.Keyring(),
			github: github.NewClient(github.Options{BaseURL: gh.URL()}),
			ghFake: gh,
		})
		org, owner = e.newTeamOrg(ctx)
	})
	create := func(ctx SpecContext) string {
		GinkgoHelper()
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "acme", AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
		})
		Expect(err).NotTo(HaveOccurred())
		return res.GetConnection().GetId()
	}
	reading := make([]orgsettings.CodeHostRepo, 200)
	for i := range reading {
		reading[i] = orgsettings.CodeHostRepo{Owner: "acme", Name: fmt.Sprintf("repo-%03d", i), Visibility: "private"}
	}
	fp := func(ctx SpecContext, conn string) string {
		GinkgoHelper()
		var f string
		Expect(e.pool.QueryRow(ctx, `SELECT secret_fingerprint FROM connections WHERE id = $1`, conn).Scan(&f)).To(Succeed())
		return f
	}
	// race runs replace and change at the same moment and returns both errors.
	race := func(replace, change func() error) (error, error) {
		start := make(chan struct{})
		var wg sync.WaitGroup
		var rerr, cerr error
		wg.Add(2)
		go func() { defer GinkgoRecover(); defer wg.Done(); <-start; rerr = replace() }()
		go func() { defer GinkgoRecover(); defer wg.Done(); <-start; cerr = change() }()
		close(start)
		wg.Wait()
		return rerr, cerr
	}
	noDeadlock := func(err error) {
		GinkgoHelper()
		if err == nil {
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			Expect(pgErr.Code).NotTo(Equal("40P01"), "deadlock: %v", err)
		}
		Expect(err).NotTo(MatchError(ContainSubstring("40P01")))
		Expect(err).NotTo(MatchError(ContainSubstring("deadlock")))
	}

	It("replaces the scope while the secret is replaced, without a deadlock", func(ctx SpecContext) {
		conn := create(ctx)
		svc := orgsettings.NewService(e.pool)
		for range 25 {
			rerr, cerr := race(
				func() error {
					return svc.ReplaceCodeHostRepos(ctx, mustUUID(org), audit.System("koinon-test:spec"), conn, fp(ctx, conn), reading, time.Now())
				},
				func() error {
					_, err := owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
						OrgId: org, ConnectionId: conn,
						Secret: &connectionv1alpha1.SetConnectionSecretRequest_Github{Github: &connectionv1alpha1.GitHubConnectionSecret{PrivateKey: string(gh.PEM)}},
					})
					return err
				})
			noDeadlock(rerr)
			noDeadlock(cerr)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(cerr).NotTo(HaveOccurred())
		}
	}, SpecTimeout(2*time.Minute))

	It("replaces the scope while the connection is deleted, without a deadlock", func(ctx SpecContext) {
		svc := orgsettings.NewService(e.pool)
		for range 15 {
			conn := create(ctx)
			// Rows for the delete's cascade to remove, so it waits on the replacement's.
			Expect(svc.ReplaceCodeHostRepos(ctx, mustUUID(org), audit.System("koinon-test:spec"), conn, fp(ctx, conn), reading, time.Now())).To(Succeed())
			rerr, cerr := race(
				func() error {
					return svc.ReplaceCodeHostRepos(ctx, mustUUID(org), audit.System("koinon-test:spec"), conn, fp(ctx, conn), reading, time.Now())
				},
				func() error {
					_, err := owner.conns.DeleteConnection(ctx, &connectionv1alpha1.DeleteConnectionRequest{OrgId: org, ConnectionId: conn})
					return err
				})
			noDeadlock(rerr)
			noDeadlock(cerr)
			if rerr != nil {
				Expect(rerr).To(MatchError(orgsettings.ErrNotFound), "the connection was deleted first")
			}
			Expect(cerr).NotTo(HaveOccurred())
		}
	}, SpecTimeout(2*time.Minute))
})
