// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/reconciler"
)

const (
	namespace = "koinon"
	image     = "ghcr.io/stateracloud/flavor-opencode:test"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

var _ = Describe("names", func() {
	It("derives short, distinct DNS names from the IDs", func() {
		seen := map[string]bool{}
		for i := range 500 {
			var org, agent pgtype.UUID
			org.Valid, agent.Valid = true, true
			org.Bytes[0], org.Bytes[15] = byte(i), byte(i>>8)
			agent.Bytes[3], agent.Bytes[9] = byte(i*7), byte(i)
			n := execution.AgentObjectNames(org.Bytes, agent.Bytes)
			for _, name := range []string{n.Sandbox, n.Provider} {
				Expect(len(name)).To(BeNumerically("<=", execution.MaxSandboxName), name)
				Expect(name).To(MatchRegexp(dnsLabel.String()))
			}
			Expect(n.KeySecret).To(MatchRegexp(dnsLabel.String()))
			Expect(execution.WorkspaceName(org.Bytes)).To(MatchRegexp(dnsLabel.String()))
			Expect(seen[n.Sandbox]).To(BeFalse())
			seen[n.Sandbox] = true
			Expect(execution.AgentObjectNames(org.Bytes, agent.Bytes)).To(Equal(n), "names are stable")
		}
	})
})

var _ = Describe("Reconciler", func() {
	var (
		osg     *fakegateway.Gateway
		backend *openshell.Backend
		mg      *fakeModelGateway
		kube    *fake.Clientset
		logs    *syncBuffer
		now     atomic.Pointer[time.Time]
		cfg     reconciler.Config
		org     pgtype.UUID
		user    pgtype.UUID
	)

	newReconciler := func() *reconciler.Reconciler {
		r, err := reconciler.New(pool, backend, mg,
			reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	pass := func(ctx context.Context, r *reconciler.Reconciler) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}
	advance := func(d time.Duration) {
		t := time.Now().Add(d)
		now.Store(&t)
	}
	sandboxRow := func(ctx context.Context, agent pgtype.UUID) store.AgentSandbox {
		GinkgoHelper()
		row, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent})
		Expect(err).NotTo(HaveOccurred())
		return row
	}
	material := func(ctx context.Context, row store.AgentSandbox) string {
		GinkgoHelper()
		s, err := kube.CoreV1().Secrets(namespace).Get(ctx, row.KeySecretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return string(s.Data["api_key"])
	}

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		backend = openshell.New(conn)
		mg = &fakeModelGateway{}
		kube = fake.NewClientset()
		logs = &syncBuffer{}
		now.Store(nil)
		cfg = reconciler.Config{
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			URLPolicy:      testPolicy,
			Flavors: map[store.AgentFlavor]reconciler.Flavor{
				store.AgentFlavorOpencode: {Image: image, Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
			},
			RuntimeClassName: "kata",
			Logger:           slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Now: func() time.Time {
				if t := now.Load(); t != nil {
					return *t
				}
				return time.Now()
			},
		}
		org, user = newOrg(ctx)
		addProvider(ctx, org, user, "upstream", "", "fake")
	})

	DescribeTable("lets only the flavor's network-calling binaries use the agentgateway credential",
		func(ctx SpecContext, flavor store.AgentFlavor, binaries []string) {
			Expect(reconciler.DefaultBinaries[flavor]).To(Equal(binaries))
			cfg.Flavors = map[store.AgentFlavor]reconciler.Flavor{
				flavor: {Image: image, Binaries: reconciler.DefaultBinaries[flavor]},
			}
			agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
				OrgID: org, DisplayName: "Flavor binaries", Flavor: flavor, CreatedBy: user,
			})
			Expect(err).NotTo(HaveOccurred())
			version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
				OrgID: org, AgentID: agent.ID, Config: []byte(`{"instructions":"help"}`), CreatedBy: user,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{
				OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user,
			})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, newReconciler())

			row := sandboxRow(ctx, agent.ID)
			Expect(row.State).To(Equal(store.SandboxStateReady))
			profile := osg.Profile(row.Workspace, "koinon-agentgateway-"+string(flavor))
			Expect(profile).NotTo(BeNil())
			var paths []string
			for _, b := range profile.GetBinaries() {
				paths = append(paths, b.GetPath())
			}
			Expect(paths).To(Equal(binaries))
		},
		Entry("OpenCode: its single binary", store.AgentFlavorOpencode, []string{"/usr/local/bin/opencode"}),
	)

	Describe("an active agent", func() {
		var (
			r     *reconciler.Reconciler
			agent store.Agent
			row   store.AgentSandbox
		)
		BeforeEach(func(ctx SpecContext) {
			agent = newAgent(ctx, org, user)
			r = newReconciler()
			pass(ctx, r)
			row = sandboxRow(ctx, agent.ID)
		})

		It("gets a workspace, a key, a provider record and a sandbox", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			ws := execution.WorkspaceName(org.Bytes)
			Expect(row.Workspace).To(Equal(ws))
			Expect(row.SandboxName).To(Equal(names.Sandbox))
			Expect(len(row.SandboxName)).To(BeNumerically("<=", execution.MaxSandboxName))
			Expect(row.State).To(Equal(store.SandboxStateReady))
			Expect(row.StartedAt.Valid).To(BeTrue())

			By("the workspace is recorded")
			w, err := q.GetOrgWorkspace(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(w.Workspace).To(Equal(ws))
			Expect(w.WorkspaceID).NotTo(BeEmpty())

			By("the key material is in a labelled Secret, only its hash in Postgres and the gateway")
			key := material(ctx, row)
			Expect(key).To(HavePrefix(gateway.KeyPrefix))
			Expect(row.GatewayKeyHash.String).To(Equal(gateway.HashKey(key)))
			secret, err := kube.CoreV1().Secrets(namespace).Get(ctx, row.KeySecretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(secret.Labels).To(HaveKeyWithValue(reconciler.LabelManagedBy, reconciler.ManagedByValue))
			Expect(secret.Labels).To(HaveKey(reconciler.LabelAgent))
			state := mg.last()
			Expect(state.Keys).To(ConsistOf(gateway.Key{
				ID: row.GatewayKeyID.String, Hash: row.GatewayKeyHash.String,
				Agent: gateway.AgentRef{Org: state.Keys[0].Agent.Org, Name: state.Keys[0].Agent.Name},
			}))
			Expect(state.Providers).To(HaveLen(1))
			Expect(state.Providers[0].Models).To(HaveLen(1))

			By("the provider record holds the key behind a stable placeholder")
			p := osg.Provider(ws, names.Provider)
			Expect(p).NotTo(BeNil())
			Expect(p.GetCredentials()).To(HaveKeyWithValue("OPENAI_API_KEY", key))
			Expect(p.GetMetadata().GetLabels()).To(HaveKeyWithValue(reconciler.LabelManagedBy, reconciler.ManagedByValue))
			profile := osg.Profile(ws, "koinon-agentgateway-opencode")
			Expect(profile.GetCredentials()).To(HaveLen(1))
			Expect(profile.GetCredentials()[0].GetStablePlaceholder()).To(BeTrue())
			Expect(profile.GetEndpoints()).To(HaveLen(1))
			ep := profile.GetEndpoints()[0]
			Expect(ep.GetHost()).To(Equal("koinon-gateway.koinon.svc"))
			Expect(ep.GetPort()).To(BeEquivalentTo(8080))
			Expect(ep.GetEnforcement()).To(Equal(sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE))
			Expect(profile.GetBinaries()[0].GetPath()).To(Equal("/usr/local/bin/opencode"))

			By("the sandbox runs the flavor image under an enforced policy")
			sb := osg.Sandbox(ws, names.Sandbox)
			Expect(sb).NotTo(BeNil())
			Expect(sb.GetSpec().GetTemplate().GetImage()).To(Equal(image))
			Expect(sb.GetSpec().GetTemplate().GetRuntimeClassName()).To(Equal("kata"))
			Expect(sb.GetSpec().GetProviders()).To(Equal([]string{names.Provider}))
			Expect(sb.GetMetadata().GetLabels()).To(HaveKeyWithValue(reconciler.LabelManagedBy, reconciler.ManagedByValue))
			Expect(sb.GetMetadata().GetLabels()).To(HaveKey(reconciler.LabelSpecHash))
			Expect(sb.GetSpec().GetPolicy().GetLandlock().GetCompatibility()).To(Equal("hard_requirement"))
		})

		It("keeps key material out of the sandbox spec, the profile, the gateway state and the logs", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			key := material(ctx, row)

			sb := osg.Sandbox(row.Workspace, names.Sandbox)
			Expect(sb.GetSpec().GetTemplate().GetEnvironment()).To(BeEmpty())
			Expect(sb.GetSpec().GetEnvironment()).To(BeEmpty())
			spec, err := protojson.Marshal(sb)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(spec)).NotTo(ContainSubstring(key))
			Expect(string(spec)).NotTo(ContainSubstring(gateway.KeyPrefix))
			for _, p := range osg.Policies(row.Workspace, names.Sandbox) {
				b, err := protojson.Marshal(p)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(b)).NotTo(ContainSubstring(key))
			}
			profile, err := protojson.Marshal(osg.Profile(row.Workspace, "koinon-agentgateway-opencode"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(profile)).NotTo(ContainSubstring(key))
			state := fmt.Sprintf("%+v", mg.applied)
			Expect(state).NotTo(ContainSubstring(key))
			Expect(logs.String()).To(ContainSubstring("issued agentgateway key"))
			Expect(logs.String()).NotTo(ContainSubstring(key))
			Expect(logs.String()).NotTo(ContainSubstring(gateway.KeyPrefix))
		})

		It("ensures each object once and is idempotent on later passes", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			for range 3 {
				pass(ctx, r)
			}
			Expect(osg.Calls("CreateWorkspace")).To(Equal(1))
			Expect(osg.Calls("ImportProviderProfiles")).To(Equal(1))
			Expect(osg.Calls("UpdateProviderProfiles")).To(Equal(0))
			Expect(osg.Calls("CreateProvider")).To(Equal(1))
			Expect(osg.Calls("UpdateProvider")).To(Equal(0))
			Expect(osg.Calls("CreateSandbox")).To(Equal(1))
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			Expect(osg.Calls("StopSandbox")).To(Equal(0))
			Expect(osg.Policies(row.Workspace, names.Sandbox)).To(HaveLen(1))
			Expect(mg.issuedCount()).To(Equal(1))
			Expect(sandboxRow(ctx, agent.ID).GatewayKeyID).To(Equal(row.GatewayKeyID))
		})

		It("reverts drift", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			ws := row.Workspace

			By("a hand-added network rule is removed again")
			_, err := backend.EnsurePolicy(ctx, ws, names.Sandbox, execution.Policy{
				Filesystem: execution.FilesystemPolicy{
					IncludeWorkdir: true,
					ReadOnly:       []string{"/bin", "/usr", "/lib", "/lib64", "/opt", "/proc", "/dev/urandom", "/etc", "/var/log"},
					ReadWrite:      []string{"/tmp", "/dev/null"},
				},
				Network: map[string]execution.NetworkRule{"exfil": {
					Endpoints: []execution.Endpoint{{Host: "evil.example", Port: 443}},
					Binaries:  []string{"/usr/bin/curl"},
				}},
			})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r)
			revs := osg.Policies(ws, names.Sandbox)
			Expect(revs[len(revs)-1].GetNetworkPolicies()).To(BeEmpty())

			By("a deleted provider record is recreated with the current key")
			Expect(backend.DeleteProvider(ctx, ws, names.Provider)).To(Succeed())
			pass(ctx, r)
			Expect(osg.Provider(ws, names.Provider).GetCredentials()).To(HaveKeyWithValue("OPENAI_API_KEY", material(ctx, row)))

			By("a deleted key Secret rotates the key without recreating the sandbox")
			Expect(kube.CoreV1().Secrets(namespace).Delete(ctx, row.KeySecretName, metav1.DeleteOptions{})).To(Succeed())
			pass(ctx, r)
			rotated := sandboxRow(ctx, agent.ID)
			Expect(rotated.GatewayKeyID).NotTo(Equal(row.GatewayKeyID))
			newKey := material(ctx, rotated)
			Expect(rotated.GatewayKeyHash.String).To(Equal(gateway.HashKey(newKey)))
			Expect(osg.Provider(ws, names.Provider).GetCredentials()).To(HaveKeyWithValue("OPENAI_API_KEY", newKey))
			Expect(mg.last().Keys).To(HaveLen(1))
			Expect(mg.last().Keys[0].ID).To(Equal(rotated.GatewayKeyID.String))
			Expect(osg.Calls("CreateSandbox")).To(Equal(1))

			By("a deleted sandbox is recreated")
			Expect(backend.DeleteSandbox(ctx, ws, names.Sandbox)).To(Succeed())
			pass(ctx, r)
			Expect(osg.Sandbox(ws, names.Sandbox)).NotTo(BeNil())
			Expect(osg.Calls("CreateSandbox")).To(Equal(2))
		})

		It("recreates the sandbox when its image changes, but not during a run", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			cfg.Flavors[store.AgentFlavorOpencode] = reconciler.Flavor{
				Image: image + "-2", Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode],
			}
			r = newReconciler()

			queueRun(ctx, org, user, agent.ID)
			run := claimRun(ctx, org)
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			Expect(sandboxRow(ctx, agent.ID).Message).To(ContainSubstring("after the active and parked runs finish"))

			_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: org, ID: run.ID, Epoch: run.Epoch, Outcome: store.RunStateSucceeded})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r) // deletes
			pass(ctx, r) // creates
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(osg.Sandbox(row.Workspace, names.Sandbox).GetSpec().GetTemplate().GetImage()).To(Equal(image + "-2"))
			Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateReady))
		})

		It("records the sandbox's OpenShell ID as its home generation, new for a recreated sandbox", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			id := osg.Sandbox(row.Workspace, names.Sandbox).GetMetadata().GetId()
			Expect(id).NotTo(BeEmpty())
			Expect(row.HomeGeneration.String).To(Equal(id))

			By("stop and start keep the home, and so the generation")
			advance(11 * time.Minute)
			pass(ctx, r)
			Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateStopped))
			now.Store(nil)
			queueRun(ctx, org, user, agent.ID)
			pass(ctx, r)
			Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateReady))
			Expect(sandboxRow(ctx, agent.ID).HomeGeneration.String).To(Equal(id))

			By("a recreated sandbox has a new home")
			Expect(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox)).To(Succeed())
			pass(ctx, r)
			recreated := sandboxRow(ctx, agent.ID)
			Expect(recreated.HomeGeneration.String).To(Equal(osg.Sandbox(row.Workspace, names.Sandbox).GetMetadata().GetId()))
			Expect(recreated.HomeGeneration.String).NotTo(Equal(id))
		})

		It("does not recreate the sandbox while a run is parked, and does once it has finished", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			queueRun(ctx, org, user, agent.ID)
			run := claimRun(ctx, org)
			run, err := q.ParkRun(ctx, store.ParkRunParams{
				OrgID: org, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonApproval,
			})
			Expect(err).NotTo(HaveOccurred())

			cfg.Flavors[store.AgentFlavorOpencode] = reconciler.Flavor{
				Image: image + "-2", Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode],
			}
			r = newReconciler()
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			Expect(sandboxRow(ctx, agent.ID).Message).To(Equal(
				"sandbox spec changed; recreating it after the active and parked runs finish"))

			By("a resolved park still waits for the resumed run")
			_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: org, ID: run.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			resumed := claimRun(ctx, org)
			Expect(resumed.ID).To(Equal(run.ID))
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))

			By("the resumed run finishing lets the recreation proceed")
			_, err = q.FinishRun(ctx, store.FinishRunParams{OrgID: org, ID: resumed.ID, Epoch: resumed.Epoch, Outcome: store.RunStateSucceeded})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r) // deletes
			pass(ctx, r) // creates
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(osg.Sandbox(row.Workspace, names.Sandbox).GetSpec().GetTemplate().GetImage()).To(Equal(image + "-2"))
		})

		It("recreates a sandbox agentd found stuck at once, parked run or not, audits it, and clears the mark with the new home", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			old := osg.Sandbox(row.Workspace, names.Sandbox).GetMetadata().GetId()
			queueRun(ctx, org, user, agent.ID)
			run := claimRun(ctx, org)
			_, err := q.ParkRun(ctx, store.ParkRunParams{OrgID: org, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonApproval})
			Expect(err).NotTo(HaveOccurred())

			By("a healthy sandbox with a parked run is left alone")
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))

			By("agentd marks the stuck one")
			_, err = q.MarkSandboxUnhealthy(ctx, store.MarkSandboxUnhealthyParams{
				OrgID: org, AgentID: agent.ID, Reason: "a process could not be ended for 1m0s: worker (pid 41, state D)", Generation: old,
			})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r) // deletes
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1), "the parked run does not hold it off")
			Expect(sandboxRow(ctx, agent.ID).UnhealthyAt.Valid).To(BeTrue(), "still marked while it is being deleted")
			pass(ctx, r) // creates
			recreated := sandboxRow(ctx, agent.ID)
			Expect(recreated.HomeGeneration.String).NotTo(Equal(old), "a new home")
			Expect(recreated.UnhealthyAt.Valid).To(BeFalse())
			Expect(recreated.State).To(Equal(store.SandboxStateReady))
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'sandbox.recreate' AND summary::text LIKE '%state D%'`, org).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))

			By("the parked run is still parked in Postgres")
			parked, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: run.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(parked.State).To(Equal(store.RunStateParked))

			By("a mark for a sandbox that is already gone does not recreate the new one")
			_, err = q.MarkSandboxUnhealthy(ctx, store.MarkSandboxUnhealthyParams{OrgID: org, AgentID: agent.ID, Reason: "old", Generation: old})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(sandboxRow(ctx, agent.ID).UnhealthyAt.Valid).To(BeFalse(), "recording the new home clears the stale mark")
		})

		Describe("a sandbox in Error or Completed", func() {
			var names execution.AgentNames
			fail := func(phase openshellv1.SandboxPhase) {
				osg.SetPhase(row.Workspace, names.Sandbox, phase)
			}
			recreations := func(ctx context.Context) int {
				var n int
				Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'sandbox.recreate'`, org).Scan(&n)).To(Succeed())
				return n
			}
			BeforeEach(func() { names = execution.AgentObjectNames(org.Bytes, agent.ID.Bytes) })

			DescribeTable("is deleted and recreated with an audit event once no run is active",
				func(ctx SpecContext, phase openshellv1.SandboxPhase, name string) {
					old := sandboxRow(ctx, agent.ID).HomeGeneration.String
					fail(phase)
					pass(ctx, r) // deletes
					Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
					Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateDeleting))
					Expect(sandboxRow(ctx, agent.ID).Message).To(ContainSubstring("recreating a failed sandbox (phase " + name))
					pass(ctx, r) // creates
					recreated := sandboxRow(ctx, agent.ID)
					Expect(recreated.State).To(Equal(store.SandboxStateReady))
					Expect(recreated.HomeGeneration.String).NotTo(Equal(old))
					Expect(recreations(ctx)).To(Equal(1))
					var summary string
					Expect(pool.QueryRow(ctx, `SELECT summary::text FROM audit_events WHERE org_id = $1 AND action = 'sandbox.recreate'`, org).Scan(&summary)).To(Succeed())
					Expect(summary).To(ContainSubstring(`"phase": "` + name + `"`))
				},
				Entry("Error", openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR, "Error"),
				Entry("Completed", openshellv1.SandboxPhase_SANDBOX_PHASE_COMPLETED, "Completed"),
			)

			It("is repaired under a starting run, which waits for the new sandbox", func(ctx SpecContext) {
				queueRun(ctx, org, user, agent.ID)
				run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "agentd-test", LeaseSeconds: 60})
				Expect(err).NotTo(HaveOccurred())
				Expect(run.State).To(Equal(store.RunStateStarting))
				fail(openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(1), "repaired in the same pass")
				Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateDeleting))
				pass(ctx, r)
				Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateReady))
				got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: run.ID})
				Expect(err).NotTo(HaveOccurred())
				Expect(got.State).To(Equal(store.RunStateStarting))
			})

			It("is left alone while a run is running, and repaired once it has finished", func(ctx SpecContext) {
				queueRun(ctx, org, user, agent.ID)
				run := claimRun(ctx, org)
				fail(openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
				Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateError))

				_, err := q.FinishRun(ctx, store.FinishRunParams{
					OrgID: org, ID: run.ID, Epoch: run.Epoch, Outcome: store.RunStateFailed,
					OutcomeReason: pgtype.Text{String: "sandbox error", Valid: true},
				})
				Expect(err).NotTo(HaveOccurred())
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			})

			It("shares one budget of three with stuck recreations, for the reconciler and for agentd", func(ctx SpecContext) {
				count := func() int64 {
					n, err := q.CountRecentSandboxRecreations(ctx, store.CountRecentSandboxRecreationsParams{OrgID: org, AgentID: agent.ID})
					Expect(err).NotTo(HaveOccurred())
					return n
				}
				for range 2 {
					fail(openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
					pass(ctx, r) // deletes
					pass(ctx, r) // creates
				}
				Expect(count()).To(BeEquivalentTo(2))

				By("agentd finds a sandbox stuck: its incident and the recreation count once")
				run := queueRun(ctx, org, user, agent.ID)
				_, err := q.InsertSandboxIncident(ctx, store.InsertSandboxIncidentParams{
					OrgID: org, AgentID: agent.ID, RunID: run.ID, TaskID: run.TaskID,
					Processes: []byte(`[]`), Outcome: "recreate", Work: "nothing",
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = q.MarkSandboxUnhealthy(ctx, store.MarkSandboxUnhealthyParams{
					OrgID: org, AgentID: agent.ID, Reason: "stuck", Generation: sandboxRow(ctx, agent.ID).HomeGeneration.String,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(count()).To(BeEquivalentTo(3), "agentd's guard now refuses a further recreation")
				pass(ctx, r) // deletes the stuck one
				pass(ctx, r) // creates
				Expect(count()).To(BeEquivalentTo(3), "the stuck recreation's audit event is not counted again")
				Expect(osg.Calls("DeleteSandbox")).To(Equal(3))

				By("and the reconciler's guard refuses the next failure")
				_, err = pool.Exec(ctx, `UPDATE runs SET state = 'canceled', finished_at = now(), ready_at = NULL WHERE org_id = $1 AND id = $2`, org, run.ID)
				Expect(err).NotTo(HaveOccurred())
				fail(openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(3))
				a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
				Expect(err).NotTo(HaveOccurred())
				Expect(a.UnhealthyAt.Valid).To(BeTrue())
			})

			It("marks the agent unhealthy instead of recreating a fourth time in 24 hours, and recreates again once resumed", func(ctx SpecContext) {
				for i := 1; i <= 3; i++ {
					fail(openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
					pass(ctx, r) // deletes
					pass(ctx, r) // creates
					Expect(recreations(ctx)).To(Equal(i))
				}
				Expect(osg.Calls("DeleteSandbox")).To(Equal(3))

				fail(openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(3), "not recreated in a loop")
				a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
				Expect(err).NotTo(HaveOccurred())
				Expect(a.UnhealthyAt.Valid).To(BeTrue())
				Expect(a.UnhealthyReason.String).To(And(
					ContainSubstring("the sandbox kept failing"), ContainSubstring("3 recreations in 24 hours"), ContainSubstring("phase Error")))
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(3), "an unhealthy agent's failed sandbox waits for the admin")

				_, err = q.ResumeAgent(ctx, store.ResumeAgentParams{OrgID: org, ID: agent.ID})
				Expect(err).NotTo(HaveOccurred())
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(4), "a new count after the resume")
			})
		})

		It("stops after the idle timeout and starts again for a queued run", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			phase := func() openshellv1.SandboxPhase {
				return osg.Sandbox(row.Workspace, names.Sandbox).GetStatus().GetPhase()
			}

			advance(9 * time.Minute)
			pass(ctx, r)
			Expect(phase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))

			advance(11 * time.Minute)
			pass(ctx, r)
			Expect(phase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
			stopped := sandboxRow(ctx, agent.ID)
			Expect(stopped.State).To(Equal(store.SandboxStateStopped))
			Expect(stopped.StoppedAt.Valid).To(BeTrue())

			By("a queued run starts it")
			now.Store(nil)
			queueRun(ctx, org, user, agent.ID)
			pass(ctx, r)
			Expect(phase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
			Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateReady))

			By("an active run keeps it up past the timeout")
			run := claimRun(ctx, org)
			advance(time.Hour)
			pass(ctx, r)
			Expect(phase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))

			By("a parked run does not")
			parked, err := q.ParkRun(ctx, store.ParkRunParams{
				OrgID: org, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonApproval,
			})
			Expect(err).NotTo(HaveOccurred())
			advance(5 * time.Minute)
			pass(ctx, r)
			Expect(phase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
			t := parked.ParkedAt.Time.Add(10*time.Minute + time.Second)
			now.Store(&t)
			pass(ctx, r)
			Expect(phase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))

			By("resolving the park starts it again")
			_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: org, ID: run.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r)
			Expect(phase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
		})

		It("stops the sandbox of an agent returned to draft", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			_, err := q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r)
			Expect(osg.Sandbox(row.Workspace, names.Sandbox).GetStatus().GetPhase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
			Expect(osg.Provider(row.Workspace, names.Provider)).NotTo(BeNil())
			Expect(mg.last().Keys).To(HaveLen(1), "the key survives for re-activation")
		})

		It("cleans up everything on archive", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r)

			Expect(osg.Sandbox(row.Workspace, names.Sandbox)).To(BeNil())
			Expect(osg.Provider(row.Workspace, names.Provider)).To(BeNil())
			_, err = kube.CoreV1().Secrets(namespace).Get(ctx, row.KeySecretName, metav1.GetOptions{})
			Expect(err).To(HaveOccurred())
			Expect(mg.last().Keys).To(BeEmpty(), "the key is revoked")
			done := sandboxRow(ctx, agent.ID)
			Expect(done.State).To(Equal(store.SandboxStateDeleted))
			Expect(done.GatewayKeyID.Valid).To(BeFalse())

			By("later passes leave it alone")
			calls := osg.Calls("GetSandbox")
			pass(ctx, r)
			Expect(osg.Calls("GetSandbox")).To(Equal(calls))
		})

		It("keeps the stopped sandbox, and so its home, with retention retain", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			cfg.HomeRetention = reconciler.RetentionRetain
			r = newReconciler()
			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx, r)

			Expect(osg.Sandbox(row.Workspace, names.Sandbox).GetStatus().GetPhase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
			Expect(osg.Provider(row.Workspace, names.Provider)).To(BeNil())
			Expect(mg.last().Keys).To(BeEmpty())
			Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateRetained))
		})
	})

	It("pushes the agent's spend limit net of recorded spend, also after a key rotation", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"10.00"}}`)
		r := newReconciler()
		pass(ctx, r)
		row := sandboxRow(ctx, agent.ID)
		ref := mg.last().Keys[0].Agent
		full := mg.last().Limits[ref]
		Expect(full.Unit).To(Equal(gateway.USD))
		Expect(full.Amount).To(BeNumerically("==", 10))
		Expect(full.Window).To(BeZero(), "the budget never resets on its own: the generation carries the day")

		By("recorded spend lowers the pushed amount and starts a new gateway counter")
		_, err := pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model,
			input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, 'req-1', $3, 'm', 1, 1, 2.5, true, now())`, org, agent.ID, row.GatewayKeyID.String)
		Expect(err).NotTo(HaveOccurred())
		pass(ctx, r)
		net := mg.last().Limits[ref]
		Expect(net.Amount).To(BeNumerically("==", 7.5))
		Expect(net.Generation).NotTo(Equal(full.Generation))

		By("a rotated key gets the remainder, not the full limit")
		Expect(kube.CoreV1().Secrets(namespace).Delete(ctx, row.KeySecretName, metav1.DeleteOptions{})).To(Succeed())
		pass(ctx, r)
		Expect(mg.last().Keys[0].ID).NotTo(Equal(row.GatewayKeyID.String))
		Expect(mg.last().Limits[ref]).To(Equal(net))
	})

	It("fails one agent's key closed when its budget cannot be computed, and still serves every other agent", func(ctx SpecContext) {
		good := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"10.00"}}`)
		broken := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"20.00"}}`)
		cfg2 := cfg
		cfg2.AgentLimit = func(ctx context.Context, q *store.Queries, a store.Agent, at time.Time) (*gateway.Limit, error) {
			if a.ID == broken.ID {
				return nil, errors.New("simulated budget failure")
			}
			return ledger.AgentLimit(ctx, q, a, at)
		}
		r, err := reconciler.New(pool, backend, mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg2)
		Expect(err).NotTo(HaveOccurred())

		err = r.ReconcileAll(ctx)
		Expect(err).To(HaveOccurred(), "the failure is reported")
		Expect(err.Error()).To(ContainSubstring("fails closed"))
		st := mg.last()
		byAgent := map[string]gateway.Limit{}
		for _, k := range st.Keys {
			byAgent[k.Agent.Name] = st.Limits[k.Agent]
		}
		Expect(byAgent[good.ID.String()].Amount).To(BeNumerically("==", 10), "the other agent is served with its budget")
		Expect(byAgent[broken.ID.String()].Amount).To(BeZero(), "no earlier limit: the key is blocked")
	})

	It("keeps a failed agent's last limit only for the UTC day it was pushed", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"10.00"}}`)
		failing := false
		cfg2 := cfg
		cfg2.AgentLimit = func(ctx context.Context, q *store.Queries, a store.Agent, at time.Time) (*gateway.Limit, error) {
			if failing && a.ID == agent.ID {
				return nil, errors.New("simulated budget failure")
			}
			return ledger.AgentLimit(ctx, q, a, at)
		}
		r, err := reconciler.New(pool, backend, mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg2)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		ref := mg.last().Keys[0].Agent
		Expect(mg.last().Limits[ref].Amount).To(BeNumerically("==", 10))

		failing = true
		Expect(r.ReconcileAll(ctx)).NotTo(Succeed())
		Expect(mg.last().Limits[ref].Amount).To(BeNumerically("==", 10), "the same day: the last pushed limit stands")

		advance(26 * time.Hour)
		Expect(r.ReconcileAll(ctx)).NotTo(Succeed())
		Expect(mg.last().Limits[ref].Amount).To(BeZero(), "a new UTC day: a failing computation does not regrant yesterday's amount")
	})

	It("fails a key closed to zero at once when the gateway only honours a limit of zero (Kubernetes mode)", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"10.00"}}`)
		mg.ledgerBudgets = true
		failing := false
		cfg2 := cfg
		cfg2.AgentLimit = func(ctx context.Context, q *store.Queries, a store.Agent, at time.Time) (*gateway.Limit, error) {
			if failing && a.ID == agent.ID {
				return nil, errors.New("simulated budget failure")
			}
			return ledger.AgentLimit(ctx, q, a, at)
		}
		r, err := reconciler.New(pool, backend, mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg2)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		ref := mg.last().Keys[0].Agent
		Expect(mg.last().Limits[ref].Amount).To(BeNumerically("==", 10))

		failing = true
		Expect(r.ReconcileAll(ctx)).NotTo(Succeed())
		Expect(mg.last().Limits[ref].Amount).To(BeZero(), "a positive last limit would not bind: the key is revoked")
	})

	It("stops a spent agent's runs with the budget reason before the key is revoked (#188)", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"1.00"}}`)
		mg.ledgerBudgets = true
		r := newReconciler()
		pass(ctx, r)
		row := sandboxRow(ctx, agent.ID)
		first := queueRun(ctx, org, user, agent.ID)
		running := claimRun(ctx, org)
		Expect(running.ID).To(Equal(first.ID))
		queued := queueRun(ctx, org, user, agent.ID)

		// The limit runs out without an entry that reaches it in the ledger (an admin lowered it
		// below the spend, or the spend has no run), so only the reconciler sees it.
		_, err := pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model,
			input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, $3, $4, 'm', 1, 1, 1.25, true, now())`, org, agent.ID, uniq("req"), row.GatewayKeyID.String)
		Expect(err).NotTo(HaveOccurred())

		var atRevoke []store.RunState
		mg.onApply = func(s gateway.State) {
			for ref, l := range s.Limits {
				if ref.Name == agent.ID.String() && l.Amount <= 0 {
					for _, id := range []pgtype.UUID{running.ID, queued.ID} {
						got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: id})
						Expect(err).NotTo(HaveOccurred())
						atRevoke = append(atRevoke, got.State)
					}
				}
			}
		}
		pass(ctx, r)
		Expect(atRevoke).To(Equal([]store.RunState{store.RunStateCanceled, store.RunStateCanceled}),
			"both runs are stopped when the gateway is told to revoke the key")
		for _, id := range []pgtype.UUID{running.ID, queued.ID} {
			got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: id})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
			Expect(got.OutcomeReason.String).To(HavePrefix("agent daily limit reached ($1.25 of $1.00"))
		}

		By("the owner's late report of the gateway's 401 changes nothing")
		_, err = q.FinishRun(ctx, store.FinishRunParams{
			Outcome: store.RunStateFailed, OutcomeReason: pgtype.Text{String: "provider authentication required", Valid: true},
			OrgID: org, ID: running.ID, Epoch: running.Epoch,
		})
		Expect(err).To(MatchError(pgx.ErrNoRows))
	})

	It("joins several rejections of one org into one message", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		var slug string
		Expect(pool.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, org).Scan(&slug)).To(Succeed())
		mg.fail = &gateway.RejectedError{Rejected: []gateway.Rejection{
			{Org: slug, Kind: "AgentgatewayModel", Name: "m-1", Reason: "first"},
			{Org: slug, Kind: "ConfigMap", Name: "keys", Reason: "second"},
		}}
		Expect(newReconciler().ReconcileAll(ctx)).To(HaveOccurred())
		msg := sandboxRow(ctx, agent.ID).Message
		Expect(msg).To(ContainSubstring("AgentgatewayModel m-1: first"))
		Expect(msg).To(ContainSubstring("ConfigMap keys: second"))
	})

	It("uses one workspace per org", func(ctx SpecContext) {
		a1 := newAgent(ctx, org, user)
		a2 := newAgent(ctx, org, user)
		org2, user2 := newOrg(ctx)
		a3 := newAgent(ctx, org2, user2)
		pass(ctx, newReconciler())
		Expect(osg.Calls("CreateWorkspace")).To(Equal(2))
		Expect(sandboxRow(ctx, a1.ID).Workspace).To(Equal(sandboxRow(ctx, a2.ID).Workspace))
		row3, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org2, AgentID: a3.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(row3.Workspace).To(Equal(execution.WorkspaceName(org2.Bytes)))
		Expect(mg.last().Keys).To(HaveLen(3))
	})

	It("does not touch OpenShell when the gateway rejects the state", func(ctx SpecContext) {
		newAgent(ctx, org, user)
		mg.fail = errors.New("rejected")
		Expect(newReconciler().ReconcileAll(ctx)).To(MatchError(ContainSubstring("rejected")))
		Expect(osg.Calls("CreateSandbox")).To(Equal(0))
		Expect(osg.Calls("CreateProvider")).To(Equal(0))
	})

	Describe("when the gateway cannot apply the state", func() {
		phase := func(row store.AgentSandbox, agent pgtype.UUID) openshellv1.SandboxPhase {
			names := execution.AgentObjectNames(org.Bytes, agent.Bytes)
			return osg.Sandbox(row.Workspace, names.Sandbox).GetStatus().GetPhase()
		}

		It("still starts a stopped sandbox with demand and stops an idle one, and reports the error", func(ctx SpecContext) {
			a := newAgent(ctx, org, user)
			b := newAgent(ctx, org, user)
			r := newReconciler()
			pass(ctx, r)
			Expect(phase(sandboxRow(ctx, a.ID), a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))

			mg.fail = errors.New("gateway unreachable")
			advance(11 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("gateway unreachable")))
			Expect(phase(sandboxRow(ctx, a.ID), a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED), "idle stop")
			Expect(sandboxRow(ctx, b.ID).State).To(Equal(store.SandboxStateStopped))

			now.Store(nil)
			queueRun(ctx, org, user, a.ID)
			Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("gateway unreachable")))
			Expect(phase(sandboxRow(ctx, a.ID), a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY), "start on demand")
			Expect(sandboxRow(ctx, a.ID).State).To(Equal(store.SandboxStateReady))
			Expect(sandboxRow(ctx, b.ID).State).To(Equal(store.SandboxStateStopped), "no demand for b")
		})

		It("records sandbox state of agents whose keys did not change, and holds back an agent whose new key is not live", func(ctx SpecContext) {
			a := newAgent(ctx, org, user)
			r := newReconciler()
			pass(ctx, r)
			keyBefore := sandboxRow(ctx, a.ID).GatewayKeyID
			Expect(osg.Calls("CreateSandbox")).To(Equal(1))

			// A reload that does not complete in time, as a slow Waiter reports it.
			mg.fail = fmt.Errorf("wait for revision: %w", context.DeadlineExceeded)
			fresh := newAgent(ctx, org, user)
			advance(11 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(MatchError(context.DeadlineExceeded))
			Expect(sandboxRow(ctx, a.ID).State).To(Equal(store.SandboxStateStopped), "a's state is recorded")
			Expect(sandboxRow(ctx, a.ID).GatewayKeyID).To(Equal(keyBefore))
			Expect(osg.Calls("CreateSandbox")).To(Equal(1), "the fresh agent's key is not live: no sandbox yet")
			Expect(sandboxRow(ctx, fresh.ID).Message).To(ContainSubstring("waiting for the gateway"))

			By("the gateway is back")
			mg.fail = nil
			now.Store(nil)
			pass(ctx, r)
			Expect(osg.Calls("CreateSandbox")).To(Equal(2))
			Expect(sandboxRow(ctx, fresh.ID).State).To(Equal(store.SandboxStateReady))
		})

		It("holds back every agent after a restart until one Apply has succeeded", func(ctx SpecContext) {
			a := newAgent(ctx, org, user)
			pass(ctx, newReconciler())
			Expect(phase(sandboxRow(ctx, a.ID), a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))

			restarted := newReconciler()
			mg.fail = errors.New("gateway unreachable")
			advance(11 * time.Minute)
			Expect(restarted.ReconcileAll(ctx)).To(HaveOccurred())
			Expect(phase(sandboxRow(ctx, a.ID), a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY),
				"the recorded key may not be live: no sandbox work")

			mg.fail = nil
			pass(ctx, restarted)
			Expect(phase(sandboxRow(ctx, a.ID), a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
		})

		It("keeps holding back a new key over later failed passes", func(ctx SpecContext) {
			r := newReconciler()
			newAgent(ctx, org, user)
			mg.fail = errors.New("gateway unreachable")
			Expect(r.ReconcileAll(ctx)).To(HaveOccurred())
			Expect(r.ReconcileAll(ctx)).To(HaveOccurred())
			Expect(osg.Calls("CreateSandbox")).To(Equal(0))
			Expect(osg.Calls("CreateProvider")).To(Equal(0))
		})
	})

	It("goes on when the gateway rejects only some orgs' objects, and reports them on the org's agents", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		var slug string
		Expect(pool.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, org).Scan(&slug)).To(Succeed())
		mg.fail = &gateway.RejectedError{Rejected: []gateway.Rejection{{Org: slug, Kind: "AgentgatewayModel", Name: "m-1", Reason: "not CEL"}}}
		err := newReconciler().ReconcileAll(ctx)
		Expect(err).To(MatchError(ContainSubstring("AgentgatewayModel m-1 rejected")))
		// The agent was still converged: the rest of the gateway state is live.
		Expect(osg.Calls("CreateSandbox")).To(Equal(1))
		Expect(sandboxRow(ctx, agent.ID).Message).To(ContainSubstring("the gateway rejected AgentgatewayModel m-1: not CEL"))
	})

	It("wakes up on NOTIFY when a run is queued", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		cfg.ResyncInterval = time.Hour
		advance(time.Hour) // the first pass creates the sandbox; the next one stops it as idle
		r := newReconciler()
		pass(ctx, r)
		pass(ctx, r)
		names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		ws := execution.WorkspaceName(org.Bytes)
		Expect(osg.Sandbox(ws, names.Sandbox).GetStatus().GetPhase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
		now.Store(nil)

		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Run(runCtx) }()
		DeferCleanup(func() { cancel(); Eventually(done).Should(Receive(BeNil())) })
		Eventually(logs.String).Should(ContainSubstring("reconciler is leading"))

		queueRun(ctx, org, user, agent.ID)
		Eventually(func() openshellv1.SandboxPhase {
			return osg.Sandbox(ws, names.Sandbox).GetStatus().GetPhase()
		}).WithTimeout(5 * time.Second).Should(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
	})
})
