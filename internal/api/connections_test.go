// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	outwardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

// lockedBuffer collects logs from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// pemBody is the base64 body of a PEM block, the part that must never leak.
func pemBody(pem []byte) []string {
	var lines []string
	for l := range strings.Lines(string(pem)) {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "-----") {
			lines = append(lines, l)
		}
	}
	return lines
}

var _ = Describe("ConnectionService", func() {
	var (
		e     *env
		gh    *githubtest.Fake
		kube  *fake.Clientset
		ring  *secretstore.Keyring
		logs  *lockedBuffer
		org   string
		owner *client
	)

	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		kube = fake.NewClientset()
		ring = secretstoretest.Keyring()
		logs = &lockedBuffer{}
		e = newEnv(ctx, envOptions{
			keys:   ring,
			github: github.NewClient(github.Options{BaseURL: gh.URL()}),
			ghFake: gh,
			logs:   logs,
		})
		org, owner = e.newTeamOrg(ctx)
	})

	create := func(ctx SpecContext, c *client, key []byte, test bool) (*connectionv1alpha1.CreateGitHubConnectionResponse, error) {
		return c.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "acme", AppId: gh.AppID, InstallationId: gh.InstallationID,
			PrivateKey: string(key), Test: test,
		})
	}

	It("lists the GitHub connector type with its setup guide", func(ctx SpecContext) {
		res, err := owner.conns.ListConnectorTypes(ctx, &connectionv1alpha1.ListConnectorTypesRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnectorTypes()).To(HaveLen(2))
		Expect(res.GetConnectorTypes()[0].GetId()).To(Equal("gitea"))
		Expect(res.GetConnectorTypes()[0].GetAuthType()).To(Equal("static-token"))
		gh := res.GetConnectorTypes()[1]
		Expect(gh.GetId()).To(Equal("github"))
		Expect(gh.GetAuthType()).To(Equal("app-credential"))
		Expect(gh.GetScopes()).To(ContainElement(SatisfyAll(
			HaveField("Id", "pull_request.create"),
			HaveField("Tier", connectionv1alpha1.Tier_TIER_REVERSIBLE_WRITE),
		)))
		guide := gh.GetDocument().GetFields()["spec"].GetStructValue().GetFields()["setupGuide"].GetStructValue()
		Expect(guide.GetFields()["steps"].GetListValue().GetValues()).NotTo(BeEmpty())
	})

	It("stores the key only as an envelope in Postgres and never returns or logs it", func(ctx SpecContext) {
		res, err := create(ctx, owner, gh.PEM, true)
		Expect(err).NotTo(HaveOccurred())
		conn := res.GetConnection()
		Expect(conn.GetGithub().GetAppId()).To(Equal(gh.AppID))
		Expect(conn.GetGithub().GetKeyFingerprint()).To(HavePrefix("SHA256:"))
		Expect(conn.GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_OK))
		Expect(res.GetResults()).To(HaveLen(3))
		for _, r := range res.GetResults() {
			Expect(r.GetOk()).To(BeTrue(), "%s: %s", r.GetCheck(), r.GetMessage())
		}

		By("the row holds the key as an envelope that opens only with the keyring, and no Kubernetes Secret is made")
		secrets, err := kube.CoreV1().Secrets("koinon").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets.Items).To(BeEmpty())
		var stored store.Connection
		stored, err = store.New(e.pool).GetConnection(ctx, store.GetConnectionParams{OrgID: mustUUID(org), ID: mustUUID(conn.GetId())})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.SecretName.Valid).To(BeFalse())
		fields, err := connections.OpenFields(ring, stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(fields[connections.GitHubPrivateKeyField]).To(Equal(gh.PEM))
		Expect(string(stored.SecretEnvelope)).NotTo(ContainSubstring("PRIVATE KEY"))

		By("the creation is audited without the key")
		var summary string
		Expect(e.pool.QueryRow(ctx, `SELECT summary::text FROM audit_events WHERE org_id = $1 AND action = 'connection.create'`, org).Scan(&summary)).To(Succeed())
		Expect(summary).To(ContainSubstring("keyFingerprint"))
		Expect(summary).NotTo(ContainSubstring("PRIVATE KEY"))

		By("no response, row or log line carries it")
		got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: conn.GetId()})
		Expect(err).NotTo(HaveOccurred())
		list, err := owner.conns.ListConnections(ctx, &connectionv1alpha1.ListConnectionsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		tested, err := owner.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: conn.GetId()})
		Expect(err).NotTo(HaveOccurred())
		var row string
		Expect(e.pool.QueryRow(ctx, `SELECT row_to_json(c)::text FROM connections c`).Scan(&row)).To(Succeed())
		var seen []string
		for _, b := range [][]byte{mustJSON(res), mustJSON(got), mustJSON(list), mustJSON(tested)} {
			seen = append(seen, string(b))
		}
		seen = append(seen, row, logs.String())
		for _, s := range seen {
			for _, line := range pemBody(gh.PEM) {
				Expect(s).NotTo(ContainSubstring(line))
			}
			Expect(s).NotTo(ContainSubstring("PRIVATE KEY"))
			Expect(s).NotTo(ContainSubstring("ghs_"), "installation tokens are not leaked either")
		}
		Expect(logs.String()).To(ContainSubstring("connection created"))
	})

	It("tests a switched-off connection without recording a failing status (ADR 0110)", func(ctx SpecContext) {
		conn, err := create(ctx, owner, gh.PEM, false)
		Expect(err).NotTo(HaveOccurred())
		id := conn.GetConnection().GetId()
		_, err = owner.outward.SetOutwardTargetDisabled(ctx, &outwardv1alpha1.SetOutwardTargetDisabledRequest{OrgId: org, Kind: outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_CONNECTION, TargetId: id, Disabled: true})
		Expect(err).NotTo(HaveOccurred())
		var before string
		Expect(e.pool.QueryRow(ctx, `SELECT status::text FROM connections WHERE id = $1`, id).Scan(&before)).To(Succeed())
		tested, err := owner.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: id})
		Expect(err).NotTo(HaveOccurred())
		Expect(mustJSON(tested)).To(ContainSubstring("disabled by an admin"))
		var after string
		Expect(e.pool.QueryRow(ctx, `SELECT status::text FROM connections WHERE id = $1`, id).Scan(&after)).To(Succeed())
		Expect(after).To(Equal(before))
	})

	It("rejects a bad key without quoting it, and writes nothing", func(ctx SpecContext) {
		_, err := create(ctx, owner, []byte("-----BEGIN RSA PRIVATE KEY-----\nTOPSECRETSTUFF\n-----END RSA PRIVATE KEY-----\n"), false)
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).NotTo(ContainSubstring("TOPSECRETSTUFF"))
		Expect(logs.String()).NotTo(ContainSubstring("TOPSECRETSTUFF"))
		secrets, err := kube.CoreV1().Secrets("koinon").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets.Items).To(BeEmpty())
	})

	It("stores a base URL and CA bundle with the connection and returns them", func(ctx SpecContext) {
		tlsServer := httptest.NewTLSServer(http.NotFoundHandler()) // a certificate to use as the CA
		DeferCleanup(tlsServer.Close)
		ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}))
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "ghes", AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
			BaseUrl: " https://GHES.example.test:8443/ ", CaBundle: ca,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnection().GetGithub().GetBaseUrl()).To(Equal("https://ghes.example.test:8443"))
		Expect(res.GetConnection().GetGithub().GetCaBundle()).To(Equal(strings.TrimSpace(ca)))
		got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: res.GetConnection().GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetConnection().GetGithub().GetBaseUrl()).To(Equal("https://ghes.example.test:8443"))

		// github.com is the default and is not stored.
		res, err = owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM), BaseUrl: "https://github.com",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnection().GetGithub().GetBaseUrl()).To(BeEmpty())
	})

	It("stores the connection's CI directories cleaned and returns them", func(ctx SpecContext) {
		res, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM),
			CiDirs: []string{"./ci//pipelines/", " build/ci ", "ci/pipelines", ""},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnection().GetGithub().GetCiDirs()).To(Equal([]string{"ci/pipelines", "build/ci"}))
		got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: res.GetConnection().GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetConnection().GetGithub().GetCiDirs()).To(Equal([]string{"ci/pipelines", "build/ci"}))

		for _, bad := range [][]string{{"/abs"}, {"../up"}, {"."}, {"a\\b"}, make([]string, 21)} {
			_, err := owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
				OrgId: org, AppId: gh.AppID, InstallationId: gh.InstallationID, PrivateKey: string(gh.PEM), CiDirs: bad,
			})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "%v", bad)
		}
	})

	It("rejects a base URL that is not a bare https host and a CA bundle that is not certificates", func(ctx SpecContext) {
		for _, req := range []*connectionv1alpha1.CreateGitHubConnectionRequest{
			{BaseUrl: "http://ghes.example.test"},
			{BaseUrl: "https://ghes.example.test/enterprise"},
			{BaseUrl: "https://10.0.0.1"},
			{BaseUrl: "https://127.0.0.1:8443"},
			{BaseUrl: "https://koinon-postgres"},
			{CaBundle: "not pem"},
			{CaBundle: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-a-real-key")}))},
		} {
			req.OrgId, req.AppId, req.InstallationId, req.PrivateKey = org, gh.AppID, gh.InstallationID, string(gh.PEM)
			_, err := owner.conns.CreateGitHubConnection(ctx, req)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "%v", req)
		}
		Expect(count(ctx, e, `SELECT count(*) FROM connections`)).To(BeZero())
	})

	It("reports failing checks at the step that caused them", func(ctx SpecContext) {
		gh.Permissions["workflows"] = "write"
		res, err := create(ctx, owner, gh.PEM, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnection().GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_FAILING))
		Expect(res.GetResults()).To(HaveExactElements(
			SatisfyAll(HaveField("Check", "github.app"), HaveField("Ok", true)),
			SatisfyAll(HaveField("Check", "github.installation"), HaveField("Step", "install-app"), HaveField("Ok", false),
				HaveField("Message", ContainSubstring("remove the workflows permission"))),
			SatisfyAll(HaveField("Check", "github.token"), HaveField("Ok", false), HaveField("Message", ContainSubstring("skipped"))),
		))
		Expect(count(ctx, e, `SELECT count(*) FROM connections WHERE status = 'failing'`)).To(Equal(1))
	})

	It("needs an admin to create or test, and hides other orgs", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := create(ctx, member, gh.PEM, false)
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		res, err := create(ctx, owner, gh.PEM, false)
		Expect(err).NotTo(HaveOccurred())
		_, err = member.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: res.GetConnection().GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: res.GetConnection().GetId()})
		Expect(err).NotTo(HaveOccurred(), "members may read connections")

		other, otherOwner := e.newTeamOrg(ctx)
		_, err = otherOwner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: other, ConnectionId: res.GetConnection().GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})

	Describe("ListInstallationRepos", func() {
		var connID string

		BeforeEach(func(ctx SpecContext) {
			gh.Repositories = nil
			for i := range 250 {
				gh.Repositories = append(gh.Repositories, fmt.Sprintf("Repo-%03d", i))
			}
			gh.Private = map[string]bool{"Repo-007": true}
			res, err := create(ctx, owner, gh.PEM, false)
			Expect(err).NotTo(HaveOccurred())
			connID = res.GetConnection().GetId()
		})

		list := func(ctx SpecContext, c *client, token, query string) (*connectionv1alpha1.ListInstallationReposResponse, error) {
			return c.conns.ListInstallationRepos(ctx, &connectionv1alpha1.ListInstallationReposRequest{
				OrgId: org, ConnectionId: connID, PageToken: token, Query: query,
			})
		}

		It("pages through GitHub's list with a metadata-read token", func(ctx SpecContext) {
			var names []string
			token := ""
			for range 3 {
				res, err := list(ctx, owner, token, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(res.GetTotalCount()).To(BeEquivalentTo(250))
				for _, r := range res.GetRepos() {
					names = append(names, r.GetFullName())
					Expect(r.GetDefaultBranch()).To(Equal("main"))
					Expect(r.GetPrivate()).To(Equal(r.GetFullName() == "acme/Repo-007"))
				}
				token = res.GetNextPageToken()
			}
			Expect(token).To(BeEmpty(), "the third page is the last")
			Expect(names).To(HaveLen(250))
			Expect(names[0]).To(Equal("acme/Repo-000"), "GitHub's spelling, owner included")
			Expect(names[249]).To(Equal("acme/Repo-249"))
			Expect(gh.ListCalls()).To(Equal(3))
			for _, r := range gh.Requests() {
				Expect(r).To(Equal(githubtest.Request{Permissions: map[string]string{"metadata": "read"}}))
			}
		})

		It("filters case-insensitively across GitHub pages", func(ctx SpecContext) {
			res, err := list(ctx, owner, "", "  REPO-2  ")
			Expect(err).NotTo(HaveOccurred())
			// Repo-200..249, all on GitHub's third page.
			Expect(res.GetRepos()).To(HaveLen(50))
			Expect(res.GetRepos()[0].GetFullName()).To(Equal("acme/Repo-200"))
			Expect(res.GetNextPageToken()).To(BeEmpty())
			Expect(gh.ListCalls()).To(Equal(3))

			res, err = list(ctx, owner, "", "ACME/REPO-24")
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetRepos()).To(HaveLen(10))

			res, err = list(ctx, owner, "", "nothing-matches")
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetRepos()).To(BeEmpty())
			Expect(res.GetNextPageToken()).To(BeEmpty())
		})

		It("stops a filtered call after a page's worth of matches and continues from the token", func(ctx SpecContext) {
			res, err := list(ctx, owner, "", "acme/")
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetRepos()).To(HaveLen(100))
			Expect(res.GetNextPageToken()).To(Equal("2"))
			res, err = list(ctx, owner, res.GetNextPageToken(), "acme/")
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetRepos()[0].GetFullName()).To(Equal("acme/Repo-100"))
		})

		It("refuses bad tokens and queries, members and other orgs", func(ctx SpecContext) {
			_, err := list(ctx, owner, "two", "")
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			_, err = list(ctx, owner, "0", "")
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			_, err = list(ctx, owner, "", strings.Repeat("x", 201))
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

			for _, role := range []store.OrgRole{store.OrgRoleMember, store.OrgRoleViewer} {
				_, err = list(ctx, e.userIn(ctx, org, role), "", "")
				Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), string(role))
			}
			_, err = list(ctx, e.userIn(ctx, org, store.OrgRoleAdmin), "", "")
			Expect(err).NotTo(HaveOccurred(), "admins may list")

			other, otherOwner := e.newTeamOrg(ctx)
			_, err = otherOwner.conns.ListInstallationRepos(ctx, &connectionv1alpha1.ListInstallationReposRequest{OrgId: other, ConnectionId: connID})
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "another org's connection is not found")
			_, err = otherOwner.conns.ListInstallationRepos(ctx, &connectionv1alpha1.ListInstallationReposRequest{OrgId: org, ConnectionId: connID})
			Expect(codeOf(err)).To(BeElementOf(connect.CodeNotFound, connect.CodePermissionDenied), "another org is denied")
			Expect(gh.ListCalls()).To(Equal(1), "only the admin's call reached GitHub")
		})

		It("reports GitHub refusing the connection", func(ctx SpecContext) {
			gh.InstallationID++
			_, err := list(ctx, owner, "", "")
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("GitHub refused the connection"))
		})
	})

	It("deletes a connection entered here once no agent is bound to it, and audits it", func(ctx SpecContext) {
		// The fixture connection is the one draftAgent binds.
		id := e.githubConnection(ctx, org)
		agent := owner.draftAgent(ctx, org)
		del := func(c *client) error {
			_, err := c.conns.DeleteConnection(ctx, &connectionv1alpha1.DeleteConnectionRequest{OrgId: org, ConnectionId: id})
			return err
		}
		Expect(codeOf(del(e.userIn(ctx, org, store.OrgRoleMember)))).To(Equal(connect.CodePermissionDenied))

		err := del(owner)
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring(agent.GetId()), "the bound agents are listed")
		var ce *connect.Error
		Expect(errors.As(err, &ce)).To(BeTrue())
		Expect(ce.Details()).To(HaveLen(1))
		msg, uerr := connectproto.UnmarshalErrorDetail(ce.Details()[0])
		Expect(uerr).NotTo(HaveOccurred())
		bound, ok := msg.(*connectionv1alpha1.BoundAgents)
		Expect(ok).To(BeTrue())
		Expect(bound.GetAgents()).To(HaveLen(1))
		Expect(bound.GetAgents()[0].GetId()).To(Equal(agent.GetId()))
		Expect(bound.GetAgents()[0].GetDisplayName()).NotTo(BeEmpty())

		_, err = owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(del(owner)).To(Succeed())
		_, err = owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: id})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'connection.delete'`, org)).To(Equal(1))
		Expect(count(ctx, e, `SELECT count(*) FROM connections WHERE org_id = $1`, org)).To(BeZero(), "the envelope went with the row")
	})

	It("audits the unlinking of every MCP server linked to a connection it deletes (ADR 0091, old issue 350)", func(ctx SpecContext) {
		id := e.githubConnection(ctx, org)
		for _, name := range []string{"tracker-a", "tracker-b"} {
			_, err := e.pool.Exec(ctx, `INSERT INTO mcp_servers (org_id, name, url, code_host_connection_id) VALUES ($1, $2, 'https://mcp.example.com/'||$2, $3)`, org, name, id)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := owner.conns.DeleteConnection(ctx, &connectionv1alpha1.DeleteConnectionRequest{OrgId: org, ConnectionId: id})
		Expect(err).NotTo(HaveOccurred())
		Expect(count(ctx, e, `SELECT count(*) FROM mcp_servers WHERE org_id = $1 AND code_host_connection_id IS NULL`, org)).To(Equal(2), "both servers are unlinked")
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'mcp_server.data_class.reset' AND summary->'after'->>'reason' = 'connection_deleted' AND summary->'before'->>'codeHostConnectionId' = $2`, org, id)).To(Equal(2))
	})

	It("takes the connection's lock before its servers', as the link check does, so the two cannot deadlock (#117)", func(ctx SpecContext) {
		id := e.githubConnection(ctx, org)
		var srv string
		Expect(e.pool.QueryRow(ctx, `INSERT INTO mcp_servers (org_id, name, url, code_host_connection_id) VALUES ($1, 'tracker', 'https://api.githubcopilot.com/mcp/', $2) RETURNING id::text`, org, id).Scan(&srv)).To(Succeed())

		By("a link check holds the connection (FOR SHARE) and is about to lock the server")
		tx, err := e.pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
		_, err = tx.Exec(ctx, `SELECT 1 FROM connections WHERE org_id = $1 AND id = $2 FOR SHARE`, org, id)
		Expect(err).NotTo(HaveOccurred())

		deleted := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, err := owner.conns.DeleteConnection(ctx, &connectionv1alpha1.DeleteConnectionRequest{OrgId: org, ConnectionId: id})
			deleted <- err
		}()
		Eventually(func() int {
			return count(ctx, e, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`)
		}).WithTimeout(10*time.Second).Should(Equal(1), "the delete waits on a lock")

		By("the link check locks the server: the delete must not hold it")
		_, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.Exec(ctx, `SELECT 1 FROM mcp_servers WHERE org_id = $1 AND id = $2 FOR UPDATE`, org, srv)
		Expect(err).NotTo(HaveOccurred(), "no deadlock: the delete waits for the connection first")
		Expect(tx.Commit(ctx)).To(Succeed())
		Eventually(deleted).WithTimeout(10 * time.Second).Should(Receive(BeNil()))
		Expect(count(ctx, e, `SELECT count(*) FROM mcp_servers WHERE org_id = $1 AND code_host_connection_id IS NULL`, org)).To(Equal(1))
	})

	Describe("the extra MCP hosts of a connection (#240)", func() {
		var id string
		add := func(c *client, host string) (*connectionv1alpha1.Connection, error) {
			res, err := c.conns.AddConnectionMCPHost(context.Background(), &connectionv1alpha1.AddConnectionMCPHostRequest{OrgId: org, ConnectionId: id, Host: host})
			return res.GetConnection(), err
		}
		remove := func(c *client, host string) (*connectionv1alpha1.Connection, error) {
			res, err := c.conns.RemoveConnectionMCPHost(context.Background(), &connectionv1alpha1.RemoveConnectionMCPHostRequest{OrgId: org, ConnectionId: id, Host: host})
			return res.GetConnection(), err
		}
		// audited counts the events of action written by a user (the actor is recorded).
		audited := func(ctx SpecContext, action string) int {
			return count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2 AND actor_kind = 'user' AND actor_id <> ''`, org, action)
		}
		BeforeEach(func(ctx SpecContext) {
			id = e.githubConnection(ctx, org)
		})

		It("adds and removes hosts for an admin, lists them sorted, and audits each change with its actor", func(ctx SpecContext) {
			conn, err := add(owner, " Zeta-MCP.Corp.Example. ")
			Expect(err).NotTo(HaveOccurred())
			Expect(conn.GetMcpHosts()).To(Equal([]string{"zeta-mcp.corp.example"}))
			conn, err = add(owner, "alpha-mcp")
			Expect(err).NotTo(HaveOccurred())
			Expect(conn.GetMcpHosts()).To(Equal([]string{"alpha-mcp", "zeta-mcp.corp.example"}))
			got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: id})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetConnection().GetMcpHosts()).To(Equal([]string{"alpha-mcp", "zeta-mcp.corp.example"}))
			Expect(audited(ctx, "connection.mcp_host.add")).To(Equal(2))

			conn, err = remove(owner, "alpha-mcp")
			Expect(err).NotTo(HaveOccurred())
			Expect(conn.GetMcpHosts()).To(Equal([]string{"zeta-mcp.corp.example"}))
			Expect(audited(ctx, "connection.mcp_host.remove")).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'connection.mcp_host.add' AND summary->'after'->>'host' = 'zeta-mcp.corp.example'`, org)).To(Equal(1))
		})

		It("changes and audits nothing when a host is already there or already absent", func(ctx SpecContext) {
			_, err := add(owner, "gitea-mcp")
			Expect(err).NotTo(HaveOccurred())
			_, err = add(owner, "GITEA-MCP")
			Expect(err).NotTo(HaveOccurred())
			_, err = remove(owner, "other-mcp")
			Expect(err).NotTo(HaveOccurred())
			Expect(audited(ctx, "connection.mcp_host.add")).To(Equal(1))
			Expect(audited(ctx, "connection.mcp_host.remove")).To(BeZero())
		})

		It("needs an admin, and hides other orgs' connections", func(ctx SpecContext) {
			for _, role := range []store.OrgRole{store.OrgRoleMember, store.OrgRoleViewer} {
				member := e.userIn(ctx, org, role)
				_, err := add(member, "gitea-mcp")
				Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
			}
			_, err := add(owner, "gitea-mcp")
			Expect(err).NotTo(HaveOccurred())
			_, err = remove(e.userIn(ctx, org, store.OrgRoleMember), "gitea-mcp")
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
			_, err = add(owner, "second-mcp")
			Expect(err).NotTo(HaveOccurred())
			Expect(count(ctx, e, `SELECT cardinality(mcp_hosts) FROM connections WHERE org_id = $1 AND id = $2`, org, id)).To(Equal(2))

			otherOrg, otherOwner := e.newTeamOrg(ctx)
			_, err = otherOwner.conns.AddConnectionMCPHost(ctx, &connectionv1alpha1.AddConnectionMCPHostRequest{OrgId: otherOrg, ConnectionId: id, Host: "x-mcp"})
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		})

		DescribeTable("refuses a host that is not a plain host name, and writes nothing",
			func(ctx SpecContext, host string) {
				_, err := add(owner, host)
				Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(audited(ctx, "connection.mcp_host.add")).To(BeZero())
				Expect(count(ctx, e, `SELECT cardinality(mcp_hosts) FROM connections WHERE org_id = $1 AND id = $2`, org, id)).To(BeZero())
			},
			Entry("empty", ""),
			Entry("with a port", "mcp.example.com:8443"),
			Entry("a URL", "https://mcp.example.com/mcp"),
			Entry("a wildcard", "*.example.com"),
			Entry("an IP address", "10.0.0.5"),
		)

		It("allows at most 20 hosts", func(ctx SpecContext) {
			for i := range 20 {
				_, err := add(owner, fmt.Sprintf("mcp-%02d.example.com", i))
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := add(owner, "one-too-many.example.com")
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			_, err = add(owner, "mcp-00.example.com")
			Expect(err).NotTo(HaveOccurred(), "a present host is still a no-op at the limit")
		})

		It("refuses to change the hosts of a removed connection, and writes nothing", func(ctx SpecContext) {
			_, err := add(owner, "gitea-mcp")
			Expect(err).NotTo(HaveOccurred())
			_, err = e.pool.Exec(ctx, `UPDATE connections SET removed_at = now() WHERE org_id = $1 AND id = $2`, org, id)
			Expect(err).NotTo(HaveOccurred())
			_, err = add(owner, "second-mcp")
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			_, err = remove(owner, "gitea-mcp")
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(count(ctx, e, `SELECT cardinality(mcp_hosts) FROM connections WHERE org_id = $1 AND id = $2`, org, id)).To(Equal(1))
		})

		It("unlinks, audited, the servers that lose their host on removal, and keeps the others (the link check uses the list)", func(ctx SpecContext) {
			_, err := add(owner, "gitea-mcp.corp.example")
			Expect(err).NotTo(HaveOccurred())
			// One server on the extra host, one on a host the connector declares for github.com.
			_, err = e.pool.Exec(ctx, `INSERT INTO mcp_servers (org_id, name, url, code_host_connection_id) VALUES
				($1, 'extra', 'https://gitea-mcp.corp.example/mcp', $2), ($1, 'declared', 'https://api.githubcopilot.com/mcp/', $2)`, org, id)
			Expect(err).NotTo(HaveOccurred())

			_, err = remove(owner, "gitea-mcp.corp.example")
			Expect(err).NotTo(HaveOccurred())
			Expect(count(ctx, e, `SELECT count(*) FROM mcp_servers WHERE org_id = $1 AND name = 'extra' AND code_host_connection_id IS NULL`, org)).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM mcp_servers WHERE org_id = $1 AND name = 'declared' AND code_host_connection_id IS NOT NULL`, org)).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'mcp_server.data_class.reset' AND summary->'after'->>'reason' = 'connection_mcp_host_removed'`, org)).To(Equal(1))
		})
	})

	It("deletes a declared connection only after its Secret was deleted", func(ctx SpecContext) {
		res, err := create(ctx, owner, gh.PEM, false)
		Expect(err).NotTo(HaveOccurred())
		id := res.GetConnection().GetId()
		// What the importer does for a declared connection.
		_, err = e.pool.Exec(ctx, `UPDATE connections SET source = 'secret', managed_by_secret = 'gh' WHERE id = $1`, id)
		Expect(err).NotTo(HaveOccurred())
		del := func() error {
			_, err := owner.conns.DeleteConnection(ctx, &connectionv1alpha1.DeleteConnectionRequest{OrgId: org, ConnectionId: id})
			return err
		}
		err = del()
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("Secret gh"))

		_, err = e.pool.Exec(ctx, `UPDATE connections SET removed_at = now() WHERE id = $1`, id)
		Expect(err).NotTo(HaveOccurred())
		got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: id})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetConnection().GetRemoved()).To(BeTrue())
		Expect(got.GetConnection().GetManagedBySecret()).To(Equal("gh"))
		Expect(del()).To(Succeed())
	})

	Describe("SetConnectionSecret", func() {
		var id string
		// What keys forget leaves behind (ADR 0085 decision 13): no envelope, and the mark.
		lose := func(ctx SpecContext) {
			_, err := e.pool.Exec(ctx, `UPDATE connections SET secret_envelope = NULL, credential_lost_at = now() WHERE id = $1`, id)
			Expect(err).NotTo(HaveOccurred())
		}
		set := func(ctx SpecContext, c *client, org string, key []byte, test bool) (*connectionv1alpha1.SetConnectionSecretResponse, error) {
			return c.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
				OrgId: org, ConnectionId: id, Test: test,
				Secret: &connectionv1alpha1.SetConnectionSecretRequest_Github{Github: &connectionv1alpha1.GitHubConnectionSecret{PrivateKey: string(key)}},
			})
		}
		BeforeEach(func(ctx SpecContext) {
			res, err := create(ctx, owner, gh.PEM, true)
			Expect(err).NotTo(HaveOccurred())
			id = res.GetConnection().GetId()
		})

		It("enters a lost private key again: the mark ends, the connection works, and nothing quotes the key", func(ctx SpecContext) {
			lose(ctx)
			got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: id})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetConnection().GetCredentialLostAt()).NotTo(BeNil())
			tested, err := owner.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: id})
			Expect(err).NotTo(HaveOccurred())
			Expect(tested.GetResults()).To(HaveExactElements(HaveField("Message",
				ContainSubstring(`was lost with a key-encryption key; enter the secret of the connection "acme" again on the Connections page`))))

			res, err := set(ctx, owner, org, gh.PEM, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetConnection().GetCredentialLostAt()).To(BeNil())
			Expect(res.GetConnection().GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_OK))
			Expect(res.GetResults()).To(HaveLen(3))
			for _, r := range res.GetResults() {
				Expect(r.GetOk()).To(BeTrue(), "%s: %s", r.GetCheck(), r.GetMessage())
			}
			Expect(res.GetConnection().GetGithub().GetAppId()).To(Equal(gh.AppID), "the settings stay")
			Expect(res.GetConnection().GetGithub().GetKeyFingerprint()).To(HavePrefix("SHA256:"))

			stored, err := store.New(e.pool).GetConnection(ctx, store.GetConnectionParams{OrgID: mustUUID(org), ID: mustUUID(id)})
			Expect(err).NotTo(HaveOccurred())
			fields, err := connections.OpenFields(ring, stored)
			Expect(err).NotTo(HaveOccurred())
			Expect(fields[connections.GitHubPrivateKeyField]).To(Equal(gh.PEM))

			var summary string
			Expect(e.pool.QueryRow(ctx, `SELECT summary::text FROM audit_events WHERE org_id = $1 AND action = 'connection.secret.set'`, org).Scan(&summary)).To(Succeed())
			Expect(summary).To(ContainSubstring(`"lost": true`))
			for _, s := range []string{string(mustJSON(res)), summary, logs.String()} {
				for _, line := range pemBody(gh.PEM) {
					Expect(s).NotTo(ContainSubstring(line))
				}
				Expect(s).NotTo(ContainSubstring("PRIVATE KEY"))
			}
			Expect(logs.String()).To(ContainSubstring("connection secret set"))
		})

		It("leaves the connection untested when it is not asked to test", func(ctx SpecContext) {
			res, err := set(ctx, owner, org, gh.PEM, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetConnection().GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_UNTESTED))
			Expect(res.GetConnection().GetLastTest()).To(BeEmpty(), "the old result was about the old secret")
			Expect(res.GetConnection().GetLastTestedAt()).To(BeNil())
		})

		It("answers with the stored connection and a failed check when the test after storing cannot be recorded", func(ctx SpecContext) {
			_, err := e.pool.Exec(ctx, `
CREATE FUNCTION koinon_test_refuse_test_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'test record refused'; END $$;
CREATE TRIGGER koinon_test_refuse_test_record BEFORE UPDATE ON connections
FOR EACH ROW WHEN (NEW.last_tested_at IS NOT NULL) EXECUTE FUNCTION koinon_test_refuse_test_record()`)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) {
				_, err := e.pool.Exec(ctx, `DROP TRIGGER koinon_test_refuse_test_record ON connections; DROP FUNCTION koinon_test_refuse_test_record()`)
				Expect(err).NotTo(HaveOccurred())
			})

			res, err := set(ctx, owner, org, gh.PEM, true)
			Expect(err).NotTo(HaveOccurred(), "the secret is stored, so the call succeeds")
			Expect(res.GetConnection().GetId()).To(Equal(id))
			Expect(res.GetResults()).To(HaveExactElements(And(HaveField("Ok", BeFalse()), HaveField("Message", ContainSubstring("test the connection again")))))
			stored, err := store.New(e.pool).GetConnection(ctx, store.GetConnectionParams{OrgID: mustUUID(org), ID: mustUUID(id)})
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.SecretFingerprint).To(HavePrefix("SHA256:"))
		})

		It("refuses a bad key, another connector's secret, no secret, a declared connection, members and other orgs, and changes nothing", func(ctx SpecContext) {
			lose(ctx)
			_, err := set(ctx, owner, org, []byte("-----BEGIN RSA PRIVATE KEY-----\nTOPSECRETSTUFF\n-----END RSA PRIVATE KEY-----\n"), false)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).NotTo(ContainSubstring("TOPSECRETSTUFF"))
			Expect(logs.String()).NotTo(ContainSubstring("TOPSECRETSTUFF"))

			_, err = owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
				OrgId: org, ConnectionId: id,
				Secret: &connectionv1alpha1.SetConnectionSecretRequest_Gitea{Gitea: &connectionv1alpha1.GiteaConnectionSecret{
					ReadToken: "koinon_test_not-a-real-read-token", WriteToken: "koinon_test_not-a-real-write-token",
				}},
			})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("this is a github connection"))
			_, err = owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{OrgId: org, ConnectionId: id})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

			member := e.userIn(ctx, org, store.OrgRoleMember)
			_, err = set(ctx, member, org, gh.PEM, false)
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
			other, otherOwner := e.newTeamOrg(ctx)
			_, err = set(ctx, otherOwner, other, gh.PEM, false)
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

			_, err = e.pool.Exec(ctx, `UPDATE connections SET source = 'secret', managed_by_secret = 'gh' WHERE id = $1`, id)
			Expect(err).NotTo(HaveOccurred())
			_, err = set(ctx, owner, org, gh.PEM, false)
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("Secret gh"))

			Expect(count(ctx, e, `SELECT count(*) FROM connections WHERE credential_lost_at IS NOT NULL AND secret_envelope IS NULL`)).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE action = 'connection.secret.set'`)).To(BeZero())
		})
	})

	It("refuses to create connections without a secret store", func(ctx SpecContext) {
		e2 := newEnvOn(ctx, e, envOptions{noKeys: true})
		_, err := create(ctx, owner.on(e2), gh.PEM, false)
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})
})

func mustJSON(m proto.Message) []byte {
	b, err := protojson.Marshal(m)
	Expect(err).NotTo(HaveOccurred())
	return b
}

var _ = Describe("ConnectionService for Gitea", func() {
	const (
		readTok  = "koinon_test_not-a-real-read-token"
		writeTok = "koinon_test_not-a-real-write-token"
	)
	var (
		e     *env
		gt    *fakegitea.Fake
		ring  *secretstore.Keyring
		logs  *lockedBuffer
		org   string
		owner *client
	)

	BeforeEach(func(ctx SpecContext) {
		gt = fakegitea.NewTLS()
		DeferCleanup(gt.Close)
		gt.Tokens = map[string]string{}
		gt.AddScopedToken(readTok, "koinon-bot", "read")
		gt.AddScopedToken(writeTok, "koinon-bot", "write")
		gt.AddRepo("app")
		gt.AddRepo("secret")
		gt.Collaborate("koinon-bot", "app", "write")
		ring = secretstoretest.Keyring()
		logs = &lockedBuffer{}
		u, err := url.Parse(gt.URL())
		Expect(err).NotTo(HaveOccurred())
		e = newEnv(ctx, envOptions{keys: ring, logs: logs, allowedHosts: []string{u.Hostname()}})
		org, owner = e.newTeamOrg(ctx)
	})

	create := func(ctx SpecContext, c *client, mutate func(*connectionv1alpha1.CreateGiteaConnectionRequest)) (*connectionv1alpha1.CreateGiteaConnectionResponse, error) {
		req := &connectionv1alpha1.CreateGiteaConnectionRequest{
			OrgId: org, DisplayName: "lab", BaseUrl: gt.URL(), CaBundle: gt.CAPEM(), ReadToken: readTok, WriteToken: writeTok, Test: true,
		}
		if mutate != nil {
			mutate(req)
		}
		return c.conns.CreateGiteaConnection(ctx, req)
	}

	It("creates a connection, tests it and shows what the bot can see", func(ctx SpecContext) {
		res, err := create(ctx, owner, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnection().GetConnector()).To(Equal("gitea"))
		Expect(res.GetConnection().GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_OK))
		Expect(res.GetResults()).To(HaveLen(4))
		for _, r := range res.GetResults() {
			Expect(r.GetOk()).To(BeTrue(), "%s: %s", r.GetCheck(), r.GetMessage())
		}
		Expect(res.GetResults()[2].GetMessage()).To(ContainSubstring("acme/app (write)"))
		Expect(res.GetResults()[2].GetMessage()).NotTo(ContainSubstring("secret"), "the bot is not a collaborator there")
		g := res.GetConnection().GetGitea()
		Expect(g.GetBaseUrl()).To(Equal(gt.URL()))
		Expect(g.GetCaBundle()).To(Equal(strings.TrimSpace(gt.CAPEM())))
		Expect(g.GetReadTokenSetAt().AsTime()).To(BeTemporally("~", time.Now(), time.Minute))
		Expect(g.GetWriteTokenSetAt()).NotTo(BeNil())
		Expect(g.GetTokenWarnAfterSeconds()).To(BeEquivalentTo(90 * 24 * 3600))
		Expect(res.GetConnection().GetGithub()).To(BeNil())
	})

	It("stores the tokens only as an envelope, and never returns or logs them", func(ctx SpecContext) {
		res, err := create(ctx, owner, nil)
		Expect(err).NotTo(HaveOccurred())
		id := res.GetConnection().GetId()
		stored, err := store.New(e.pool).GetConnection(ctx, store.GetConnectionParams{OrgID: mustUUID(org), ID: mustUUID(id)})
		Expect(err).NotTo(HaveOccurred())
		fields, err := connections.OpenFields(ring, stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fields["readToken"])).To(Equal(readTok))
		Expect(string(fields["writeToken"])).To(Equal(writeTok))

		got, err := owner.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: id})
		Expect(err).NotTo(HaveOccurred())
		list, err := owner.conns.ListConnections(ctx, &connectionv1alpha1.ListConnectionsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		tested, err := owner.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: id})
		Expect(err).NotTo(HaveOccurred())
		var row, summary string
		Expect(e.pool.QueryRow(ctx, `SELECT row_to_json(c)::text FROM connections c`).Scan(&row)).To(Succeed())
		Expect(e.pool.QueryRow(ctx, `SELECT summary::text FROM audit_events WHERE org_id = $1 AND action = 'connection.create'`, org).Scan(&summary)).To(Succeed())
		for _, s := range []string{string(mustJSON(res)), string(mustJSON(got)), string(mustJSON(list)), string(mustJSON(tested)), row, summary, logs.String()} {
			Expect(s).NotTo(ContainSubstring("not-a-real"))
		}
		Expect(logs.String()).To(ContainSubstring("connection created"))
	})

	It("keeps a sub-path and cleaned CI directories", func(ctx SpecContext) {
		res, err := create(ctx, owner, func(r *connectionv1alpha1.CreateGiteaConnectionRequest) {
			r.BaseUrl = " " + gt.URL() + "/gitea/ "
			r.CiDirs = []string{"./ci//pipelines/", "ci/pipelines"}
			r.Test = false
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnection().GetGitea().GetBaseUrl()).To(Equal(gt.URL() + "/gitea"))
		Expect(res.GetConnection().GetGitea().GetCiDirs()).To(Equal([]string{"ci/pipelines"}))
		Expect(res.GetConnection().GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_UNTESTED))
	})

	It("reports a read token that can write at its step", func(ctx SpecContext) {
		gt.AddScopedToken(readTok, "koinon-bot", "write")
		res, err := create(ctx, owner, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetConnection().GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_FAILING))
		Expect(res.GetResults()[3]).To(SatisfyAll(HaveField("Check", "gitea.read-token"), HaveField("Step", "test-read-token"), HaveField("Ok", false)))
	})

	DescribeTable("rejects, without quoting a token and writing nothing,",
		func(ctx SpecContext, mutate func(*connectionv1alpha1.CreateGiteaConnectionRequest)) {
			_, err := create(ctx, owner, func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.Test = false; mutate(r) })
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).NotTo(ContainSubstring("not-a-real"))
			Expect(logs.String()).NotTo(ContainSubstring("not-a-real"))
			Expect(count(ctx, e, `SELECT count(*) FROM connections`)).To(BeZero())
		},
		Entry("no base URL", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.BaseUrl = "" }),
		Entry("http", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.BaseUrl = "http://git.example.test" }),
		Entry("a glob in the path", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.BaseUrl = "https://git.example.test/*" }),
		Entry("an internal host", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.BaseUrl = "https://10.0.0.1/gitea" }),
		Entry("a bad CA bundle", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.CaBundle = "not pem" }),
		Entry("a missing read token", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.ReadToken = "" }),
		Entry("the same token twice", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.WriteToken = r.ReadToken }),
		Entry("a token with a space", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.WriteToken = "koinon_test not-a-real" }),
		Entry("an unsafe CI directory", func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.CiDirs = []string{"../up"} }),
	)

	It("needs an admin", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := create(ctx, member, nil)
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})

	It("enters lost tokens again, and a token equal to the stored one keeps the date it was entered", func(ctx SpecContext) {
		const newWriteTok = "koinon_test_not-a-real-write-token-2"
		gt.AddScopedToken(newWriteTok, "koinon-bot", "write")
		res, err := create(ctx, owner, func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.Test = false })
		Expect(err).NotTo(HaveOccurred())
		id, before := res.GetConnection().GetId(), res.GetConnection().GetGitea()
		_, err = e.pool.Exec(ctx, `UPDATE connections SET secret_envelope = NULL, credential_lost_at = now() WHERE id = $1`, id)
		Expect(err).NotTo(HaveOccurred())

		set, err := owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
			OrgId: org, ConnectionId: id, Test: true,
			Secret: &connectionv1alpha1.SetConnectionSecretRequest_Gitea{Gitea: &connectionv1alpha1.GiteaConnectionSecret{
				ReadToken: " " + readTok + " ", WriteToken: newWriteTok,
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetConnection().GetCredentialLostAt()).To(BeNil())
		Expect(set.GetConnection().GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_OK))
		after := set.GetConnection().GetGitea()
		Expect(after.GetBaseUrl()).To(Equal(before.GetBaseUrl()))
		Expect(after.GetCaBundle()).To(Equal(before.GetCaBundle()))
		Expect(after.GetReadTokenSetAt().AsTime()).To(Equal(before.GetReadTokenSetAt().AsTime()), "the same read token")
		Expect(after.GetWriteTokenSetAt().AsTime()).To(BeTemporally(">", before.GetWriteTokenSetAt().AsTime()), "a new write token")

		stored, err := store.New(e.pool).GetConnection(ctx, store.GetConnectionParams{OrgID: mustUUID(org), ID: mustUUID(id)})
		Expect(err).NotTo(HaveOccurred())
		fields, err := connections.OpenFields(ring, stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fields["readToken"])).To(Equal(readTok))
		Expect(string(fields["writeToken"])).To(Equal(newWriteTok))
		var summary string
		Expect(e.pool.QueryRow(ctx, `SELECT summary::text FROM audit_events WHERE org_id = $1 AND action = 'connection.secret.set'`, org).Scan(&summary)).To(Succeed())
		for _, s := range []string{string(mustJSON(set)), summary, logs.String()} {
			Expect(s).NotTo(ContainSubstring("not-a-real"))
		}

		By("both tokens are required, by the rules of create")
		_, err = owner.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
			OrgId: org, ConnectionId: id,
			Secret: &connectionv1alpha1.SetConnectionSecretRequest_Gitea{Gitea: &connectionv1alpha1.GiteaConnectionSecret{ReadToken: readTok}},
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).NotTo(ContainSubstring("not-a-real"))
	})

	It("lists the repositories the bot can see, for choosing what to bind", func(ctx SpecContext) {
		res, err := create(ctx, owner, func(r *connectionv1alpha1.CreateGiteaConnectionRequest) { r.Test = false })
		Expect(err).NotTo(HaveOccurred())
		repos, err := owner.conns.ListInstallationRepos(ctx, &connectionv1alpha1.ListInstallationReposRequest{OrgId: org, ConnectionId: res.GetConnection().GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(repos.GetTotalCount()).To(BeEquivalentTo(1))
		Expect(repos.GetRepos()).To(HaveLen(1))
		Expect(repos.GetRepos()[0].GetFullName()).To(Equal("acme/app"))
	})
})
