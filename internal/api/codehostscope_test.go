// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"time"

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
