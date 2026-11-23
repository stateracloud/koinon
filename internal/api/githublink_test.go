// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"net/url"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/apiassembly"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

// The GitHub link RPCs (issue #1175): who may start, list and remove a link, and the App's client
// secret surviving a private key rotation.
var _ = Describe("GitHub links", func() {
	var (
		e      *env
		gh     *githubtest.Fake
		ring   *secretstore.Keyring
		org    string
		owner  *client
		connID string
	)

	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		ring = secretstoretest.Keyring()
		e = newEnv(ctx, envOptions{
			keys: ring, github: github.NewClient(github.Options{BaseURL: gh.URL()}), ghFake: gh,
			settings: func(s *apiassembly.Settings) { s.PublicURL = "https://koinon.example.test" },
		})
		org, owner = e.newTeamOrg(ctx)
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "acme", AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
		})
		Expect(err).NotTo(HaveOccurred())
		connID = res.GetConnection().GetId()
	})

	// withUserAuth gives the connection the App's OAuth client, as the guided setup does.
	withUserAuth := func(ctx SpecContext) {
		GinkgoHelper()
		var orgID, id pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		Expect(id.Scan(connID)).To(Succeed())
		env, err := connections.SealFields(ring, orgID, id, map[string][]byte{
			connections.GitHubPrivateKeyField: gh.PEM, github.ClientSecretField: []byte(githubtest.FakeClientSecret),
		})
		Expect(err).NotTo(HaveOccurred())
		cfg, _ := json.Marshal(github.Config{AppID: gh.AppID, InstallationID: gh.InstallationID, ClientID: githubtest.FakeClientID})
		_, err = e.pool.Exec(ctx, `UPDATE connections SET config = $3, secret_envelope = $4 WHERE org_id = $1 AND id = $2`, orgID, id, cfg, env)
		Expect(err).NotTo(HaveOccurred())
	}

	It("starts a link for a member through an App with an OAuth client", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := member.conns.StartGitHubLink(ctx, &connectionv1alpha1.StartGitHubLinkRequest{OrgId: org, ConnectionId: connID})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition), "the App has no client ID and secret yet")

		withUserAuth(ctx)
		got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: connID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetConnection().GetGithub().GetClientId()).To(Equal(githubtest.FakeClientID))

		res, err := member.conns.StartGitHubLink(ctx, &connectionv1alpha1.StartGitHubLinkRequest{OrgId: org, ConnectionId: connID})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAuthorizeUrl()).To(HavePrefix(gh.WebURL() + "/login/oauth/authorize?"))
		Expect(res.GetAuthorizeUrl()).To(ContainSubstring("client_id=" + githubtest.FakeClientID))
		Expect(res.GetAuthorizeUrl()).To(ContainSubstring("redirect_uri=" + url.QueryEscape("https://koinon.example.test/auth/github/link/callback")))

		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		_, err = viewer.conns.StartGitHubLink(ctx, &connectionv1alpha1.StartGitHubLinkRequest{OrgId: org, ConnectionId: connID})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})

	It("lets a member remove only their own link and an admin list and remove any", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		other := e.userIn(ctx, org, store.OrgRoleMember)
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		for i, c := range []*client{member, other} {
			_, err := e.pool.Exec(ctx, `INSERT INTO github_user_links (org_id, user_id, host, github_user_id, github_login)
				VALUES ($1, $2, 'https://github.com', $3, $4)`, org, c.userID, 5000+i, "login-"+c.userID[:8])
			Expect(err).NotTo(HaveOccurred())
		}

		mine, err := member.conns.ListMyGitHubLinks(ctx, &connectionv1alpha1.ListMyGitHubLinksRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(mine.GetLinks()).To(HaveLen(1))
		Expect(mine.GetLinks()[0].GetUserId()).To(Equal(member.userID))
		Expect(mine.GetLinks()[0].GetEmail()).To(BeEmpty())

		_, err = member.conns.ListGitHubLinks(ctx, &connectionv1alpha1.ListGitHubLinksRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		all, err := admin.conns.ListGitHubLinks(ctx, &connectionv1alpha1.ListGitHubLinksRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(all.GetLinks()).To(HaveLen(2))

		_, err = member.conns.DeleteGitHubLink(ctx, &connectionv1alpha1.DeleteGitHubLinkRequest{OrgId: org, UserId: other.userID, Host: "https://github.com"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.conns.DeleteGitHubLink(ctx, &connectionv1alpha1.DeleteGitHubLinkRequest{OrgId: org, Host: "https://github.com"})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.conns.DeleteGitHubLink(ctx, &connectionv1alpha1.DeleteGitHubLinkRequest{OrgId: org, UserId: other.userID, Host: "https://github.com"})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.conns.DeleteGitHubLink(ctx, &connectionv1alpha1.DeleteGitHubLinkRequest{OrgId: org, UserId: other.userID, Host: "https://github.com"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'github_link.delete'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2))
	})

	It("keeps the App's client secret when an admin replaces the private key", func(ctx SpecContext) {
		withUserAuth(ctx)
		_, err := owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
			OrgId: org, ConnectionId: connID,
			Secret: &connectionv1alpha1.SetConnectionSecretRequest_Github{Github: &connectionv1alpha1.GitHubConnectionSecret{PrivateKey: string(gh.PEM)}},
		})
		Expect(err).NotTo(HaveOccurred())
		var orgID, id pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		Expect(id.Scan(connID)).To(Succeed())
		row, err := store.New(e.pool).GetConnection(ctx, store.GetConnectionParams{OrgID: orgID, ID: id})
		Expect(err).NotTo(HaveOccurred())
		fields, err := connections.OpenFields(ring, row)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fields[github.ClientSecretField])).To(Equal(githubtest.FakeClientSecret))
		Expect(fields).To(HaveKey(connections.GitHubPrivateKeyField))
	})
})
