// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		bk   func(owner string) *openshell.Backend
		m    *reconciler.Metrics
		// secrets are shared by the reconcilers of one spec, as the real ones share the cluster's Secrets:
		// a reconciler of the same install made after another one finds the first one's agent keys.
		secrets *fake.Clientset
		// retention is the home retention of the reconcilers mk makes.
		retention reconciler.Retention
	)
	flavors := map[store.AgentFlavor]reconciler.Flavor{
		store.AgentFlavorOpencode: {Image: image, Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
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
				Prices:              prices.NewResolver(pricing.Embedded()),
				Gateway:             reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
				Keys:                ring,
				ProviderKeys:        reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
				ProviderKeyDir:      "/secrets/providers",
				URLPolicy:           testPolicy,
				Flavors:             flavors,
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

		It("is recreated with the label by its owner, and its provider claimed after the sandbox is gone", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // deletes the sandbox; its provider is still attached
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(osg.Provider(ws, names.Provider)).NotTo(BeNil())
			Expect(osg.Provider(ws, names.Provider).GetMetadata().GetLabels()).NotTo(HaveKey(execution.LabelInstall))
			oldID := osg.Provider(ws, names.Provider).GetMetadata().GetId()
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // claims the provider, creates the sandbox
			Expect(labels(agent)).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
			Expect(osg.Provider(ws, names.Provider).GetMetadata().GetLabels()).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
			Expect(osg.Provider(ws, names.Provider).GetMetadata().GetId()).NotTo(Equal(oldID), "the record was made again")
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
				Expect(testutil.ToFloat64(m.UnmarkedWaiting.WithLabelValues(kind))).To(BeZero(), kind)
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

	// The objects of draft and archived agents, and what lists the rest (issue 511).
	Describe("the unmarked list and the claim of draft and archived agents", func() {
		type line struct{ Kind, Name, Reason string }
		list := func(ctx context.Context) []line {
			GinkgoHelper()
			objs, err := reconciler.ListUnmarked(ctx, q, bk("mark-live"), flavors)
			Expect(err).NotTo(HaveOccurred())
			var out []line
			for _, o := range objs {
				if o.Workspace == ws {
					out = append(out, line{o.Kind, o.Name, o.Reason})
				}
			}
			return out
		}
		gauge := func(kind string) float64 { return testutil.ToFloat64(m.UnmarkedObjects.WithLabelValues(kind)) }
		profileID := "koinon-agentgateway-opencode"

		It("gives every unmarked object its reason, and lists nothing of another install", func(ctx SpecContext) {
			active, draft, archived := newAgent(ctx, org, user), newAgent(ctx, org, user), newAgent(ctx, org, user)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed()) // made before the install mark
			_, err := q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: org, ID: draft.ID})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: archived.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed()) // stops the draft's sandbox, cleans up the archived agent

			// A sandbox nobody labeled, in a workspace of this database.
			_, err = bk("").CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "ka-handmade", Image: image})
			Expect(err).NotTo(HaveOccurred())
			// Objects of a workspace no org owns.
			orphanWS := execution.WorkspaceName([16]byte{9, 9, 9})
			_, err = bk("").EnsureWorkspace(ctx, orphanWS, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = bk("").CreateSandbox(ctx, orphanWS, execution.SandboxSpec{Name: "ka-orphan", Image: image})
			Expect(err).NotTo(HaveOccurred())
			// Another install's object in this database's workspace.
			theirs := reconciler.GatewayProfile(store.AgentFlavorOpencode, reconciler.Flavor{Binaries: []string{"/bin/x"}}, reconciler.Endpoint{Host: "h", Port: 1}, false)
			theirs.ID = "koinon-theirs"
			Expect(bk("mark-other").EnsureProvider(ctx, ws, execution.Provider{
				Name: "kp-theirs", Profile: theirs,
				Credentials: map[string]string{reconciler.GatewayKeyEnvVar: "koinon_test_not-a-real-token"},
			})).To(Succeed())

			names := func(a store.Agent) execution.AgentNames { return execution.AgentObjectNames(org.Bytes, a.ID.Bytes) }
			objs, err := reconciler.ListUnmarked(ctx, q, bk("mark-live"), flavors)
			Expect(err).NotTo(HaveOccurred())
			got := map[string]string{}
			for _, o := range objs {
				got[o.Workspace+"/"+o.Kind+"/"+o.Name] = o.Reason
			}
			Expect(got).To(HaveKeyWithValue(ws+"/sandbox/"+names(active).Sandbox, reconciler.ReasonPendingClaim))
			Expect(got).To(HaveKeyWithValue(ws+"/provider/"+names(active).Provider, reconciler.ReasonPendingClaim))
			Expect(got).To(HaveKeyWithValue(ws+"/sandbox/"+names(draft).Sandbox, reconciler.ReasonWaitsForAgent))
			Expect(got).To(HaveKeyWithValue(ws+"/provider/"+names(draft).Provider, reconciler.ReasonWaitsForAgent))
			Expect(got).To(HaveKeyWithValue(ws+"/profile/"+profileID, reconciler.ReasonPendingClaim), "an active agent of the flavor imports it")
			Expect(got).To(HaveKeyWithValue(ws+"/sandbox/ka-handmade", reconciler.ReasonNotReconcilerMade))
			Expect(got).To(HaveKeyWithValue(orphanWS+"/sandbox/ka-orphan", reconciler.ReasonOrgNotInDB))
			Expect(got).NotTo(HaveKey(ws + "/provider/kp-theirs"))
			Expect(got).NotTo(HaveKey(ws + "/profile/koinon-theirs"))
			for k, reason := range got {
				Expect(reconciler.UnmarkedReasons).To(ContainElement(reason), k)
			}
			Expect(got).NotTo(HaveKey(ContainSubstring(names(archived).Provider)), "the archived agent's objects are gone")

			By("listing only reads")
			creates, deletes := osg.Calls("CreateProvider")+osg.Calls("CreateSandbox"), osg.Calls("DeleteProvider")+osg.Calls("DeleteSandbox")
			_ = list(ctx)
			Expect(osg.Calls("CreateProvider") + osg.Calls("CreateSandbox")).To(Equal(creates))
			Expect(osg.Calls("DeleteProvider") + osg.Calls("DeleteSandbox")).To(Equal(deletes))
			_, err = reconciler.ListUnmarked(ctx, q, bk(""), flavors)
			Expect(err).To(HaveOccurred(), "without an install mark nothing is safe to say")
		})

		It("claims the provider and profile of a draft agent with no sandbox", func(ctx SpecContext) {
			agent := newOnDemandAgent(ctx, org, user, `{"instructions":"help"}`)
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed()) // provider and profile, no sandbox (on demand)
			Expect(osg.Sandbox(ws, names.Sandbox)).To(BeNil())
			// The agent had a GitHub grant while it was active: its provider and profile are still there.
			Expect(bk("").EnsureProvider(ctx, ws, execution.Provider{
				Name:        names.GitHubProvider,
				Profile:     reconciler.GitHubProfile(names.GitHubProfile, reconciler.GitHubRead{Provider: "GitHub"}, reconciler.DefaultGitBinaries),
				Credentials: map[string]string{"GH_TOKEN": "koinon_test_not-a-real-token"},
			})).To(Succeed())
			Expect(osg.Provider(ws, names.GitHubProvider)).NotTo(BeNil())
			Expect(osg.Profile(ws, names.GitHubProfile)).NotTo(BeNil())
			_, err := q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			keyBefore := row(ctx, agent).GatewayKeyID

			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(list(ctx)).To(BeEmpty())
			Expect(osg.Provider(ws, names.GitHubProvider)).To(BeNil(), "a draft agent with no sandbox keeps no GitHub provider")
			Expect(osg.Profile(ws, names.GitHubProfile)).To(BeNil(), "nor a GitHub profile")
			Expect(osg.Provider(ws, names.Provider).GetMetadata().GetLabels()).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
			Expect(osg.Profile(ws, profileID).GetAnnotations()).To(HaveKey(execution.LabelInstall))
			Expect(row(ctx, agent).GatewayKeyID).To(Equal(keyBefore), "the key survives for re-activation, none was issued")

			By("the gauge reaches 0 and stays there")
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			for _, kind := range []string{"sandbox", "provider", "profile"} {
				Expect(gauge(kind)).To(BeZero(), kind)
			}
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(BeZero())
			id := osg.Provider(ws, names.Provider).GetMetadata().GetId()
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Provider(ws, names.Provider).GetMetadata().GetId()).To(Equal(id), "a marked provider is not recreated")
		})

		It("marks the flavor profile of a draft agent whose key Secret was deleted, and leaves its provider", func(ctx SpecContext) {
			agent := newOnDemandAgent(ctx, org, user, `{"instructions":"help"}`)
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed()) // provider and profile, no sandbox (on demand)
			_, err := q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(secrets.CoreV1().Secrets(namespace).Delete(ctx, row(ctx, agent).KeySecretName, metav1.DeleteOptions{})).To(Succeed())

			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Profile(ws, profileID).GetAnnotations()).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")),
				"the org pass marks the profile; it needs no key")
			Expect(list(ctx)).To(ConsistOf(line{"provider", names.Provider, reconciler.ReasonDraftAgent}),
				"without the key material the provider is not touched")
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(gauge("profile")).To(BeZero())
		})

		It("marks the profile of a draft agent whose stopped sandbox still attaches the provider, and the provider on re-activation", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed())
			// The agent had a GitHub grant while it was active: its provider and profile are still there.
			Expect(bk("").EnsureProvider(ctx, ws, execution.Provider{
				Name:        names.GitHubProvider,
				Profile:     reconciler.GitHubProfile(names.GitHubProfile, reconciler.GitHubRead{Provider: "GitHub"}, reconciler.DefaultGitBinaries),
				Labels:      map[string]string{reconciler.LabelManagedBy: reconciler.ManagedByValue},
				Credentials: map[string]string{"GH_TOKEN": "koinon_test_not-a-real-token"},
			})).To(Succeed())
			_, err := q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			live := mk("mark-live", 0)
			deletes := osg.Calls("DeleteProvider")
			Expect(live.ReconcileAll(ctx)).To(Succeed()) // stops the sandbox
			Expect(osg.Sandbox(ws, names.Sandbox)).NotTo(BeNil())
			Expect(osg.Calls("DeleteProvider")).To(Equal(deletes), "an attached provider is not deleted")
			Expect(list(ctx)).To(ConsistOf(line{"sandbox", names.Sandbox, reconciler.ReasonWaitsForAgent}, line{"provider", names.Provider, reconciler.ReasonWaitsForAgent},
				line{"provider", names.GitHubProvider, reconciler.ReasonWaitsForAgent}, line{"profile", names.GitHubProfile, reconciler.ReasonWaitsForAgent}))

			version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
				OrgID: org, AgentID: agent.ID, Config: []byte(`{"instructions":"help"}`), CreatedBy: user,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user})
			Expect(err).NotTo(HaveOccurred())
			for range 3 {
				Expect(live.ReconcileAll(ctx)).To(Succeed())
			}
			Expect(list(ctx)).To(BeEmpty())
		})

		It("marks the flavor profile of an archived agent and does not delete it", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed())
			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(list(ctx)).To(ContainElement(line{"profile", profileID, reconciler.ReasonArchivedAgent}))

			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Provider(ws, names.Provider)).To(BeNil(), "the archive deletes the agent's provider")
			Expect(osg.Profile(ws, profileID)).NotTo(BeNil(), "the shared flavor profile stays")
			Expect(osg.Profile(ws, profileID).GetAnnotations()).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
			Expect(list(ctx)).To(BeEmpty())
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(gauge("profile")).To(BeZero())
			Expect(gauge("provider")).To(BeZero())
			Expect(gauge("sandbox")).To(BeZero())
		})

		It("marks the flavor profile of an agent archived and cleaned up before the mark was on", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed()) // made before the install mark
			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed()) // cleans up; the row is 'deleted' and leaves the list
			Expect(row(ctx, agent).State).To(Equal(store.SandboxStateDeleted))
			Expect(osg.Provider(ws, names.Provider)).To(BeNil())
			Expect(list(ctx)).To(ContainElement(line{"profile", profileID, reconciler.ReasonArchivedAgent}))

			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Profile(ws, profileID).GetAnnotations()).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
			Expect(list(ctx)).To(BeEmpty())
			gets := osg.Calls("GetProviderProfile")
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Calls("GetProviderProfile")).To(Equal(gets), "a profile found marked is not asked for again")
			for _, kind := range []string{"sandbox", "provider", "profile"} {
				Expect(gauge(kind)).To(BeZero(), kind)
			}
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(BeZero())
		})

		It("keeps the flavor profile of an archived agent for an active agent of the same flavor", func(ctx SpecContext) {
			keep, gone := newAgent(ctx, org, user), newAgent(ctx, org, user)
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed())
			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: gone.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(mk("mark-live", 0).ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Profile(ws, profileID)).NotTo(BeNil())
			Expect(osg.Provider(ws, execution.AgentObjectNames(org.Bytes, keep.ID.Bytes).Provider)).NotTo(BeNil())
		})

		It("does not create a profile for an archived agent when there is none", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			Expect(mk("mark-live", 0).ReconcileAll(ctx)).To(Succeed())
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			Expect(bk("mark-live").DeleteSandbox(ctx, ws, names.Sandbox)).To(Succeed())
			Expect(bk("mark-live").DeleteProvider(ctx, ws, names.Provider)).To(Succeed())
			Expect(bk("mark-live").DeleteProviderProfile(ctx, ws, profileID)).To(Succeed())
			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Profile(ws, profileID)).To(BeNil())
			gets := osg.Calls("GetProviderProfile")
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(osg.Calls("GetProviderProfile")).To(Equal(gets), "a profile found missing is not asked for again")
			Expect(osg.Profile(ws, profileID)).To(BeNil())
		})

		It("says pending-claim for the profile of a configured flavor no agent of the org has, not-reconciler-made otherwise", func(ctx SpecContext) {
			_, err := bk("").EnsureWorkspace(ctx, ws, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(bk("").EnsureProvider(ctx, ws, execution.Provider{
				Name: "kp-handmade", Profile: reconciler.GatewayProfile(store.AgentFlavorOpencode, flavors[store.AgentFlavorOpencode], reconciler.Endpoint{Host: "h", Port: 1}, false),
				Credentials: map[string]string{reconciler.GatewayKeyEnvVar: "koinon_test_not-a-real-token"},
			})).To(Succeed())
			reasonOf := func(configured map[store.AgentFlavor]reconciler.Flavor) string {
				GinkgoHelper()
				objs, err := reconciler.ListUnmarked(ctx, q, bk("mark-live"), configured)
				Expect(err).NotTo(HaveOccurred())
				for _, o := range objs {
					if o.Workspace == ws && o.Kind == "profile" && o.Name == profileID {
						return o.Reason
					}
				}
				Fail("the profile is not listed")
				return ""
			}
			Expect(reasonOf(flavors)).To(Equal(reconciler.ReasonPendingClaim), "the org pass marks it")
			Expect(reasonOf(nil)).To(Equal(reconciler.ReasonNotReconcilerMade), "an unconfigured flavor")
			Expect(reasonOf(map[store.AgentFlavor]reconciler.Flavor{store.AgentFlavorOpencode: {}})).To(Equal(reconciler.ReasonNotReconcilerMade),
				"a flavor without an image is not rendered")
		})

		It("lists a draft agent's stopped sandbox and an archived agent's retained sandbox as waits-for-agent, and the rest reaches 0", func(ctx SpecContext) {
			draft, archived := newAgent(ctx, org, user), newAgent(ctx, org, user)
			retention = reconciler.RetentionRetain
			for range 2 { // old objects, no install mark; every sandbox is created and ready
				Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed())
			}
			Expect(osg.Sandbox(ws, execution.AgentObjectNames(org.Bytes, archived.ID.Bytes).Sandbox)).NotTo(BeNil())
			_, err := q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: org, ID: draft.ID})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: archived.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(mk("", 0).ReconcileAll(ctx)).To(Succeed()) // stops the draft's sandbox, retains the archived agent's
			Expect(row(ctx, archived).State).To(Equal(store.SandboxStateRetained))

			live := mk("mark-live", 0)
			for range 3 {
				Expect(live.ReconcileAll(ctx)).To(Succeed())
			}
			dn, an := execution.AgentObjectNames(org.Bytes, draft.ID.Bytes), execution.AgentObjectNames(org.Bytes, archived.ID.Bytes)
			Expect(list(ctx)).To(ConsistOf(
				line{"sandbox", dn.Sandbox, reconciler.ReasonWaitsForAgent},
				line{"provider", dn.Provider, reconciler.ReasonWaitsForAgent},
				line{"sandbox", an.Sandbox, reconciler.ReasonWaitsForAgent},
			))
			waiting := func(kind string) float64 { return testutil.ToFloat64(m.UnmarkedWaiting.WithLabelValues(kind)) }
			for _, kind := range []string{"sandbox", "provider", "profile"} {
				Expect(gauge(kind)).To(BeZero(), "the drill gauge leaves out what waits for an agent: %s", kind)
			}
			Expect(waiting("sandbox")).To(Equal(2.0), "the second gauge counts what waits for an agent")
			Expect(waiting("provider")).To(Equal(1.0))
			Expect(waiting("profile")).To(BeZero())
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(BeZero())

			By("activating the draft agent and unarchiving the archived one clears it")
			version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
				OrgID: org, AgentID: draft.ID, Config: []byte(`{"instructions":"help"}`), CreatedBy: user,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: draft.ID, VersionID: version.ID, ActivatedBy: user})
			Expect(err).NotTo(HaveOccurred())
			Expect(bk("mark-live").DeleteSandbox(ctx, ws, an.Sandbox)).To(Succeed(), "or the operator deletes the retained sandbox by hand")
			for range 3 {
				Expect(live.ReconcileAll(ctx)).To(Succeed())
			}
			Expect(list(ctx)).To(BeEmpty())
			for _, kind := range []string{"sandbox", "provider", "profile"} {
				Expect(waiting(kind)).To(BeZero(), kind)
			}
		})

		It("reaches 0 on an install with no orphans", func(ctx SpecContext) {
			Expect(newAgent(ctx, org, user).ID.Valid).To(BeTrue())
			live := mk("mark-live", 0)
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(live.ReconcileAll(ctx)).To(Succeed())
			Expect(list(ctx)).To(BeEmpty())
			for _, kind := range []string{"sandbox", "provider", "profile"} {
				Expect(gauge(kind)).To(BeZero(), kind)
			}
			Expect(testutil.ToFloat64(m.UnmarkedStale)).To(BeZero())
		})
	})
})
