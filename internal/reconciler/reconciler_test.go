// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/encoding/protojson"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

const (
	namespace = "koinon"
	image     = "ghcr.io/stateracloud/koinon/sandbox:test"
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

// testGatewayID is the OpenShell gateway ID every wiring in this suite shares, so the namespaces the
// specs compute and the ones the writers use cannot drift apart.
const testGatewayID = "openshell"

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
			Prices:         prices.NewResolver(pricing.Embedded()),
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			URLPolicy:      testPolicy,
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
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

	DescribeTable("lets only the harness's network-calling binaries use the agentgateway credential",
		func(ctx SpecContext, harness string, binaries []string) {
			Expect(reconciler.DefaultBinaries[harness]).To(Equal(binaries))
			cfg.Harnesses = map[string]reconciler.Harness{
				harness: {Image: image, Binaries: reconciler.DefaultBinaries[harness]},
			}
			agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
				OrgID: org, DisplayName: "Harness binaries", Harness: harness, CreatedBy: user,
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
			keepWarm(ctx, org, agent.ID)
			pass(ctx, newReconciler())

			row := sandboxRow(ctx, agent.ID)
			Expect(row.State).To(Equal(store.SandboxStateReady))
			// The enforcement mode of the sandbox's policy is recorded on the row (ADR 0045, #926).
			Expect(row.Enforcement).To(Equal("enforce"))
			profile := osg.Profile(row.Workspace, "koinon-agentgateway-"+harness)
			Expect(profile).NotTo(BeNil())
			var paths []string
			for _, b := range profile.GetBinaries() {
				paths = append(paths, b.GetPath())
			}
			Expect(paths).To(Equal(binaries))
		},
		Entry("OpenCode: its single binary", harness.OpenCode, []string{"/usr/local/bin/opencode"}),
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
			Expect(agentKeys(state)).To(ConsistOf(gateway.Key{
				ID: row.GatewayKeyID.String, Hash: row.GatewayKeyHash.String,
				Agent: gateway.AgentRef{Org: agentKeys(state)[0].Agent.Org, Name: agentKeys(state)[0].Agent.Name},
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

			By("the sandbox runs the harness image under an enforced policy")
			sb := osg.Sandbox(ws, names.Sandbox)
			Expect(sb).NotTo(BeNil())
			Expect(sb.GetSpec().GetTemplate().GetImage()).To(Equal(image))
			Expect(sb.GetSpec().GetTemplate().GetRuntimeClassName()).To(Equal("kata"))
			// No template size: the platform default, with the Kata memory request equal to its limit (ADR 0083).
			Expect(sb.GetSpec().GetTemplate().GetResources().AsMap()).To(Equal(map[string]any{
				"requests": map[string]any{"cpu": "250m", "memory": "2Gi"},
				"limits":   map[string]any{"cpu": "2", "memory": "2Gi"},
			}))
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
			Expect(mg.issuedCount()).To(Equal(2), "the agent's key and the org's model-check key, each once")
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
			Expect(agentKeys(mg.last())).To(HaveLen(1))
			Expect(agentKeys(mg.last())[0].ID).To(Equal(rotated.GatewayKeyID.String))
			Expect(osg.Calls("CreateSandbox")).To(Equal(1))

			By("a deleted sandbox is recreated")
			Expect(deleteErr(backend.DeleteSandbox(ctx, ws, names.Sandbox))).To(Succeed())
			pass(ctx, r)
			Expect(osg.Sandbox(ws, names.Sandbox)).NotTo(BeNil())
			Expect(osg.Calls("CreateSandbox")).To(Equal(2))
		})

		It("recreates the sandbox when its image changes, but not during a run", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			cfg.Harnesses[harness.OpenCode] = reconciler.Harness{
				Image: image + "-2", Binaries: reconciler.DefaultBinaries[harness.OpenCode],
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

		It("recreates the sandbox when the gateway's supervisor image changes, and keeps the last image when the gateway cannot be read", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			sup := &fakeSupervisor{image: "ghcr.io/nvidia/openshell/supervisor:0.1.5"}
			cfg.Supervisor = sup
			r = newReconciler()
			// The sandbox made before the supervisor label is recreated once (the next test); this one
			// starts from a sandbox that carries it.
			Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
			pass(ctx, r)
			pass(ctx, r)
			baseline := osg.Calls("DeleteSandbox")

			By("an unreadable gateway ConfigMap changes nothing")
			sup.err = errors.New("forbidden")
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(baseline))

			By("the same image changes nothing")
			sup.err = nil
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(baseline))

			By("a new image recreates the sandbox through the usual path")
			sup.image = "ghcr.io/nvidia/openshell/supervisor:0.1.6"
			pass(ctx, r) // deletes
			pass(ctx, r) // creates
			Expect(osg.Calls("DeleteSandbox")).To(Equal(baseline + 1))
			Expect(osg.Sandbox(row.Workspace, names.Sandbox)).NotTo(BeNil())
			Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateReady))
		})

		It("recreates a sandbox that has no supervisor label once the image is known, then settles", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			sup := &fakeSupervisor{err: errors.New("forbidden")}
			cfg.Supervisor = sup
			r = newReconciler()
			Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
			pass(ctx, r) // created while the image is unreadable: no label
			Expect(osg.Sandbox(row.Workspace, names.Sandbox)).NotTo(BeNil())
			baseline := osg.Calls("DeleteSandbox")

			sup.err, sup.image = nil, "ghcr.io/nvidia/openshell/supervisor:0.1.5"
			pass(ctx, r) // deletes
			pass(ctx, r) // creates, with the label
			Expect(osg.Calls("DeleteSandbox")).To(Equal(baseline + 1))
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(baseline + 1))
		})

		It("still recreates the sandbox for a changed harness image when the gateway ConfigMap can never be read", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			cfg.Supervisor = &fakeSupervisor{err: errors.New("forbidden")}
			r = newReconciler()
			pass(ctx, r)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))

			cfg.Harnesses[harness.OpenCode] = reconciler.Harness{
				Image: image + "-2", Binaries: reconciler.DefaultBinaries[harness.OpenCode],
			}
			r = newReconciler()
			pass(ctx, r) // deletes
			pass(ctx, r) // creates
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(osg.Sandbox(row.Workspace, names.Sandbox).GetSpec().GetTemplate().GetImage()).To(Equal(image + "-2"))
		})

		It("still creates a sandbox when the gateway ConfigMap can never be read", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			cfg.Supervisor = &fakeSupervisor{err: errors.New("forbidden")}
			r = newReconciler()
			Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
			pass(ctx, r)
			Expect(osg.Sandbox(row.Workspace, names.Sandbox)).NotTo(BeNil())
		})

		It("records the sandbox's OpenShell ID as its home generation, new for a recreated sandbox", func(ctx SpecContext) {
			coolDown(ctx, org, agent.ID)
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
			Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
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

			cfg.Harnesses[harness.OpenCode] = reconciler.Harness{
				Image: image + "-2", Binaries: reconciler.DefaultBinaries[harness.OpenCode],
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

		It("records the reason and message of the sandbox's condition that is not true", func(ctx SpecContext) {
			names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
			osg.SetCondition(row.Workspace, names.Sandbox, "ErrImagePull", "boom")
			pass(ctx, r)
			got := sandboxRow(ctx, agent.ID)
			Expect(got.Reason).To(Equal("ErrImagePull"))
			Expect(got.Message).To(Equal("boom"))
		})

		Describe("the sandbox observer (ADR 0078)", func() {
			var (
				names   execution.AgentNames
				dyn     *dynamicfake.FakeDynamicClient
				metrics *reconciler.Metrics
				ns      string
				obsNow  atomic.Pointer[time.Time]
			)
			const osReason, osMessage = "ConfigurationPending", "Waiting for effective configuration validation before workload activation"
			imagePullPod := func() *corev1.Pod {
				// The captured wc-img pod status (internal/execution/testdata).
				data, err := os.ReadFile("../execution/testdata/wc-img-init-container-statuses.json")
				Expect(err).NotTo(HaveOccurred())
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Sandbox, Namespace: ns}}
				Expect(json.Unmarshal(data, &pod.Status.InitContainerStatuses)).To(Succeed())
				return pod
			}
			BeforeEach(func() {
				names = execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
				ns = execution.WorkspaceNamespace(testGatewayID, row.Workspace)
				scheme := runtime.NewScheme()
				dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
					{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}: "SandboxList",
				})
				metrics = reconciler.NewMetrics(nil)
				cfg.Metrics = metrics
				cfg.Observer = &reconciler.SandboxObserver{
					Kube: kube, Dynamic: dyn, GatewayID: testGatewayID, ClusterRole: "koinon-koinon-sandbox-observer",
					ServiceAccount: "koinon-reconciler", ServiceAccountNamespace: namespace,
					Now: func() time.Time { return *obsNow.Load() },
				}
				t0 := time.Now()
				obsNow.Store(&t0)
				r = newReconciler()
			})
			provisioning := func() {
				osg.SetPhase(row.Workspace, names.Sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_PROVISIONING)
				osg.SetCondition(row.Workspace, names.Sandbox, osReason, osMessage)
			}

			It("binds the observer role once per workspace namespace", func(ctx SpecContext) {
				pass(ctx, r)
				pass(ctx, r)
				bindings, err := kube.RbacV1().RoleBindings(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(bindings.Items).To(HaveLen(1))
				b := bindings.Items[0]
				Expect(b.Name).To(Equal("koinon-koinon-sandbox-observer"))
				Expect(b.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "koinon-koinon-sandbox-observer"}))
				Expect(b.Subjects).To(Equal([]rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "koinon-reconciler", Namespace: namespace}}))
			})

			It("records the pod's pull error in place of OpenShell's opaque reason", func(ctx SpecContext) {
				_, err := kube.CoreV1().Pods(ns).Create(ctx, imagePullPod(), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				provisioning()
				pass(ctx, r)
				got := sandboxRow(ctx, agent.ID)
				Expect(got.Reason).To(Equal("ImagePullBackOff"))
				Expect(got.Message).To(HavePrefix(`Back-off pulling image "kind.local/koinon-escape/agent:nope"`))
				Expect(execution.ClassifySandboxWait(got.Reason, got.Message)).To(Equal(execution.WaitCauseImagePull))
			})

			It("reads the supervisor pod, by the sandbox's ID, when the workload pod says nothing", func(ctx SpecContext) {
				// Captured on kind (OpenShell 0.1.5, supervisor_image that cannot be pulled): the workload pod
				// ended with nothing waiting, the supervisor pod was in ErrImagePull, and OpenShell said
				// ConfigurationPending.
				workload := imagePullPod()
				workload.Status.InitContainerStatuses = nil
				_, err := kube.CoreV1().Pods(ns).Create(ctx, workload, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				data, err := os.ReadFile("../execution/testdata/supervisor-bad-image-container-statuses.json")
				Expect(err).NotTo(HaveOccurred())
				sup := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: execution.SupervisorPodName(sandboxRow(ctx, agent.ID).HomeGeneration.String), Namespace: ns}}
				Expect(json.Unmarshal(data, &sup.Status.ContainerStatuses)).To(Succeed())
				_, err = kube.CoreV1().Pods(ns).Create(ctx, sup, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				provisioning()
				pass(ctx, r)
				got := sandboxRow(ctx, agent.ID)
				Expect(got.Reason).To(Equal("ErrImagePull"))
				Expect(got.Message).To(HavePrefix("supervisor pod: failed to pull and unpack image"))
				Expect(execution.ClassifySandboxWait(got.Reason, got.Message)).To(Equal(execution.WaitCauseImagePull))
			})

			It("reads the Agent Sandbox object when the pod is missing", func(ctx SpecContext) {
				cr := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
					"metadata": map[string]any{"name": names.Sandbox, "namespace": ns},
					"status": map[string]any{"conditions": []any{map[string]any{
						"type": "Ready", "status": "False", "reason": "ReconcilerError",
						"message": `Error seen: pods "x" is forbidden: ValidatingAdmissionPolicy 'p' with binding 'b' denied request: no`,
					}}},
				}}
				_, err := dyn.Resource(schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}).
					Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				provisioning()
				pass(ctx, r)
				got := sandboxRow(ctx, agent.ID)
				Expect(got.Reason).To(Equal("ReconcilerError"))
				Expect(execution.ClassifySandboxWait(got.Reason, got.Message)).To(Equal(execution.WaitCauseAdmission))
			})

			It("keeps OpenShell's words when the pod says nothing, and reads nothing for an informative reason", func(ctx SpecContext) {
				pod := imagePullPod()
				pod.Status.InitContainerStatuses = nil
				_, err := kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				provisioning()
				pass(ctx, r)
				Expect(sandboxRow(ctx, agent.ID).Reason).To(Equal(osReason))

				_, err = kube.CoreV1().Pods(ns).Update(ctx, imagePullPod(), metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				osg.SetCondition(row.Workspace, names.Sandbox, "Unschedulable", "0/1 nodes are available: 1 Insufficient cpu.")
				pass(ctx, r)
				Expect(sandboxRow(ctx, agent.ID).Reason).To(Equal("Unschedulable"))
			})

			It("counts a forbidden read, keeps OpenShell's reason and does not fail the pass", func(ctx SpecContext) {
				kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, names.Sandbox, errors.New("no"))
				})
				provisioning()
				pass(ctx, r)
				Expect(sandboxRow(ctx, agent.ID).Reason).To(Equal(osReason))
				Expect(testutil.ToFloat64(metrics.ObserverErrors.WithLabelValues("forbidden"))).To(BeNumerically(">=", 1))
			})

			It("counts a refused RoleBinding and retries it after the backoff", func(ctx SpecContext) {
				var refuse atomic.Bool
				refuse.Store(true)
				kube.PrependReactor("create", "rolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
					if refuse.Load() {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "rolebindings"}, "b", errors.New("policy"))
					}
					return false, nil, nil
				})
				pass(ctx, r)
				Expect(testutil.ToFloat64(metrics.ObserverErrors.WithLabelValues("forbidden"))).To(BeNumerically(">=", 1))
				By("the next pass does not ask again inside the backoff")
				before := len(kube.Actions())
				pass(ctx, r)
				creates := 0
				for _, a := range kube.Actions()[before:] {
					if a.Matches("create", "rolebindings") {
						creates++
					}
				}
				Expect(creates).To(BeZero())
				refuse.Store(false)
				later := obsNow.Load().Add(11 * time.Second)
				obsNow.Store(&later)
				pass(ctx, r)
				bindings, err := kube.RbacV1().RoleBindings(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(bindings.Items).To(HaveLen(1))
			})

			It("treats AlreadyExists as done", func(ctx SpecContext) {
				_, err := kube.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "koinon-koinon-sandbox-observer", Namespace: ns}}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				pass(ctx, r)
				Expect(testutil.ToFloat64(metrics.ObserverErrors.WithLabelValues("other"))).To(BeZero())
			})

			It("is off when no observer is configured", func(ctx SpecContext) {
				cfg.Observer = nil
				r = newReconciler()
				_, err := kube.CoreV1().Pods(ns).Create(ctx, imagePullPod(), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				provisioning()
				pass(ctx, r)
				Expect(sandboxRow(ctx, agent.ID).Reason).To(Equal(osReason))
				bindings, err := kube.RbacV1().RoleBindings(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(bindings.Items).To(BeEmpty())
			})
		})

		Describe("the sandbox default-deny NetworkPolicy (#993)", func() {
			var (
				metrics *reconciler.Metrics
				ns      string
			)
			const npName = "koinon-koinon-sandbox-default-deny"
			BeforeEach(func() {
				ns = execution.WorkspaceNamespace(testGatewayID, row.Workspace)
				metrics = reconciler.NewMetrics(nil)
				cfg.Metrics = metrics
				cfg.NetworkPolicy = &reconciler.SandboxNetworkPolicy{Kube: kube, GatewayID: testGatewayID, Name: npName}
				r = newReconciler()
			})
			// The namespace is OpenShell's, made with the first sandbox: writes into it fail until then.
			namespaceAfterCreate := func(ctx context.Context, failAfter bool) {
				names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
				Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
				created := osg.Calls("CreateSandbox")
				kube.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
					if osg.Calls("CreateSandbox") == created || failAfter {
						return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
					}
					return false, nil, nil
				})
			}

			It("writes the policy in the same pass as the first CreateSandbox", func(ctx SpecContext) {
				namespaceAfterCreate(ctx, false)
				pass(ctx, r)
				Expect(sandboxRow(ctx, agent.ID).Phase).NotTo(BeEmpty())
				np, err := kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, npName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "the policy exists after the pass that created the sandbox")
				Expect(np.Labels).To(HaveKeyWithValue(reconciler.NetPolLabel, "true"))
			})

			It("counts a missing namespace after CreateSandbox and fails the pass", func(ctx SpecContext) {
				namespaceAfterCreate(ctx, true)
				Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("sandbox NetworkPolicy after create")))
				Expect(testutil.ToFloat64(metrics.NetworkPolicyErrors.WithLabelValues("not_found"))).To(BeNumerically(">=", 1))
			})

			It("still handles the LimitRange and records the sandbox when the policy write is forbidden", func(ctx SpecContext) {
				lrName := "koinon-koinon-sandbox-limitrange"
				cfg.LimitRange = &reconciler.SandboxLimitRange{
					Kube: kube, GatewayID: testGatewayID, Name: lrName,
				}
				cfg.Metrics = metrics
				r = newReconciler()
				names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
				Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
				deletes := osg.Calls("DeleteSandbox")
				var lrCreates atomic.Int32
				kube.PrependReactor("create", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
					if lrCreates.Add(1) == 1 {
						return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
					}
					return false, nil, nil
				})
				kube.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "networkpolicies"}, npName, errors.New("rbac"))
				})
				Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("sandbox NetworkPolicy after create")))
				Expect(osg.Calls("DeleteSandbox")).To(Equal(deletes+1), "the BestEffort sandbox is still deleted once")
				Expect(sandboxRow(ctx, agent.ID).Phase).To(Equal(string(execution.PhaseDeleting)), "the pass records the sandbox")
				Expect(testutil.ToFloat64(metrics.NetworkPolicyErrors.WithLabelValues("forbidden"))).To(BeNumerically(">=", 1))
				list, err := kube.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(list.Items).To(HaveLen(1), "the LimitRange is written although the policy write failed")
			})
		})

		Describe("the sandbox LimitRange (#76)", func() {
			var (
				metrics *reconciler.Metrics
				ns      string
				lrNow   atomic.Pointer[time.Time]
			)
			const lrName = "koinon-koinon-sandbox-limitrange"
			BeforeEach(func() {
				ns = execution.WorkspaceNamespace(testGatewayID, row.Workspace)
				metrics = reconciler.NewMetrics(nil)
				cfg.Metrics = metrics
				cfg.LimitRange = &reconciler.SandboxLimitRange{
					Kube: kube, GatewayID: testGatewayID, Name: lrName,
					Now: func() time.Time { return *lrNow.Load() },
				}
				t0 := time.Now()
				lrNow.Store(&t0)
				r = newReconciler()
			})

			It("applies the LimitRange once per workspace namespace", func(ctx SpecContext) {
				pass(ctx, r)
				pass(ctx, r)
				list, err := kube.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(list.Items).To(HaveLen(1))
				lr := list.Items[0]
				Expect(lr.Name).To(Equal(lrName))
				Expect(lr.Labels).To(HaveKeyWithValue(reconciler.LimitRangeLabel, "true"))
				Expect(lr.Spec.Limits).To(HaveLen(1))
				Expect(lr.Spec.Limits[0].Type).To(Equal(corev1.LimitTypeContainer))
			})

			It("counts a refused Create, holds the pass, and retries after the backoff", func(ctx SpecContext) {
				var refuse atomic.Bool
				refuse.Store(true)
				kube.PrependReactor("create", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
					if refuse.Load() {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "limitranges"}, "lr", errors.New("policy"))
					}
					return false, nil, nil
				})
				Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("sandbox LimitRange")))
				Expect(testutil.ToFloat64(metrics.LimitRangeErrors.WithLabelValues("forbidden"))).To(BeNumerically(">=", 1))
				By("the next pass does not ask again inside the backoff")
				before := len(kube.Actions())
				Expect(r.ReconcileAll(ctx)).To(HaveOccurred())
				creates := 0
				for _, a := range kube.Actions()[before:] {
					if a.Matches("create", "limitranges") {
						creates++
					}
				}
				Expect(creates).To(BeZero())
				refuse.Store(false)
				later := lrNow.Load().Add(11 * time.Second)
				lrNow.Store(&later)
				pass(ctx, r)
				list, err := kube.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(list.Items).To(HaveLen(1))
			})

			It("is off when no LimitRange writer is configured", func(ctx SpecContext) {
				cfg.LimitRange = nil
				r = newReconciler()
				pass(ctx, r)
				list, err := kube.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(list.Items).To(BeEmpty())
			})

			It("deletes the first sandbox when the LimitRange was missing at CreateSandbox", func(ctx SpecContext) {
				names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
				Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
				deletes := osg.Calls("DeleteSandbox")
				var creates atomic.Int32
				kube.PrependReactor("create", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
					if creates.Add(1) == 1 {
						return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
					}
					return false, nil, nil
				})
				pass(ctx, r)
				Expect(osg.Calls("DeleteSandbox")).To(Equal(deletes+1), "BestEffort admission is discarded once")
				Expect(sandboxRow(ctx, agent.ID).Phase).To(Equal(string(execution.PhaseDeleting)))
				Expect(sandboxRow(ctx, agent.ID).Message).To(ContainSubstring("recreating sandbox after the LimitRange was applied"))
				list, err := kube.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(list.Items).To(HaveLen(1), "Ensure after CreateSandbox wrote the LimitRange")
			})

			It("deletes the first sandbox even when Ensure after CreateSandbox fails", func(ctx SpecContext) {
				names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
				Expect(deleteErr(backend.DeleteSandbox(ctx, row.Workspace, names.Sandbox))).To(Succeed())
				deletes := osg.Calls("DeleteSandbox")
				var creates atomic.Int32
				kube.PrependReactor("create", "limitranges", func(k8stesting.Action) (bool, runtime.Object, error) {
					n := creates.Add(1)
					if n == 1 {
						return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
					}
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "limitranges"}, lrName, errors.New("policy"))
				})
				Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("sandbox LimitRange after create")))
				Expect(osg.Calls("DeleteSandbox")).To(Equal(deletes+1), "must not leave a BestEffort sandbox when Ensure fails")
				Expect(testutil.ToFloat64(metrics.LimitRangeErrors.WithLabelValues("forbidden"))).To(BeNumerically(">=", 1))
			})
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
			coolDown(ctx, org, agent.ID)
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
			Expect(agentKeys(mg.last())).To(HaveLen(1), "the key survives for re-activation")
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
			Expect(agentKeys(mg.last())).To(BeEmpty(), "the key is revoked")
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
			Expect(agentKeys(mg.last())).To(BeEmpty())
			Expect(sandboxRow(ctx, agent.ID).State).To(Equal(store.SandboxStateRetained))
		})
	})

	It("pushes the agent's spend limit net of recorded spend, also after a key rotation", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"10.00"}}`)
		r := newReconciler()
		pass(ctx, r)
		row := sandboxRow(ctx, agent.ID)
		ref := agentKeys(mg.last())[0].Agent
		full := mg.last().Limits[ref]
		Expect(full.Unit).To(Equal(gateway.USD))
		Expect(full.Amount).To(Equal(money.MustParse("10")))
		Expect(full.Window).To(Equal(ledger.Window), "windowed, so agentgateway can drop expired counters")

		By("recorded spend lowers the pushed amount and starts a new gateway counter")
		_, err := pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model,
			input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, 'req-1', $3, 'm', 1, 1, 2.5, true, now())`, org, agent.ID, row.GatewayKeyID.String)
		Expect(err).NotTo(HaveOccurred())
		pass(ctx, r)
		net := mg.last().Limits[ref]
		Expect(net.Amount).To(Equal(money.MustParse("7.5")))
		Expect(net.Generation).NotTo(Equal(full.Generation))

		By("a rotated key gets the remainder, not the full limit")
		Expect(kube.CoreV1().Secrets(namespace).Delete(ctx, row.KeySecretName, metav1.DeleteOptions{})).To(Succeed())
		pass(ctx, r)
		Expect(agentKeys(mg.last())[0].ID).NotTo(Equal(row.GatewayKeyID.String))
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
		Expect(byAgent[good.ID.String()].Amount).To(Equal(money.MustParse("10")), "the other agent is served with its budget")
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
		ref := agentKeys(mg.last())[0].Agent
		Expect(mg.last().Limits[ref].Amount).To(Equal(money.MustParse("10")))

		failing = true
		Expect(r.ReconcileAll(ctx)).NotTo(Succeed())
		Expect(mg.last().Limits[ref].Amount).To(Equal(money.MustParse("10")), "the same day: the last pushed limit stands")

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
		ref := agentKeys(mg.last())[0].Agent
		Expect(mg.last().Limits[ref].Amount).To(Equal(money.MustParse("10")))

		failing = true
		Expect(r.ReconcileAll(ctx)).NotTo(Succeed())
		Expect(mg.last().Limits[ref].Amount).To(BeZero(), "a positive last limit would not bind: the key is revoked")
	})

	It("stops a spent agent's runs with the budget reason before the key is revoked (old issue 188)", func(ctx SpecContext) {
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

	It("revokes the key after spend crosses the limit: ledger transaction, NOTIFY, reconcile pass (Kubernetes mode, #20)", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","limits":{"spendUSD":"0.05"}}`)
		mg.ledgerBudgets = true
		var slug string
		Expect(pool.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, org).Scan(&slug)).To(Succeed())

		// The first pass issues the key; Run then makes its own passes, so the only thing that can
		// wake it after them is the ledger's NOTIFY (the resync timer is an hour).
		cfg.ResyncInterval = time.Hour
		r := newReconciler()
		pass(ctx, r)
		row := sandboxRow(ctx, agent.ID)
		ref := agentKeys(mg.last())[0].Agent
		Expect(mg.last().Limits[ref].Amount).To(Equal(money.MustParse("0.05")))

		var applies atomic.Int64
		var revokedAt atomic.Pointer[time.Time]
		mg.onApply = func(s gateway.State) {
			applies.Add(1)
			if l, ok := s.Limits[ref]; ok && l.Amount <= 0 {
				t := time.Now()
				revokedAt.CompareAndSwap(nil, &t)
			}
		}
		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Run(runCtx) }()
		DeferCleanup(func() { cancel(); Eventually(done).Should(Receive(BeNil())) })
		Eventually(applies.Load).WithTimeout(10*time.Second).Should(BeNumerically(">=", 1), "the leader's first pass")
		Expect(revokedAt.Load()).To(BeNil(), "no spend yet: the key is live")

		ing := &ledger.Ingester{DB: pool, Log: cfg.Logger, Prices: prices.NewResolver(pricing.Embedded())}
		entry := func(cost string) ledger.Entry {
			return ledger.Entry{
				RequestID: uniq("req"), KeyID: row.GatewayKeyID.String, Model: "fake/model", CostUSD: cost,
				Agent: gateway.AgentRef{Org: slug, Name: agent.ID.String()}, InputTokens: 1, OutputTokens: 1, Time: time.Now(),
			}
		}
		_, err := ing.Ingest(ctx, []ledger.Entry{entry("0.03")})
		Expect(err).NotTo(HaveOccurred())
		// The ingest below the limit sends no NOTIFY (ledger/ingest.go: only a reserve change or an
		// enforce crossing does), so no pass can run between the two requests. If a later change adds
		// a wake, this check fails instead of the measured window silently changing.
		Consistently(applies.Load).WithTimeout(time.Second).WithPolling(100*time.Millisecond).Should(BeEquivalentTo(1), "no pass runs below the limit")
		Consistently(revokedAt.Load).WithTimeout(time.Second).WithPolling(100*time.Millisecond).Should(BeNil(), "below the limit the key stays live")

		By("the request that reaches the limit")
		start := time.Now()
		_, err = ing.Ingest(ctx, []ledger.Entry{entry("0.03")})
		committed := time.Now()
		Expect(err).NotTo(HaveOccurred())
		Eventually(revokedAt.Load).WithTimeout(10*time.Second).WithPolling(10*time.Millisecond).ShouldNot(BeNil(), "the NOTIFY woke the reconciler, which pushed a limit of zero")
		revoked := *revokedAt.Load()

		ledgerTx, reconcilePass, total := committed.Sub(start), revoked.Sub(committed), revoked.Sub(start)
		AddReportEntry("budget overshoot window", fmt.Sprintf("ledger transaction %s + NOTIFY and reconcile pass %s = %s from the crossing request to the revoke", ledgerTx, reconcilePass, total))
		// The measured window ends when the gateway driver receives the zero limit. Then
		// internal/gateway/kubernetes/render.go drops the key from the ConfigMap (covered by
		// render_test.go). The ConfigMap write and the agentgateway hot reload add to the real window.
		// The gateway keeps serving until the revoke, so this window (times the spend rate) is the
		// overshoot beyond the one in-flight response of ADR 0097. The bound is generous for a loaded
		// CI host and far below the one-hour resync, which proves the NOTIFY path did the waking.
		Expect(total).To(BeNumerically("<", 5*time.Second))
		Expect(mg.last().Limits[ref].Amount).To(BeZero())
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
		Expect(agentKeys(mg.last())).To(HaveLen(3))
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
			coolDown(ctx, org, a.ID, b.ID)
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
			coolDown(ctx, org, a.ID)
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

		It("names agentgateway's reason on the agents it holds back when the whole config is refused (old issue 245)", func(ctx SpecContext) {
			a := newAgent(ctx, org, user)
			r := newReconciler()
			pass(ctx, r)
			coolDown(ctx, org, a.ID)
			mg.fail = &gateway.ConfigRejectedError{Revision: "abc", Reason: "USD budget limits support at most 9 fractional digits"}
			fresh := newAgent(ctx, org, user)
			advance(11 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(MatchError(gateway.ErrConfigRejected))
			Expect(osg.Calls("CreateSandbox")).To(Equal(1), "the fresh agent's key is not live")
			msg := sandboxRow(ctx, fresh.ID).Message
			Expect(msg).To(ContainSubstring("waiting for the gateway"))
			Expect(msg).To(ContainSubstring("USD budget limits support at most 9 fractional digits"))
			Expect(sandboxRow(ctx, a.ID).State).To(Equal(store.SandboxStateStopped), "an agent with a live key is still converged")

			By("a good file loads")
			mg.fail = nil
			now.Store(nil)
			pass(ctx, r)
			Expect(sandboxRow(ctx, fresh.ID).State).To(Equal(store.SandboxStateReady))
		})

		It("keeps a key the gateway refused not live and holds its agent back, while served keys are converged (old issue 245)", func(ctx SpecContext) {
			a := newAgent(ctx, org, user)
			r := newReconciler()
			pass(ctx, r)
			coolDown(ctx, org, a.ID)
			served := map[string]bool{}
			for _, k := range mg.last().Keys {
				served[k.ID] = true
			}
			fresh := newAgent(ctx, org, user)
			// The gateway judges the new key alone: everything else of the state is live.
			mg.onApply = func(s gateway.State) {
				for _, k := range s.Keys {
					if !served[k.ID] {
						mg.fail = &gateway.RejectedError{Rejected: []gateway.Rejection{
							{Kind: "Key", Name: k.ID, Reason: "USD budget limits support at most 9 fractional digits"},
						}}
					}
				}
			}
			advance(11 * time.Minute)
			Expect(r.ReconcileAll(ctx)).To(HaveOccurred())
			Expect(osg.Calls("CreateSandbox")).To(Equal(1), "the refused key is not live: no sandbox for the fresh agent")
			Expect(sandboxRow(ctx, fresh.ID).Message).To(ContainSubstring("waiting for the gateway"))
			Expect(sandboxRow(ctx, a.ID).State).To(Equal(store.SandboxStateStopped), "the live key's agent is converged")

			By("the key is accepted")
			mg.onApply, mg.fail = nil, nil
			now.Store(nil)
			pass(ctx, r)
			Expect(sandboxRow(ctx, fresh.ID).State).To(Equal(store.SandboxStateReady))
		})

		It("holds back every agent after a restart until one Apply has succeeded", func(ctx SpecContext) {
			a := newAgent(ctx, org, user)
			pass(ctx, newReconciler())
			coolDown(ctx, org, a.ID)
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
		coolDown(ctx, org, agent.ID)
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

	// Issue 573: one agent's slow backend call must not hold back another agent. A pass hands the
	// sandbox work to a per-agent queue and does not wait for it.
	It("creates a new agent's sandbox while another agent's sandbox delete is still open", func(ctx SpecContext) {
		wsName := execution.WorkspaceName(org.Bytes)
		agentA := newAgent(ctx, org, user)
		cfg.ResyncInterval = time.Hour // only NOTIFY wakes the loop
		pass(ctx, newReconciler())
		namesA := execution.AgentObjectNames(org.Bytes, agentA.ID.Bytes)
		Expect(osg.Sandbox(wsName, namesA.Sandbox)).NotTo(BeNil())

		By("a new image makes the loop delete agent A's sandbox to recreate it; the delete stays open")
		release := osg.BlockDeletes()
		DeferCleanup(release)
		cfg.Harnesses[harness.OpenCode] = reconciler.Harness{Image: image + "-2", Binaries: reconciler.DefaultBinaries[harness.OpenCode]}
		r := newReconciler()
		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Run(runCtx) }()
		DeferCleanup(func() { release(); cancel(); Eventually(done).Should(Receive(BeNil())) })
		Eventually(func() int { return osg.Calls("DeleteSandbox") }).WithTimeout(10 * time.Second).Should(Equal(1))

		By("agent B is created during that delete: its sandbox exists before the delete returns")
		agentB := newAgent(ctx, org, user)
		namesB := execution.AgentObjectNames(org.Bytes, agentB.ID.Bytes)
		Eventually(func() *openshellv1.Sandbox { return osg.Sandbox(wsName, namesB.Sandbox) }).WithTimeout(10 * time.Second).ShouldNot(BeNil())
		Expect(osg.Sandbox(wsName, namesA.Sandbox)).NotTo(BeNil(), "agent A's delete has not returned")
		Expect(logs.String()).To(ContainSubstring("requesting the deletion of a sandbox"), "the delete is logged before the call")

		By("the delete returns and agent A is recreated with the new image")
		release()
		Eventually(func() string {
			return osg.Sandbox(wsName, namesA.Sandbox).GetSpec().GetTemplate().GetImage()
		}).WithTimeout(10 * time.Second).Should(Equal(image + "-2"))
	})

	Describe("the leader lock (#573)", func() {
		const leaderKey int64 = 0x6b6f696e6f6e7263
		// holdLock takes the reconciler's session lock on its own connection, as a running leader
		// does, and returns the function that releases it.
		holdLock := func(ctx context.Context) (release func()) {
			GinkgoHelper()
			c, err := pool.Acquire(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Exec(ctx, "SELECT pg_advisory_lock($1)", leaderKey)
			Expect(err).NotTo(HaveOccurred())
			var once sync.Once
			release = func() {
				once.Do(func() { _ = c.Conn().Close(context.Background()); c.Release() })
			}
			DeferCleanup(release)
			return release
		}
		// run starts r.Run and returns a stop function that waits for Run to end.
		run := func(r *reconciler.Reconciler) (stop func() time.Duration) {
			runCtx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(runCtx) }()
			var stopped bool
			stop = func() time.Duration {
				if stopped {
					return 0
				}
				stopped = true
				t := time.Now()
				cancel()
				Eventually(done).Should(Receive(BeNil()))
				return time.Since(t)
			}
			DeferCleanup(func() { stop() })
			return stop
		}
		leading := func() int { return strings.Count(logs.String(), "reconciler is leading") }
		// lockRows counts the advisory lock rows for key in this spec's database: pg_locks covers the
		// whole server, and suites that share one server use the same key in their own databases.
		lockRows := func(ctx context.Context, key int64, granted bool) int {
			GinkgoHelper()
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted = $2
				AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND ((classid::bigint << 32) | objid::bigint) = $1`, key, granted).Scan(&n)).To(Succeed())
			return n
		}
		holders := func(ctx context.Context) int { return lockRows(ctx, leaderKey, true) }

		It("takes over within about 2 s of the holder releasing the lock, with a long resync interval", func(ctx SpecContext) {
			cfg.ResyncInterval = time.Hour
			release := holdLock(ctx)
			run(newReconciler())
			Eventually(logs.String).Should(ContainSubstring("waiting for the leader lock"))
			Consistently(leading, "500ms", "50ms").Should(BeZero())

			t := time.Now()
			release()
			Eventually(leading).WithTimeout(2 * time.Second).Should(Equal(1))
			GinkgoWriter.Printf("leader hand-over took %s\n", time.Since(t))
			Expect(logs.String()).To(MatchRegexp(`reconciler is leading" waited=\S+`))
		})

		It("ends a blocked wait promptly when its context is cancelled, and does not take the lock afterwards", func(ctx SpecContext) {
			cfg.ResyncInterval = time.Hour
			release := holdLock(ctx)
			stop := run(newReconciler())
			Eventually(logs.String).Should(ContainSubstring("waiting for the leader lock"))

			Expect(stop()).To(BeNumerically("<", 2*time.Second))
			release()
			// No orphaned backend takes the lock once the holder is gone.
			Consistently(func() int { return holders(ctx) }, "1s", "100ms").Should(BeZero())
			Expect(leading()).To(BeZero())
		})

		It("never has two leaders, and the follower leads after the leader stops", func(ctx SpecContext) {
			cfg.ResyncInterval = time.Hour
			stopFirst := run(newReconciler())
			Eventually(leading).Should(Equal(1))
			run(newReconciler())
			Eventually(func() int { return strings.Count(logs.String(), "waiting for the leader lock") }).Should(Equal(2))
			Consistently(func() int { return holders(ctx) }, "1s", "100ms").Should(Equal(1))
			Expect(leading()).To(Equal(1))

			stopFirst()
			Eventually(leading).WithTimeout(2 * time.Second).Should(Equal(2))
			Expect(holders(ctx)).To(Equal(1))
		})

		It("cancels a blocked lock wait on the server, so no backend takes the lock later", func(ctx SpecContext) {
			key := leaderKey + seq.Add(1)
			holder, err := pool.Acquire(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, err = holder.Exec(ctx, "SELECT pg_advisory_lock($1)", key)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = holder.Conn().Close(context.Background()); holder.Release() })
			waiter, err := pool.Acquire(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = waiter.Conn().Close(context.Background()); waiter.Release() })

			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			res := make(chan error, 1)
			go func() { res <- reconciler.LockBlocking(wctx, waiter.Conn(), key) }()
			Eventually(func() int { return lockRows(ctx, key, false) }).Should(Equal(1), "the waiter is queued on the server")

			cancel()
			Eventually(res).WithTimeout(2 * time.Second).Should(Receive(MatchError(context.Canceled)))
			// The server-side wait is gone: a closed socket alone would leave the backend queued.
			Eventually(func() int { return lockRows(ctx, key, false) }).Should(BeZero())
			_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", key)
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() int { return lockRows(ctx, key, true) }, "500ms", "50ms").Should(BeZero())
		})

		It("cancels a running pass when the leader's connection is lost, before the follower leads", func(ctx SpecContext) {
			cfg.ResyncInterval = time.Hour
			mg.mu.Lock()
			mg.hold, mg.entered = make(chan struct{}), make(chan struct{})
			entered := mg.entered
			mg.mu.Unlock()
			DeferCleanup(func() {
				mg.mu.Lock()
				defer mg.mu.Unlock()
				if mg.hold != nil {
					close(mg.hold)
				}
			})
			run(newReconciler())
			Eventually(entered).Should(BeClosed(), "the leader's pass is inside Apply")
			// Later passes do not wait.
			mg.mu.Lock()
			mg.hold, mg.entered = nil, nil
			mg.mu.Unlock()
			run(newReconciler())
			Eventually(func() int { return strings.Count(logs.String(), "waiting for the leader lock") }).Should(Equal(2))

			var terminated int
			Expect(pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(l.pid)) FROM pg_locks l
				WHERE l.locktype = 'advisory' AND l.granted
				AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND ((l.classid::bigint << 32) | l.objid::bigint) = $1`, leaderKey).Scan(&terminated)).To(Succeed())
			Expect(terminated).To(Equal(1))

			Eventually(mg.canceledAt).WithTimeout(2 * time.Second).ShouldNot(BeZero())
			Eventually(leading).WithTimeout(3 * time.Second).Should(Equal(2))
			Eventually(mg.applyTimes).Should(HaveLen(2))
			// The order of the old pass's cancel and the new leader's first write is NOT guaranteed:
			// the server frees the lock when the backend dies, and the old leader learns of the dead
			// connection by its own read, so the follower can write first. That two-leader window is
			// tracked in issue 635.
			Expect(mg.appliedCount()).To(Equal(1), "only the new leader's pass finished")
			Consistently(mg.appliedCount, "500ms", "50ms").Should(Equal(1))
		})

		It("retries a failed lead after a short backoff with jitter, not after the resync interval", func() {
			Expect(reconciler.RetryDelay(1, 0)).To(Equal(reconciler.RetryBase))
			Expect(reconciler.RetryDelay(1, 0.999)).To(BeNumerically("<", 2*reconciler.RetryBase))
			Expect(reconciler.RetryDelay(2, 0)).To(Equal(2 * reconciler.RetryBase))
			Expect(reconciler.RetryDelay(50, 0.999)).To(BeNumerically("<", 2*reconciler.RetryCap))
			Expect(reconciler.RetryDelay(50, 0)).To(Equal(reconciler.RetryCap))
			Expect(reconciler.RetryCap).To(BeNumerically("<", 30*time.Second))
		})

		It("backs off further on a repeating setup failure such as LISTEN, and resets only after leading", func() {
			failures, prev := 0, time.Duration(0)
			for range 4 {
				failures = reconciler.NextFailures(failures, false) // LISTEN failed: not led
				d := reconciler.RetryDelay(failures, 0)
				Expect(d).To(BeNumerically(">", prev))
				prev = d
			}
			Expect(prev).To(Equal(8 * reconciler.RetryBase))
			Expect(reconciler.NextFailures(failures, true)).To(Equal(1), "a lead that ran passes starts over")
		})
	})
})

// agentKeys are the state's keys of agents: the system keys of the orgs (the model-check key of an
// org that serves a priced model) are not part of what these specs are about.
func agentKeys(st gateway.State) []gateway.Key {
	var out []gateway.Key
	for _, k := range st.Keys {
		if !gateway.IsSystemAgent(k.Agent) {
			out = append(out, k)
		}
	}
	return out
}

// fakeSupervisor is a reconciler.SupervisorSource a test moves.
type fakeSupervisor struct {
	image string
	err   error
}

func (f *fakeSupervisor) SupervisorImage(context.Context) (string, error) { return f.image, f.err }
