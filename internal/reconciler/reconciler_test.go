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

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/execution"
	"github.com/procoduck/koinon/internal/execution/openshell"
	"github.com/procoduck/koinon/internal/execution/openshell/fakegateway"
	"github.com/procoduck/koinon/internal/gateway"
	sandboxv1 "github.com/procoduck/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/procoduck/koinon/internal/gen/openshell/v1"
	"github.com/procoduck/koinon/internal/ledger"
	"github.com/procoduck/koinon/internal/reconciler"
)

const (
	namespace = "koinon"
	image     = "ghcr.io/procoduck/flavor-opencode:test"
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
			Gateway: reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Models: gateway.ModelUpstream{
				Provider: "openai", BaseURL: "http://fakemodel:8080/v1",
				Models: []gateway.Model{{Name: "fake"}},
			},
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
	})

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
			Expect(state.Models.Models).To(HaveLen(1))

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
		Expect(full.Window).To(Equal(ledger.Window))

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
