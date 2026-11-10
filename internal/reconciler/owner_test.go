// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

// Install ownership of OpenShell objects (issue 461): two installs share one gateway and one set of
// rows (a restored database next to the live install), and objects made before the install mark
// existed are claimed by the live install.
var _ = Describe("Reconciler install ownership", func() {
	var (
		osg  *fakegateway.Gateway
		org  pgtype.UUID
		user pgtype.UUID
		ws   string
		mk   func(owner string, maxRecreates int) *reconciler.Reconciler
		m    *reconciler.Metrics
	)
	sandboxOf := func(agent store.Agent) string { return execution.AgentSandboxName(org.Bytes, agent.ID.Bytes) }
	labels := func(agent store.Agent) map[string]string {
		GinkgoHelper()
		sb := osg.Sandbox(ws, sandboxOf(agent))
		Expect(sb).NotTo(BeNil())
		return sb.GetMetadata().GetLabels()
	}
	row := func(ctx context.Context, agent store.Agent) store.AgentSandbox {
		GinkgoHelper()
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		return sb
	}

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		m = reconciler.NewMetrics(nil)
		mk = func(owner string, maxRecreates int) *reconciler.Reconciler {
			b := openshell.New(cc)
			if owner != "" {
				b = b.WithOwner(execution.OwnerID(owner))
			}
			r, err := reconciler.New(pool, b, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
				Prices:         prices.NewResolver(pricing.Embedded()),
				Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
				Keys:           ring,
				ProviderKeys:   reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
				ProviderKeyDir: "/secrets/providers",
				URLPolicy:      testPolicy,
				Flavors: map[store.AgentFlavor]reconciler.Flavor{
					store.AgentFlavorOpencode: {Image: image, Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
				},
				RuntimeClassName:    "kata",
				MaxRecreatesPerPass: maxRecreates,
				Metrics:             m,
				Logger:              slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})
			Expect(err).NotTo(HaveOccurred())
			return r
		}
	})

	It("records OwnedByAnotherInstall and touches nothing when another install owns the sandbox", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		live := mk("mark-live", 0)
		Expect(live.ReconcileAll(ctx)).To(Succeed())
		Expect(labels(agent)).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
		id := osg.Sandbox(ws, sandboxOf(agent)).GetMetadata().GetId()
		deletes, stops := osg.Calls("DeleteSandbox"), osg.Calls("StopSandbox")

		restore := mk("mark-restore", 0)
		Expect(restore.ReconcileAll(ctx)).NotTo(Succeed())
		got := row(ctx, agent)
		Expect(got.State).To(Equal(store.SandboxStateError))
		Expect(got.Reason).To(Equal(reconciler.ForeignReason))
		Expect(osg.Calls("DeleteSandbox")).To(Equal(deletes))
		Expect(osg.Calls("StopSandbox")).To(Equal(stops))
		Expect(osg.Sandbox(ws, sandboxOf(agent)).GetMetadata().GetId()).To(Equal(id))
		Expect(labels(agent)).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
	})

	Describe("a sandbox made before the install mark", func() {
		var agent store.Agent
		BeforeEach(func(ctx SpecContext) {
			agent = newAgent(ctx, org, user)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed())
			Expect(labels(agent)).NotTo(HaveKey(execution.LabelInstall))
		})

		It("is recreated with the label by its owner, and its provider claimed after the sandbox is gone", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // deletes the sandbox; its provider is still attached
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(osg.Provider(ws, names.Provider)).NotTo(BeNil())
			Expect(osg.Provider(ws, names.Provider).GetMetadata().GetLabels()).NotTo(HaveKey(execution.LabelInstall))
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // claims the provider, creates the sandbox
			Expect(labels(agent)).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
			Expect(osg.Provider(ws, names.Provider).GetMetadata().GetLabels()).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
			Expect(osg.Calls("DeleteProvider")).To(Equal(1))
		})

		It("is counted in koinon_openshell_unmarked_objects from the gateway until it is recreated and claimed", func(ctx SpecContext) {
			gauge := func(kind string) float64 { return testutil.ToFloat64(m.UnmarkedObjects.WithLabelValues(kind)) }
			stale := func() float64 { return testutil.ToFloat64(m.UnmarkedStale) }
			Expect(stale()).To(Equal(1.0), "nothing has been counted yet")
			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // deletes the sandbox; its provider is still attached
			Expect(stale()).To(BeZero())
			Expect(gauge("sandbox")).To(BeZero(), "the unmarked sandbox is gone")
			Expect(gauge("provider")).To(Equal(1.0))
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // claims the provider, creates the sandbox
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			for _, kind := range []string{"sandbox", "provider", "profile"} {
				Expect(gauge(kind)).To(BeZero(), kind)
			}
			Expect(stale()).To(BeZero())
		})

		It("is not safe while the install has no mark: the gauges read 0 and stale reads 1", func(ctx SpecContext) {
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed())
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(Equal(1.0))
			for _, kind := range []string{"sandbox", "provider", "profile"} {
				Expect(testutil.ToFloat64(m.UnmarkedObjects.WithLabelValues(kind))).To(BeZero(), kind)
			}
		})

		It("counts a sandbox the pass held back or failed on, and keeps the values and sets stale when the list fails", func(ctx SpecContext) {
			gauge := func(kind string) float64 { return testutil.ToFloat64(m.UnmarkedObjects.WithLabelValues(kind)) }
			queueRun(ctx, org, user, agent.ID)
			claimRun(ctx, org)
			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // a run holds the agent: the sandbox stays unmarked
			Expect(gauge("sandbox")).To(Equal(1.0))
			Expect(gauge("provider")).To(Equal(1.0))
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(BeZero())

			osg.FailNext("ListWorkspaces", errors.New("gateway unavailable"))
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(Equal(1.0))
			Expect(gauge("sandbox")).To(Equal(1.0), "the last good value stays")

			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(BeZero())
		})

		It("is not recreated while a run holds the agent", func(ctx SpecContext) {
			queueRun(ctx, org, user, agent.ID)
			claimRun(ctx, org)
			Expect(mk("mark-live", 0).ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Calls("DeleteSandbox")).To(BeZero())
			Expect(osg.Calls("DeleteProvider")).To(BeZero())
			Expect(row(ctx, agent).Message).To(ContainSubstring("after the active and parked runs finish"))
		})

		It("is recreated within the pass cap", func(ctx SpecContext) {
			second := newAgent(ctx, org, user)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed())
			Expect(labels(second)).NotTo(HaveKey(execution.LabelInstall))

			live := mk("mark-live", 1)
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1), "one recreate in a pass")
			msgs := []string{row(ctx, agent).Message, row(ctx, second).Message}
			Expect(msgs).To(ContainElement(ContainSubstring("waiting for a free recreate slot")))
		})
	})
})
