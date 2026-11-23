// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"net/url"
	"strconv"

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

// The guided GitHub App setup (issue #1175), end to end against the recorded GitHub of the fake:
// the browser posts Koinon's manifest, GitHub creates the App and sends a code, Koinon trades it
// for the App's key and OAuth client, the admin installs the App, and Koinon makes the connection.
var _ = Describe("guided GitHub App setup", func() {
	const publicURL = "https://koinon.example.test"
	var (
		e     *env
		gh    *githubtest.Fake
		ring  *secretstore.Keyring
		org   string
		owner *client
	)

	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		ring = secretstoretest.Keyring()
		e = newEnv(ctx, envOptions{
			keys: ring, github: github.NewClient(github.Options{BaseURL: gh.URL()}), ghFake: gh,
			settings: func(s *apiassembly.Settings) { s.PublicURL = publicURL },
		})
		org, owner = e.newTeamOrg(ctx)
	})

	// query reads one parameter of a URL GitHub sends the browser to.
	query := func(raw, key string) string {
		GinkgoHelper()
		u, err := url.Parse(raw)
		Expect(err).NotTo(HaveOccurred())
		return u.Query().Get(key)
	}

	// created runs the setup up to the App GitHub created and returns the state and the install page.
	created := func(ctx SpecContext, c *client, code string) (string, string) {
		GinkgoHelper()
		start, err := c.conns.StartGitHubAppSetup(ctx, &connectionv1alpha1.StartGitHubAppSetupRequest{OrgId: org, DisplayName: "Acme agents"})
		Expect(err).NotTo(HaveOccurred())
		Expect(start.GetPostUrl()).To(HavePrefix(gh.WebURL() + "/settings/apps/new?state="))
		back, err := gh.CreateFromManifest(start.GetPostUrl(), start.GetManifest(), code)
		Expect(err).NotTo(HaveOccurred())
		Expect(back).To(HavePrefix(publicURL + "/connections/new/github/app-created?"))
		Expect(query(back, "state")).To(Equal(start.GetState()))
		cont, err := c.conns.ContinueGitHubAppSetup(ctx, &connectionv1alpha1.ContinueGitHubAppSetupRequest{
			OrgId: org, State: query(back, "state"), Code: query(back, "code"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(cont.GetAppSlug()).To(Equal(githubtest.FakeAppSlug))
		return start.GetState(), cont.GetInstallUrl()
	}

	It("creates the org's own App from a manifest and makes the connection", func(ctx SpecContext) {
		state, install := created(ctx, owner, "koinon-test-code-1")

		m, ok := gh.CreatedApp()
		Expect(ok).To(BeTrue())
		Expect(m.Name).To(Equal("Acme agents"))
		Expect(m.Public).To(BeFalse())
		Expect(m.HookAttributes).NotTo(BeNil())
		Expect(m.HookAttributes.Active).To(BeFalse(), "Koinon polls; the webhook is off")
		Expect(m.CallbackURLs).To(Equal([]string{publicURL + "/auth/github/link/callback"}))
		Expect(m.SetupURL).To(Equal(publicURL + "/connections/new/github/app-installed"))
		Expect(m.DefaultPermissions).To(HaveKeyWithValue("pull_requests", "write"))
		Expect(m.DefaultPermissions).NotTo(HaveKey("workflows"))

		// A reload of the page after the code was used still finds the install page.
		again, err := owner.conns.ContinueGitHubAppSetup(ctx, &connectionv1alpha1.ContinueGitHubAppSetupRequest{OrgId: org, State: state, Code: "koinon-test-code-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetInstallUrl()).To(Equal(install))

		back, err := gh.Install(install)
		Expect(err).NotTo(HaveOccurred())
		Expect(query(back, "state")).To(Equal(state))
		inst, err := strconv.ParseInt(query(back, "installation_id"), 10, 64)
		Expect(err).NotTo(HaveOccurred())

		other := e.userIn(ctx, org, store.OrgRoleAdmin)
		_, err = other.conns.FinishGitHubAppSetup(ctx, &connectionv1alpha1.FinishGitHubAppSetupRequest{OrgId: org, State: state, InstallationId: inst})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "another admin cannot finish the setup")
		_, err = owner.conns.FinishGitHubAppSetup(ctx, &connectionv1alpha1.FinishGitHubAppSetupRequest{OrgId: org, State: state, InstallationId: inst + 1})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "an installation of another App")

		res, err := owner.conns.FinishGitHubAppSetup(ctx, &connectionv1alpha1.FinishGitHubAppSetupRequest{OrgId: org, State: state, InstallationId: inst})
		Expect(err).NotTo(HaveOccurred())
		conn := res.GetConnection()
		Expect(conn.GetDisplayName()).To(Equal("Acme agents"))
		Expect(conn.GetGithub().GetAppId()).To(Equal(gh.AppID))
		Expect(conn.GetGithub().GetInstallationId()).To(Equal(inst))
		Expect(conn.GetGithub().GetClientId()).To(Equal(githubtest.FakeClientID))
		for _, r := range res.GetResults() {
			Expect(r.GetOk()).To(BeTrue(), "%s: %s", r.GetCheck(), r.GetMessage())
		}
		Expect(res.GetResults()).NotTo(BeEmpty())

		// The key and the client secret are in the connection's envelope, and only there.
		var orgID, id pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		Expect(id.Scan(conn.GetId())).To(Succeed())
		row, err := store.New(e.pool).GetConnection(ctx, store.GetConnectionParams{OrgID: orgID, ID: id})
		Expect(err).NotTo(HaveOccurred())
		fields, err := connections.OpenFields(ring, row)
		Expect(err).NotTo(HaveOccurred())
		Expect(fields[connections.GitHubPrivateKeyField]).To(Equal(gh.PEM))
		Expect(string(fields[github.ClientSecretField])).To(Equal(githubtest.FakeClientSecret))
		Expect(string(row.Config)).NotTo(ContainSubstring(githubtest.FakeClientSecret))

		var after []byte
		Expect(e.pool.QueryRow(ctx, `SELECT summary->'after' FROM audit_events WHERE org_id = $1 AND action = 'connection.create'`, org).Scan(&after)).To(Succeed())
		var a map[string]any
		Expect(json.Unmarshal(after, &a)).To(Succeed())
		Expect(a).To(HaveKeyWithValue("setup", "manifest"))
		Expect(a).To(HaveKeyWithValue("clientId", githubtest.FakeClientID))
		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'github_app.create'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND summary::text LIKE '%' || $2 || '%'`, org, githubtest.FakeClientSecret).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "no audit row holds the client secret")

		// The setup works once.
		_, err = owner.conns.FinishGitHubAppSetup(ctx, &connectionv1alpha1.FinishGitHubAppSetupRequest{OrgId: org, State: state, InstallationId: inst})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM github_app_setups WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("refuses a code GitHub does not know, a member and a finish before the App exists", func(ctx SpecContext) {
		start, err := owner.conns.StartGitHubAppSetup(ctx, &connectionv1alpha1.StartGitHubAppSetupRequest{OrgId: org, DisplayName: "Acme agents", GithubOwner: "acme"})
		Expect(err).NotTo(HaveOccurred())
		Expect(start.GetPostUrl()).To(HavePrefix(gh.WebURL() + "/organizations/acme/settings/apps/new?state="))

		_, err = owner.conns.FinishGitHubAppSetup(ctx, &connectionv1alpha1.FinishGitHubAppSetupRequest{OrgId: org, State: start.GetState(), InstallationId: gh.InstallationID})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		_, err = owner.conns.ContinueGitHubAppSetup(ctx, &connectionv1alpha1.ContinueGitHubAppSetupRequest{OrgId: org, State: start.GetState(), Code: "koinon-test-unknown"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = owner.conns.ContinueGitHubAppSetup(ctx, &connectionv1alpha1.ContinueGitHubAppSetupRequest{OrgId: org, State: "no-such-state", Code: "x"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err = member.conns.StartGitHubAppSetup(ctx, &connectionv1alpha1.StartGitHubAppSetupRequest{OrgId: org, DisplayName: "Acme agents"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		for _, bad := range []*connectionv1alpha1.StartGitHubAppSetupRequest{
			{OrgId: org},
			{OrgId: org, DisplayName: "a name that is longer than GitHub allows"},
			{OrgId: org, DisplayName: "ok", GithubOwner: "not/an-owner"},
		} {
			_, err = owner.conns.StartGitHubAppSetup(ctx, bad)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "%v", bad)
		}
	})

	It("needs a public URL", func(ctx SpecContext) {
		noURL := newEnv(ctx, envOptions{keys: ring, github: github.NewClient(github.Options{BaseURL: gh.URL()}), ghFake: gh})
		o, c := noURL.newTeamOrg(ctx)
		_, err := c.conns.StartGitHubAppSetup(ctx, &connectionv1alpha1.StartGitHubAppSetupRequest{OrgId: o, DisplayName: "Acme agents"})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})
})
