// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("The repo.read grant on a connection's own host", func() {
	cfg := agentconfig.Config{
		Grants:   []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
		Bindings: []agentconfig.Binding{{Connector: "github", ConnectionID: "00000000-0000-0000-0000-000000000001", Targets: []string{"acme/widgets"}}},
	}

	hosts := func(read reconciler.GitHubRead) []string {
		var out []string
		for _, e := range read.Endpoints {
			out = append(out, e.Host)
		}
		return out
	}

	It("renders github.com and api.github.com without a base URL", func() {
		read, ok, err := reconciler.GitHubReadAccessAt(cfg, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(hosts(read)).To(Equal([]string{"github.com", "api.github.com"}))
		Expect(read).To(Equal(mustRead(cfg)))
	})

	It("renders only the connection's host for a GitHub Enterprise Server", func() {
		read, ok, err := reconciler.GitHubReadAccessAt(cfg, "https://ghes.example.test")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(hosts(read)).To(Equal([]string{"ghes.example.test"}))
		e := read.Endpoints[0]
		Expect(e.Port).To(BeEquivalentTo(443))
		Expect(e.Protocol).To(Equal("rest"))
		Expect(e.Allow).To(ContainElements(
			execution.L7Rule{Method: "POST", Path: "/acme/widgets.git/git-upload-pack"},
			execution.L7Rule{Method: "GET", Path: "/api/v3/repos/acme/widgets"},
		))
		Expect(e.Deny).To(ContainElement(execution.L7Rule{Method: "POST", Path: "/api/v3/**"}))

		// The provider profile and the policy entry carry the same endpoints, so the token is
		// only ever sent to the connection's host.
		profile := reconciler.GitHubProfile("p", read, reconciler.DefaultGitBinaries)
		Expect(profile.Endpoints).To(Equal(read.Endpoints))
		Expect(reconciler.GitHubRule(read, reconciler.DefaultGitBinaries).Endpoints).To(Equal(read.Endpoints))
	})

	It("keeps the port of the base URL", func() {
		read, _, err := reconciler.GitHubReadAccessAt(cfg, "https://ghes.example.test:8443")
		Expect(err).NotTo(HaveOccurred())
		Expect(read.Endpoints[0].Port).To(BeEquivalentTo(8443))
	})

	It("refuses a base URL that is not a bare https host", func() {
		_, _, err := reconciler.GitHubReadAccessAt(cfg, "http://ghes.example.test")
		Expect(err).To(HaveOccurred())
	})
})

func mustRead(cfg agentconfig.Config) reconciler.GitHubRead {
	read, ok, err := reconciler.GitHubReadAccess(cfg)
	Expect(err).NotTo(HaveOccurred())
	Expect(ok).To(BeTrue())
	return read
}

// fixedTokens mints the same fake token for any connection, so no request leaves the test for
// a connection's own host.
type fixedTokens struct{}

func (fixedTokens) ReadToken(_ context.Context, _ store.Connection, scope codehost.Scope) (codehost.Token, error) {
	return codehost.NewToken("koinon_test_not-a-real-token", time.Now().Add(time.Hour), scope.Repositories), nil
}

var _ = Describe("The repo.read grant of a stored GitHub Enterprise Server connection", func() {
	It("renders nothing for a connection whose host is a private address", func(ctx SpecContext) {
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		org, user := newOrg(ctx)
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		envelope, err := connections.SealFields(ring, org, id, map[string][]byte{connections.GitHubPrivateKeyField: []byte("unused")})
		Expect(err).NotTo(HaveOccurred())
		config, err := json.Marshal(map[string]any{"appId": 1, "installationId": 2, "baseUrl": "https://10.0.0.1"})
		Expect(err).NotTo(HaveOccurred())
		conn, err := q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: "github", DisplayName: "internal", Config: config,
			SecretEnvelope: envelope, Source: store.ConfigSourceUi, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())
		raw, err := agentconfig.Config{
			Instructions: "help",
			Grants:       []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings:     []agentconfig.Binding{{Connector: "github", ConnectionID: uuidOf(conn.ID), Targets: []string{"acme/widgets"}}},
		}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		agent, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "Internal helper", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent.ID, Config: raw, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		keepWarm(ctx, org, agent.ID)
		r, err := reconciler.New(pool, openshell.New(cc), &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
			Prices:  prices.NewResolver(pricing.Embedded()),
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			GitHub: fixedTokens{}, URLPolicy: testPolicy,
			Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		})
		Expect(err).NotTo(HaveOccurred())
		_ = r.ReconcileAll(ctx)
		n := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		Expect(osg.Profile(execution.WorkspaceName(org.Bytes), n.GitHubProfile)).To(BeNil(), "no egress to a private host")
		Expect(osg.Calls("CreateSandbox")).To(BeZero())
	})

	It("reads the host from the connection and renders only it, in the profile and the policy", func(ctx SpecContext) {
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })

		org, user := newOrg(ctx)
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		envelope, err := connections.SealFields(ring, org, id, map[string][]byte{connections.GitHubPrivateKeyField: []byte("unused")})
		Expect(err).NotTo(HaveOccurred())
		config, err := json.Marshal(map[string]any{"appId": 1, "installationId": 2, "baseUrl": "https://ghes.example.test"})
		Expect(err).NotTo(HaveOccurred())
		conn, err := q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: "github", DisplayName: "ghes", Config: config,
			SecretEnvelope: envelope, Source: store.ConfigSourceUi, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())

		raw, err := agentconfig.Config{
			Instructions: "help",
			Grants:       []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings:     []agentconfig.Binding{{Connector: "github", ConnectionID: uuidOf(conn.ID), Targets: []string{"acme/widgets"}}},
		}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		agent, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "GHES helper", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent.ID, Config: raw, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		keepWarm(ctx, org, agent.ID)

		r, err := reconciler.New(pool, openshell.New(cc), &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
			Prices:  prices.NewResolver(pricing.Embedded()),
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			GitHub: fixedTokens{}, URLPolicy: testPolicy,
			Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())

		n := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		ws := execution.WorkspaceName(org.Bytes)

		profile := osg.Profile(ws, n.GitHubProfile)
		Expect(profile).NotTo(BeNil())
		Expect(profile.GetEndpoints()).To(HaveLen(1))
		ep := profile.GetEndpoints()[0]
		Expect(ep.GetHost()).To(Equal("ghes.example.test"))
		var paths []string
		for _, rule := range ep.GetRules() {
			paths = append(paths, rule.GetAllow().GetPath())
		}
		Expect(paths).To(ContainElement("/api/v3/repos/acme/widgets"))
		Expect(paths).To(ContainElement("/acme/widgets.git/git-upload-pack"))

		revs := osg.Policies(ws, n.Sandbox)
		Expect(revs).NotTo(BeEmpty())
		rule := revs[len(revs)-1].GetNetworkPolicies()["github_read"]
		Expect(rule).NotTo(BeNil())
		Expect(rule.GetEndpoints()).To(HaveLen(1))
		Expect(rule.GetEndpoints()[0].GetHost()).To(Equal("ghes.example.test"))
		for _, e := range rule.GetEndpoints() {
			for _, r := range e.GetRules() {
				Expect(r.GetAllow().GetPath()).NotTo(HavePrefix("/repos/"), "no github.com API paths")
			}
		}
	})
})
