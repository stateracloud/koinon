// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

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
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
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
		Expect(res.GetConnectorTypes()).To(HaveLen(1))
		gh := res.GetConnectorTypes()[0]
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

	It("rejects a base URL that is not a bare https host and a CA bundle that is not certificates", func(ctx SpecContext) {
		for _, req := range []*connectionv1alpha1.CreateGitHubConnectionRequest{
			{BaseUrl: "http://ghes.example.test"},
			{BaseUrl: "https://ghes.example.test/enterprise"},
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
