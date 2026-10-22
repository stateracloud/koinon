// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"bytes"
	"strings"
	"sync"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/procoduck/koinon/internal/connections"
	"github.com/procoduck/koinon/internal/connectors/github"
	"github.com/procoduck/koinon/internal/connectors/github/githubtest"
	"github.com/procoduck/koinon/internal/db/store"
	connectionv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/connection/v1alpha1"
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
		logs  *lockedBuffer
		org   string
		owner *client
	)

	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		kube = fake.NewClientset()
		logs = &lockedBuffer{}
		e = newEnv(ctx, envOptions{
			secrets: connections.KubeSecrets{Client: kube, Namespace: "koinon"},
			github:  github.NewClient(github.Options{BaseURL: gh.URL()}),
			ghFake:  gh,
			logs:    logs,
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

	It("stores the key only in a Kubernetes Secret and never returns or logs it", func(ctx SpecContext) {
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

		By("the Secret holds the key, labelled with IDs")
		secrets, err := kube.CoreV1().Secrets("koinon").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets.Items).To(HaveLen(1))
		sec := secrets.Items[0]
		Expect(sec.Data[connections.GitHubPrivateKeyField]).To(Equal(gh.PEM))
		Expect(sec.Labels).To(HaveKeyWithValue(connections.LabelConnection, conn.GetId()))
		Expect(*sec.Immutable).To(BeTrue())

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

	It("refuses to create connections without a secret store", func(ctx SpecContext) {
		e2 := newEnvOn(ctx, e, envOptions{noSecrets: true})
		_, err := create(ctx, owner.on(e2), gh.PEM, false)
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})
})

func mustJSON(m proto.Message) []byte {
	b, err := protojson.Marshal(m)
	Expect(err).NotTo(HaveOccurred())
	return b
}
