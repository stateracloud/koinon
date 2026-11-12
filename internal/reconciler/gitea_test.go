// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/policycheck"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

// The Gitea read grant (ADR 0063): the bot's READ token reaches the sandbox only through an
// OpenShell provider record and a placeholder; the WRITE token is not given to OpenShell at all.
var _ = Describe("The repo.read grant of a Gitea connection", func() {
	const (
		readTok  = "koinon_test_not-a-real-read-token"
		writeTok = "koinon_test_not-a-real-write-token"
		base     = "https://git.example.test/gitea"
	)
	var (
		osg   *fakegateway.Gateway
		org   pgtype.UUID
		user  pgtype.UUID
		conn  store.Connection
		agent store.Agent
		r     *reconciler.Reconciler
	)

	seal := func(read string) []byte {
		GinkgoHelper()
		env, err := connections.SealFields(ring, org, conn.ID, map[string][]byte{gitea.ReadTokenField: []byte(read), gitea.WriteTokenField: []byte(writeTok)})
		Expect(err).NotTo(HaveOccurred())
		return env
	}

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })

		org, user = newOrg(ctx)
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		conn.ID = id
		config, err := json.Marshal(gitea.NewConfig(codehost.Site{BaseURL: base}, []byte(readTok), []byte(writeTok), nil, time.Now()))
		Expect(err).NotTo(HaveOccurred())
		conn, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: "gitea", DisplayName: "lab", Config: config,
			SecretEnvelope: seal(readTok), Source: store.ConfigSourceUi, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())

		raw, err := agentconfig.Config{
			Instructions: "help",
			Grants:       []templates.Grant{{Connector: "gitea", Scopes: []string{"repo.read", "pull_request.create", "issue.comment"}, Preset: templates.PresetStandard}},
			Bindings:     []agentconfig.Binding{{Connector: "gitea", ConnectionID: uuidOf(conn.ID), Targets: []string{"acme/widgets"}}},
		}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "Gitea helper", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent.ID, Config: raw, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		keepWarm(ctx, org, agent.ID)

		// The shipped token source: no network, a Gitea token is static.
		r, err = reconciler.New(pool, openshell.New(cc), &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
			Prices:  prices.NewResolver(pricing.Embedded()),
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			GitHub: reconciler.GitHubApps{Keys: ring}, URLPolicy: testPolicy,
			Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		})
		Expect(err).NotTo(HaveOccurred())
	})

	n := func() execution.AgentNames { return execution.AgentObjectNames(org.Bytes, agent.ID.Bytes) }
	ws := func() string { return execution.WorkspaceName(org.Bytes) }
	evaluate := func(bin, host, method, target string) policycheck.Verdict {
		GinkgoHelper()
		var profiles []*openshellv1.ProviderProfile
		for _, p := range osg.Sandbox(ws(), n().Sandbox).GetSpec().GetProviders() {
			rec := osg.Provider(ws(), p)
			Expect(rec).NotTo(BeNil(), p)
			profiles = append(profiles, osg.Profile(ws(), rec.GetType()))
		}
		revs := osg.Policies(ws(), n().Sandbox)
		Expect(revs).NotTo(BeEmpty())
		path, query, _ := strings.Cut(target, "?")
		return policycheck.Evaluate(revs[len(revs)-1], profiles, policycheck.Request{Binary: bin, Host: host, Port: 443, Method: method, Path: path, Query: query})
	}

	It("puts the read token in the provider record behind a placeholder, and the write token nowhere", func(ctx SpecContext) {
		Expect(r.ReconcileAll(ctx)).To(Succeed())

		rec := osg.Provider(ws(), n().GitHubProvider)
		Expect(rec).NotTo(BeNil())
		Expect(rec.GetCredentials()).To(HaveKeyWithValue("GITHUB_TOKEN", readTok))
		Expect(rec.GetCredentials()).To(HaveKeyWithValue("GH_TOKEN", readTok))
		profile := osg.Profile(ws(), n().GitHubProfile)
		Expect(profile.GetDisplayName()).To(Equal("Gitea (read)"))
		Expect(profile.GetCredentials()[0].GetStablePlaceholder()).To(BeTrue())
		Expect(profile.GetCredentials()[0].GetEnvVars()).To(Equal([]string{"GITHUB_TOKEN", "GH_TOKEN"}))
		Expect(profile.GetEndpoints()).To(HaveLen(1))
		Expect(profile.GetEndpoints()[0].GetHost()).To(Equal("git.example.test"))

		By("everything OpenShell holds for the agent, except that one credential field, is token-free")
		sb := osg.Sandbox(ws(), n().Sandbox)
		var dumps []string
		sbJSON, err := protojson.Marshal(sb)
		Expect(err).NotTo(HaveOccurred())
		dumps = append(dumps, string(sbJSON))
		for _, rev := range osg.Policies(ws(), n().Sandbox) {
			b, err := protojson.Marshal(rev)
			Expect(err).NotTo(HaveOccurred())
			dumps = append(dumps, string(b))
		}
		pj, err := protojson.Marshal(profile)
		Expect(err).NotTo(HaveOccurred())
		dumps = append(dumps, string(pj))
		for _, d := range dumps {
			Expect(d).NotTo(ContainSubstring("not-a-real"), "no token in the sandbox spec (its env included), the policy or the profile")
		}
		pr, err := protojson.Marshal(rec)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(pr)).NotTo(ContainSubstring(writeTok), "the write token is never given to OpenShell")
		for k, v := range sb.GetSpec().GetEnvironment() {
			Expect(v).NotTo(ContainSubstring("not-a-real"), k)
		}
	})

	It("allows clone, fetch and REST reads of the bound repository under the sub-path, and nothing else", func(ctx SpecContext) {
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		const h = "git.example.test"
		for _, bin := range []string{git, opencode} {
			Expect(evaluate(bin, h, "GET", "/gitea/acme/widgets.git/info/refs?service=git-upload-pack").Allowed).To(BeTrue())
			Expect(evaluate(bin, h, "POST", "/gitea/acme/widgets.git/git-upload-pack").Allowed).To(BeTrue())
		}
		Expect(evaluate(opencode, h, "GET", "/gitea/api/v1/repos/acme/widgets").Allowed).To(BeTrue())
		Expect(evaluate(opencode, h, "GET", "/gitea/api/v1/repos/acme/widgets/pulls/1").Allowed).To(BeTrue())

		By("with the read token, push is denied however it is spelled")
		for _, c := range []struct{ method, target string }{
			{"GET", "/gitea/acme/widgets.git/info/refs?service=git-receive-pack"},
			{"GET", "/gitea/acme/widgets.git/info/refs?service=git-upload-pack&service=git-receive-pack"},
			{"POST", "/gitea/acme/widgets.git/git-receive-pack"},
			{"POST", "/gitea/acme/widgets/git-receive-pack"},
		} {
			v := evaluate(git, h, c.method, c.target)
			Expect(v.Allowed).To(BeFalse(), "%s %s: %s", c.method, c.target, v.Reason)
		}

		By("writes through the REST API are denied")
		for _, c := range []struct{ method, target string }{
			{"POST", "/gitea/api/v1/repos/acme/widgets/pulls"},
			{"POST", "/gitea/api/v1/repos/acme/widgets/issues/1/comments"},
			{"POST", "/gitea/api/v1/repos/acme/widgets/contents"},
			{"PUT", "/gitea/api/v1/repos/acme/widgets/contents/x"},
			{"DELETE", "/gitea/api/v1/repos/acme/widgets"},
			{"PATCH", "/gitea/api/v1/repos/acme/widgets"},
		} {
			Expect(evaluate(opencode, h, c.method, c.target).Allowed).To(BeFalse(), "%s %s", c.method, c.target)
		}

		By("other repositories, other API endpoints, other hosts and the path without the sub-path are denied")
		for _, c := range []struct{ host, method, target string }{
			{h, "GET", "/gitea/acme/gadgets.git/info/refs?service=git-upload-pack"},
			{h, "POST", "/gitea/acme/gadgets.git/git-upload-pack"},
			{h, "GET", "/gitea/api/v1/repos/acme/gadgets"},
			{h, "GET", "/gitea/api/v1/repos/acme/widgets-other"},
			{h, "GET", "/gitea/api/v1/user"},
			{h, "GET", "/gitea/api/v1/user/repos"},
			{h, "GET", "/gitea/api/v1/repos/search?q=gadgets"},
			{h, "GET", "/gitea/api/v1/admin/users"},
			{h, "GET", "/acme/widgets.git/info/refs?service=git-upload-pack"},
			{h, "GET", "/api/v1/repos/acme/widgets"},
			{h, "GET", "/gitea/acme/Widgets.git/info/refs?service=git-upload-pack"},
			{"github.com", "GET", "/acme/widgets.git/info/refs?service=git-upload-pack"},
			{"gitea.invalid", "GET", "/gitea/api/v1/repos/acme/widgets"},
		} {
			v := evaluate(git, c.host, c.method, c.target)
			Expect(v.Allowed).To(BeFalse(), "%s %s%s: %s", c.method, c.host, c.target, v.Reason)
		}
		Expect(evaluate("/tmp/git", h, "GET", "/gitea/acme/widgets.git/info/refs?service=git-upload-pack").Allowed).To(BeFalse())
	})

	It("does not push the same static token again, and pushes a new one into the same record", func(ctx SpecContext) {
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(osg.Calls("UpdateProvider")).To(BeZero(), "a static token needs no refresh")

		By("a replaced token is pushed into the same record, so running processes switch to it")
		const fresh = "koinon_test_not-a-real-read-token-2"
		_, err := pool.Exec(ctx, `UPDATE connections SET secret_envelope = $1 WHERE id = $2`, seal(fresh), conn.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(osg.Provider(ws(), n().GitHubProvider).GetCredentials()).To(HaveKeyWithValue("GITHUB_TOKEN", fresh))
		Expect(osg.Calls("UpdateProvider")).To(Equal(1))
	})
})
