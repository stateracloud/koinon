// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"log/slog"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentconfig"
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

var _ = Describe("Plugin installs in the sandbox policy", func() {
	var (
		osg *fakegateway.Gateway
		r   *reconciler.Reconciler
	)

	BeforeEach(func() {
		osg = fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		r, err = reconciler.New(pool, openshell.New(cc), &fakeModelGateway{},
			reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
				Prices:  prices.NewResolver(pricing.Embedded()),
				Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
				Harnesses: map[string]reconciler.Harness{
					harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
				},
				Logger: slog.New(slog.DiscardHandler),
			})
		Expect(err).NotTo(HaveOccurred())
	})

	// reconcile activates an OpenCode agent with the harness settings, runs a pass and
	// returns a function that decides requests under its sandbox's latest policy.
	reconcile := func(ctx context.Context, settings templates.HarnessSettings) func(bin, method, target string) bool {
		GinkgoHelper()
		org, user := newOrg(ctx)
		raw, err := agentconfig.Config{Instructions: "help", Harness: settings}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
			OrgID: org, DisplayName: "Plugin agent", Harness: harness.OpenCode, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent.ID, Config: raw, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		keepWarm(ctx, org, agent.ID)
		Expect(r.ReconcileAll(ctx)).To(Succeed())

		ws, sb := execution.WorkspaceName(org.Bytes), execution.AgentObjectNames(org.Bytes, agent.ID.Bytes).Sandbox
		revs := osg.Policies(ws, sb)
		Expect(revs).NotTo(BeEmpty())
		policy := revs[len(revs)-1]
		var profiles []*openshellv1.ProviderProfile
		for _, p := range osg.Sandbox(ws, sb).GetSpec().GetProviders() {
			profiles = append(profiles, osg.Profile(ws, osg.Provider(ws, p).GetType()))
		}
		return func(bin, method, target string) bool {
			path, query, _ := strings.Cut(target, "?")
			return policycheck.Evaluate(policy, profiles, policycheck.Request{
				Binary: bin, Host: reconciler.NPMRegistryHost, Port: 443, Method: method, Path: path, Query: query,
			}).Allowed
		}
	}

	It("renders no npm endpoint when the option is off", func(ctx SpecContext) {
		allowed := reconcile(ctx, templates.HarnessSettings{})
		Expect(allowed(opencode, "GET", "/@opencode-ai%2fplugin")).To(BeFalse())
		Expect(allowed(opencode, "GET", "/left-pad/-/left-pad-1.3.0.tgz")).To(BeFalse())
	})

	It("lets the OpenCode binary read the npm registry, and nothing else, when the option is on", func(ctx SpecContext) {
		allowed := reconcile(ctx, templates.HarnessSettings{PluginInstalls: true})

		By("GET and HEAD of metadata and tarballs, from OpenCode")
		Expect(allowed(opencode, "GET", "/@opencode-ai%2fplugin")).To(BeTrue())
		Expect(allowed(opencode, "GET", "/left-pad/-/left-pad-1.3.0.tgz")).To(BeTrue())
		Expect(allowed(opencode, "HEAD", "/left-pad")).To(BeTrue())

		By("no writes: publish, unpublish, login, tokens")
		for _, c := range []struct{ method, target string }{
			{"PUT", "/left-pad"},
			{"DELETE", "/left-pad/-rev/1-abc"},
			{"POST", "/-/v1/login"},
			{"PUT", "/-/user/org.couchdb.user:x"},
			{"POST", "/-/npm/v1/security/advisories/bulk"},
		} {
			Expect(allowed(opencode, c.method, c.target)).To(BeFalse(), "%s %s", c.method, c.target)
		}

		By("no other binary")
		for _, bin := range []string{"/usr/bin/git", "/usr/bin/curl", "/usr/bin/node", "/usr/local/bin/npm"} {
			Expect(allowed(bin, "GET", "/left-pad")).To(BeFalse(), bin)
		}
	})

	It("attaches no credential to the registry", func() {
		rule, ok := reconciler.PluginInstallRule(agentconfig.Config{Harness: templates.HarnessSettings{PluginInstalls: true}},
			harness.OpenCode, reconciler.Harness{Image: image, Binaries: []string{opencode}})
		Expect(ok).To(BeTrue())
		Expect(rule.Binaries).To(Equal([]string{opencode}))
		Expect(rule.Endpoints).To(HaveExactElements(SatisfyAll(
			HaveField("Host", "registry.npmjs.org"), HaveField("Port", BeEquivalentTo(443)),
			HaveField("Protocol", "rest"), HaveField("Access", execution.AccessNone),
		)))
	})

	It("renders nothing for a harness without the option, which has no plugin installs", func() {
		_, ok := reconciler.PluginInstallRule(agentconfig.Config{Harness: templates.HarnessSettings{PluginInstalls: true}},
			string("other"), reconciler.Harness{Image: image, Binaries: []string{"/opt/other/bin/other"}})
		Expect(ok).To(BeFalse())
	})
})
