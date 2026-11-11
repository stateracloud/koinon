// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/policycheck"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

const (
	git      = "/usr/bin/git"
	opencode = "/usr/local/bin/opencode"
)

var _ = Describe("The repo.read grant in the sandbox", func() {
	var (
		osg  *fakegateway.Gateway
		mg   *fakeModelGateway
		gh   *githubtest.Fake
		kube *fake.Clientset
		logs *syncBuffer
		now  atomic.Pointer[time.Time]
		cfg  reconciler.Config
		org  pgtype.UUID
		user pgtype.UUID
		conn store.Connection
		r    *reconciler.Reconciler
	)
	clock := func() time.Time {
		if t := now.Load(); t != nil {
			return *t
		}
		return time.Now()
	}
	advance := func(d time.Duration) {
		t := clock().Add(d)
		now.Store(&t)
	}
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}

	// agentWith creates and activates an agent whose version grants repo.read (and the ask-tier
	// scopes of the repo-helper template) on repos of conn.
	agentWith := func(ctx context.Context, preset templates.Preset, repos ...string) store.Agent {
		GinkgoHelper()
		c := agentconfig.Config{
			Instructions: "help",
			Grants: []templates.Grant{{
				Connector: "github", Scopes: []string{"repo.read", "pull_request.create", "issue.comment"}, Preset: preset,
			}},
		}
		if len(repos) > 0 {
			c.Bindings = []agentconfig.Binding{{Connector: "github", ConnectionID: uuidOf(conn.ID), Targets: repos}}
		}
		raw, err := c.Marshal()
		Expect(err).NotTo(HaveOccurred())
		agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
			OrgID: org, DisplayName: "Repo helper", Flavor: store.AgentFlavorOpencode, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent.ID, Config: raw, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		keepWarm(ctx, org, agent.ID)
		return agent
	}

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		mg = &fakeModelGateway{}
		kube = fake.NewClientset()
		logs = &syncBuffer{}
		now.Store(nil)
		gh = githubtest.New()
		gh.Now = clock
		DeferCleanup(gh.Server.Close)

		org, user = newOrg(ctx)
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		envelope, err := connections.SealFields(ring, org, id, map[string][]byte{connections.GitHubPrivateKeyField: gh.PEM})
		Expect(err).NotTo(HaveOccurred())
		cfgJSON, err := json.Marshal(connections.GitHubConfig{AppID: gh.AppID, InstallationID: gh.InstallationID})
		Expect(err).NotTo(HaveOccurred())
		conn, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: "github", DisplayName: "acme", Config: cfgJSON,
			SecretEnvelope: envelope, Source: store.ConfigSourceUi, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())

		cfg = reconciler.Config{
			Prices:  prices.NewResolver(pricing.Embedded()),
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Flavors: map[store.AgentFlavor]reconciler.Flavor{
				store.AgentFlavorOpencode: {Image: image, Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
			},
			GitHub: reconciler.GitHubApps{
				Keys:   ring,
				Client: github.NewClient(github.Options{BaseURL: gh.URL(), Now: clock}),
			},
			Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Now:    clock,
		}
		r, err = reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
	})

	names := func(agent store.Agent) execution.AgentNames {
		return execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
	}
	ws := func() string { return execution.WorkspaceName(org.Bytes) }
	latestPolicy := func(agent store.Agent) *sandboxv1.SandboxPolicy {
		GinkgoHelper()
		revs := osg.Policies(ws(), names(agent).Sandbox)
		Expect(revs).NotTo(BeEmpty())
		return revs[len(revs)-1]
	}
	// evaluate decides a request under the sandbox's policy with its attached profiles.
	evaluate := func(agent store.Agent, bin, host, method, target string) policycheck.Verdict {
		GinkgoHelper()
		var profiles []*openshellv1.ProviderProfile
		for _, p := range osg.Sandbox(ws(), names(agent).Sandbox).GetSpec().GetProviders() {
			rec := osg.Provider(ws(), p)
			Expect(rec).NotTo(BeNil(), p)
			profiles = append(profiles, osg.Profile(ws(), rec.GetType()))
		}
		path, query, _ := strings.Cut(target, "?")
		return policycheck.Evaluate(latestPolicy(agent), profiles, policycheck.Request{
			Binary: bin, Host: host, Port: 443, Method: method, Path: path, Query: query,
		})
	}
	allowed := func(agent store.Agent, bin, host, method, target string) bool {
		GinkgoHelper()
		return evaluate(agent, bin, host, method, target).Allowed
	}
	token := func(agent store.Agent) string {
		GinkgoHelper()
		p := osg.Provider(ws(), names(agent).GitHubProvider)
		Expect(p).NotTo(BeNil())
		return p.GetCredentials()["GITHUB_TOKEN"]
	}

	It("renders a provider record with a read-only token and an enforced policy for one repository", func(ctx SpecContext) {
		agent := agentWith(ctx, templates.PresetStandard, "acme/widgets")
		pass(ctx)
		n := names(agent)

		By("the token is narrowed to the repository, read only")
		Expect(gh.Requests()).To(ConsistOf(githubtest.Request{
			Repositories: []string{"widgets"},
			Permissions:  map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read", "issues": "read"},
		}))
		rec := osg.Provider(ws(), n.GitHubProvider)
		Expect(rec).NotTo(BeNil())
		Expect(rec.GetType()).To(Equal(n.GitHubProfile))
		Expect(rec.GetCredentials()).To(HaveKeyWithValue("GITHUB_TOKEN", HavePrefix("ghs_")))
		Expect(rec.GetCredentials()["GH_TOKEN"]).To(Equal(rec.GetCredentials()["GITHUB_TOKEN"]))
		Expect(rec.GetMetadata().GetLabels()).To(HaveKeyWithValue(reconciler.LabelAgent, uuidOf(agent.ID)))

		By("the profile binds it to the repository's endpoints behind a stable placeholder")
		profile := osg.Profile(ws(), n.GitHubProfile)
		Expect(profile.GetCategory()).To(Equal(openshellv1.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_SOURCE_CONTROL))
		Expect(profile.GetCredentials()).To(HaveLen(1))
		Expect(profile.GetCredentials()[0].GetStablePlaceholder()).To(BeTrue())
		Expect(profile.GetCredentials()[0].GetEnvVars()).To(Equal([]string{"GITHUB_TOKEN", "GH_TOKEN"}))
		Expect(profile.GetBinaries()).To(HaveExactElements(HaveField("Path", git), HaveField("Path", opencode)))
		Expect(profile.GetEndpoints()).To(HaveLen(2))
		for _, ep := range profile.GetEndpoints() {
			Expect(ep.GetProtocol()).To(Equal("rest"))
			Expect(ep.GetEnforcement()).To(Equal(sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE))
			Expect(ep.GetAccess()).To(Equal(sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_UNSPECIFIED))
			Expect(ep.GetTls()).To(Equal(sandboxv1.NetworkTlsMode_NETWORK_TLS_MODE_UNSPECIFIED))
		}

		By("the sandbox attaches both records, and its policy holds the same endpoints")
		sb := osg.Sandbox(ws(), n.Sandbox)
		Expect(sb.GetSpec().GetProviders()).To(Equal([]string{n.Provider, n.GitHubProvider}))
		rule := latestPolicy(agent).GetNetworkPolicies()["github_read"]
		Expect(rule).NotTo(BeNil())
		Expect(rule.GetEndpoints()).To(HaveLen(2))
		for i, ep := range rule.GetEndpoints() {
			Expect(proto.Equal(ep, profile.GetEndpoints()[i])).To(BeTrue())
		}
		github := rule.GetEndpoints()[0]
		Expect(github.GetHost()).To(Equal("github.com"))
		Expect(github.GetRules()).To(HaveLen(4))
		Expect(github.GetRules()[0].GetAllow().GetPath()).To(Equal("/acme/widgets.git/info/refs"))
		Expect(github.GetRules()[0].GetAllow().GetQuery()).To(HaveKeyWithValue("service", HaveField("Glob", "git-upload-pack")))
		api := rule.GetEndpoints()[1]
		Expect(api.GetHost()).To(Equal("api.github.com"))
		Expect(api.GetRules()).To(HaveExactElements(
			HaveField("Allow", SatisfyAll(HaveField("Method", "GET"), HaveField("Path", "/repos/acme/widgets"))),
			HaveField("Allow", SatisfyAll(HaveField("Method", "GET"), HaveField("Path", "/repos/acme/widgets/**"))),
		))

		By("clone, fetch and REST GET are allowed")
		for _, bin := range []string{git, opencode} {
			Expect(allowed(agent, bin, "github.com", "GET", "/acme/widgets.git/info/refs?service=git-upload-pack")).To(BeTrue())
			Expect(allowed(agent, bin, "github.com", "POST", "/acme/widgets.git/git-upload-pack")).To(BeTrue())
			Expect(allowed(agent, bin, "github.com", "GET", "/acme/widgets/info/refs?service=git-upload-pack")).To(BeTrue())
		}
		Expect(allowed(agent, opencode, "api.github.com", "GET", "/repos/acme/widgets")).To(BeTrue())
		Expect(allowed(agent, opencode, "api.github.com", "GET", "/repos/acme/widgets/pulls/1")).To(BeTrue())
		Expect(allowed(agent, opencode, "api.github.com", "GET", "/repos/acme/widgets/contents/README.md?ref=main")).To(BeTrue())

		By("push, POST, other repositories and other binaries are not")
		for _, c := range []struct{ host, method, target string }{
			{"github.com", "GET", "/acme/widgets.git/info/refs?service=git-receive-pack"},
			{"github.com", "GET", "/acme/widgets.git/info/refs?service=git-upload-pack&service=git-receive-pack"},
			{"github.com", "POST", "/acme/widgets.git/git-receive-pack"},
			{"github.com", "POST", "/acme/widgets/git-receive-pack"},
			{"github.com", "GET", "/acme/gadgets.git/info/refs?service=git-upload-pack"},
			{"github.com", "POST", "/acme/gadgets.git/git-upload-pack"},
			{"github.com", "POST", "/acme/widgets.git/../gadgets.git/git-upload-pack"},
			{"github.com", "GET", "/acme/Widgets.git/info/refs?service=git-upload-pack"},
			{"github.com", "GET", "/acme/widgets"},
			{"api.github.com", "POST", "/repos/acme/widgets/pulls"},
			{"api.github.com", "POST", "/repos/acme/widgets/issues/1/comments"},
			{"api.github.com", "PUT", "/repos/acme/widgets/contents/x"},
			{"api.github.com", "DELETE", "/repos/acme/widgets"},
			{"api.github.com", "PATCH", "/repos/acme/widgets"},
			{"api.github.com", "GET", "/repos/acme/gadgets"},
			{"api.github.com", "GET", "/repos/acme/widgets-other"},
			{"api.github.com", "GET", "/user"},
			{"api.github.com", "GET", "/search/code?q=repo:acme/gadgets"},
			{"api.github.com", "POST", "/graphql"},
			{"uploads.github.com", "POST", "/repos/acme/widgets/releases/1/assets"},
			{"evil.example.com", "GET", "/"},
		} {
			v := evaluate(agent, git, c.host, c.method, c.target)
			Expect(v.Allowed).To(BeFalse(), "%s %s%s: %s", c.method, c.host, c.target, v.Reason)
		}
		Expect(allowed(agent, "/tmp/git", "github.com", "GET", "/acme/widgets.git/info/refs?service=git-upload-pack")).To(BeFalse())
	})

	It("renders both repositories of a two-repository binding and nothing else", func(ctx SpecContext) {
		agent := agentWith(ctx, templates.PresetReadOnly, "acme/widgets", "acme/gadgets")
		pass(ctx)
		Expect(gh.Requests()).To(HaveLen(1))
		Expect(gh.Requests()[0].Repositories).To(ConsistOf("widgets", "gadgets"))
		for _, repo := range []string{"widgets", "gadgets"} {
			Expect(allowed(agent, git, "github.com", "GET", "/acme/"+repo+".git/info/refs?service=git-upload-pack")).To(BeTrue(), repo)
			Expect(allowed(agent, git, "github.com", "POST", "/acme/"+repo+".git/git-upload-pack")).To(BeTrue(), repo)
			Expect(allowed(agent, git, "github.com", "POST", "/acme/"+repo+".git/git-receive-pack")).To(BeFalse(), repo)
			Expect(allowed(agent, opencode, "api.github.com", "GET", "/repos/acme/"+repo+"/issues")).To(BeTrue(), repo)
			Expect(allowed(agent, opencode, "api.github.com", "POST", "/repos/acme/"+repo+"/issues")).To(BeFalse(), repo)
		}
		Expect(allowed(agent, git, "github.com", "GET", "/acme/sprockets.git/info/refs?service=git-upload-pack")).To(BeFalse())
		Expect(allowed(agent, opencode, "api.github.com", "GET", "/repos/acme/sprockets")).To(BeFalse())
		Expect(latestPolicy(agent).GetNetworkPolicies()["github_read"].GetEndpoints()[0].GetRules()).To(HaveLen(8))
	})

	It("refreshes the token in place before it expires, without touching the sandbox", func(ctx SpecContext) {
		agent := agentWith(ctx, templates.PresetStandard, "acme/widgets")
		pass(ctx)
		queueRun(ctx, org, user, agent.ID)
		claimRun(ctx, org) // an active run: the sandbox must survive the refresh
		first := token(agent)
		Expect(osg.Calls("UpdateProvider")).To(Equal(0))

		By("well before expiry the cached token stays")
		advance(40 * time.Minute)
		pass(ctx)
		Expect(gh.Mints()).To(Equal(1))
		Expect(token(agent)).To(Equal(first))
		Expect(osg.Calls("UpdateProvider")).To(Equal(0))

		By("inside the refresh margin a new token replaces it in the same record")
		advance(11 * time.Minute) // 51 minutes in: 9 minutes left
		pass(ctx)
		Expect(gh.Mints()).To(Equal(2))
		second := token(agent)
		Expect(second).NotTo(Equal(first))
		Expect(osg.Provider(ws(), names(agent).GitHubProvider).GetCredentials()["GH_TOKEN"]).To(Equal(second))
		Expect(osg.Calls("UpdateProvider")).To(Equal(1))
		Expect(osg.Calls("CreateProvider")).To(Equal(2), "one agentgateway and one GitHub record, created once")
		Expect(osg.Calls("UpdateProviderProfiles")).To(Equal(0))
		Expect(osg.Calls("CreateSandbox")).To(Equal(1))
		Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
		Expect(osg.Calls("StopSandbox")).To(Equal(0))
		Expect(osg.Sandbox(ws(), names(agent).Sandbox).GetStatus().GetPhase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
		Expect(strings.Count(logs.String(), "pushed GitHub installation token")).To(Equal(2))

		By("and later passes reuse it")
		pass(ctx)
		Expect(gh.Mints()).To(Equal(2))
		Expect(osg.Calls("UpdateProvider")).To(Equal(1))
	})

	It("keeps the token out of the sandbox spec, its env, policies, profiles, Postgres and logs", func(ctx SpecContext) {
		agent := agentWith(ctx, templates.PresetStandard, "acme/widgets")
		pass(ctx)
		advance(51 * time.Minute)
		pass(ctx) // a refresh, so both tokens are checked
		tokens := []string{"ghs_faketoken0001", "ghs_faketoken0002"}
		Expect(token(agent)).To(Equal(tokens[1]))

		n := names(agent)
		sb := osg.Sandbox(ws(), n.Sandbox)
		Expect(sb.GetSpec().GetEnvironment()).To(BeEmpty())
		Expect(sb.GetSpec().GetTemplate().GetEnvironment()).To(BeEmpty())
		var seen []string
		add := func(m proto.Message) {
			b, err := protojson.Marshal(m)
			Expect(err).NotTo(HaveOccurred())
			seen = append(seen, string(b))
		}
		add(sb)
		for _, p := range osg.Policies(ws(), n.Sandbox) {
			add(p)
		}
		add(osg.Profile(ws(), n.GitHubProfile))
		add(osg.Profile(ws(), "koinon-agentgateway-opencode"))
		for _, e := range osg.Execs() {
			add(e)
		}
		var rows string
		Expect(pool.QueryRow(ctx, `SELECT coalesce((SELECT json_agg(s)::text FROM agent_sandboxes s), '') ||
			coalesce((SELECT json_agg(v)::text FROM agent_versions v), '') ||
			coalesce((SELECT json_agg(c)::text FROM connections c), '')`).Scan(&rows)).To(Succeed())
		seen = append(seen, rows, fmt.Sprintf("%+v", mg.applied), logs.String())
		secrets, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, s := range secrets.Items {
			for _, v := range s.Data {
				seen = append(seen, string(v))
			}
		}
		for _, s := range seen {
			for _, t := range tokens {
				Expect(s).NotTo(ContainSubstring(t))
			}
			Expect(s).NotTo(ContainSubstring("ghs_"))
		}
		Expect(logs.String()).To(ContainSubstring("pushed GitHub installation token"))
		Expect(logs.String()).To(ContainSubstring("expires_at"))

		By("the sandbox learns the token only as the environment variables the profile declares")
		Expect(osg.Profile(ws(), n.GitHubProfile).GetCredentials()[0].GetEnvVars()).To(ConsistOf("GITHUB_TOKEN", "GH_TOKEN"))
	})

	It("renders no GitHub access without a binding, and cleans up on archive", func(ctx SpecContext) {
		unbound := agentWith(ctx, templates.PresetStandard)
		bound := agentWith(ctx, templates.PresetStandard, "acme/widgets")
		pass(ctx)
		Expect(osg.Provider(ws(), names(unbound).GitHubProvider)).To(BeNil())
		Expect(osg.Sandbox(ws(), names(unbound).Sandbox).GetSpec().GetProviders()).To(Equal([]string{names(unbound).Provider}))
		Expect(latestPolicy(unbound).GetNetworkPolicies()).To(BeEmpty())
		Expect(allowed(unbound, git, "github.com", "GET", "/acme/widgets.git/info/refs?service=git-upload-pack")).To(BeFalse())

		_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: bound.ID})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(osg.Provider(ws(), names(bound).GitHubProvider)).To(BeNil())
		Expect(osg.Profile(ws(), names(bound).GitHubProfile)).To(BeNil())
	})

	It("reports an error and creates no sandbox when the token cannot be minted", func(ctx SpecContext) {
		agent := agentWith(ctx, templates.PresetStandard, "acme/not-installed")
		Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("mint installation token")))
		Expect(osg.Sandbox(ws(), names(agent).Sandbox)).To(BeNil())
		row, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Message).To(ContainSubstring("not accessible to the parent installation"))
	})
})

func uuidOf(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }
