// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

// Install ownership of OpenShell objects (issue 461): two installs share one gateway and one set of
// rows (a restored database next to the live install), and objects made before the install mark
// existed are recreated by the live install.
var _ = Describe("Reconciler install ownership", func() {
	var (
		osg  *fakegateway.Gateway
		org  pgtype.UUID
		user pgtype.UUID
		ws   string
		mk   func(owner string, maxRecreates int) *reconciler.Reconciler
		bk   func(owner string) *openshell.Backend
		m    *reconciler.Metrics
		// secrets are shared by the reconcilers of one spec, as the real ones share the cluster's Secrets:
		// a reconciler of the same install made after another one finds the first one's agent keys.
		secrets *fake.Clientset
		// retention is the home retention of the reconcilers mk makes.
		retention reconciler.Retention
	)
	harnesses := map[string]reconciler.Harness{
		harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
	}
	sandboxOf := func(agent store.Agent) string { return execution.AgentSandboxName(org.Bytes, agent.ID.Bytes) }
	sandboxProviderName := func(agent store.Agent) string {
		return execution.AgentObjectNames(org.Bytes, agent.ID.Bytes).Provider
	}
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
		secrets = fake.NewClientset()
		retention = ""
		osg = fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		m = reconciler.NewMetrics(nil)
		bk = func(owner string) *openshell.Backend {
			b := openshell.New(cc)
			if owner != "" {
				b = b.WithOwner(execution.OwnerID(owner))
			}
			return b
		}
		mk = func(owner string, maxRecreates int) *reconciler.Reconciler {
			b := bk(owner)
			r, err := reconciler.New(pool, b, &fakeModelGateway{}, reconciler.KubeSecrets{Client: secrets, Namespace: namespace}, reconciler.Config{
				KeepWarmPerOrg:      2,
				Prices:              prices.NewResolver(pricing.Embedded()),
				Gateway:             reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
				Keys:                ring,
				ProviderKeys:        reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
				ProviderKeyDir:      "/secrets/providers",
				URLPolicy:           testPolicy,
				Harnesses:           harnesses,
				RuntimeClassName:    "kata",
				MaxRecreatesPerPass: maxRecreates,
				HomeRetention:       retention,
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

		It("is recreated with the label by its owner", func(ctx SpecContext) {
			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // deletes the sandbox
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // creates the sandbox with the label
			Expect(labels(agent)).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
		})

		It("is not recreated while a run holds the agent", func(ctx SpecContext) {
			queueRun(ctx, org, user, agent.ID)
			claimRun(ctx, org)
			Expect(mk("mark-live", 0).ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Calls("DeleteSandbox")).To(BeZero())
			Expect(osg.Provider(ws, sandboxProviderName(agent))).NotTo(BeNil())
			Expect(osg.Provider(ws, sandboxProviderName(agent)).GetMetadata().GetLabels()).NotTo(HaveKey(execution.LabelInstall))
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
