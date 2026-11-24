// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	"github.com/stateracloud/koinon/internal/guestlaunch"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

// substrateImage is pinned by digest: the Substrate driver refuses any other reference. The digest
// is a fake, the fakeate never pulls.
const substrateImage = "registry.example.com/koinon/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// The reconciler against the fake Substrate (issue #1245): the spec it renders must be one that the
// Substrate driver accepts, as the production wiring (execution.WithoutEgress around the driver) gives it.
var _ = Describe("Reconciler on the Substrate driver", func() {
	var (
		cl   *fakeate.Cluster
		d    *substrate.Driver
		org  pgtype.UUID
		user pgtype.UUID
		r    *reconciler.Reconciler
		now  atomic.Pointer[time.Time]
		ws   string
	)
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}
	advance := func(d time.Duration) {
		t := time.Now().Add(d)
		now.Store(&t)
	}

	BeforeEach(func(ctx SpecContext) {
		cl = fakeate.New()
		control, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cl.Stop)
		DeferCleanup(control.Close)
		DeferCleanup(router.Close)
		d = substrate.New(control, router, substrate.Config{
			PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond,
			// The class the reconciler requests (reconciler.Config.RuntimeClassName) has a tier.
			RuntimeClassTiers: map[string]harness.Tier{"kata": harness.TierVM},
		})
		now.Store(nil)
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		r, err = reconciler.New(pool, execution.WithoutEgress(d), &fakeModelGateway{},
			reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
				Prices:         prices.NewResolver(pricing.Embedded()),
				Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
				Keys:           ring,
				ProviderKeys:   reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
				ProviderKeyDir: "/secrets/providers",
				URLPolicy:      testPolicy,
				Harnesses: map[string]reconciler.Harness{
					harness.OpenCode: {Image: substrateImage, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
				},
				RuntimeClassName: "kata",
				Logger:           slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
				Now: func() time.Time {
					if t := now.Load(); t != nil {
						return *t
					}
					return time.Now()
				},
			})
		Expect(err).NotTo(HaveOccurred())
	})

	It("creates an actor, suspends it when idle and resumes it for a run", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		pass(ctx)
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, names.Sandbox) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		Expect(cl.Calls("CreateActor")).To(Equal(1))

		By("an idle sandbox is suspended")
		coolDown(ctx, org, agent.ID)
		advance(9 * time.Minute)
		pass(ctx)
		Expect(cl.State(ws, names.Sandbox)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		advance(11 * time.Minute)
		pass(ctx)
		Expect(cl.State(ws, names.Sandbox)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))

		By("a queued run resumes it")
		now.Store(nil)
		queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, names.Sandbox) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		Expect(cl.Calls("CreateActor")).To(Equal(1), "resumed, not re-created")
	})

	It("the adapter drops provider records, network rules and a filesystem policy; the plain driver refuses them", func(ctx SpecContext) {
		withEgress := execution.SandboxSpec{
			Name: "egress-probe", Image: substrateImage, RuntimeClassName: "kata", Providers: []string{"upstream"},
			Policy: execution.Policy{
				Filesystem: execution.FilesystemPolicy{ReadWrite: []string{"/work"}},
				Network:    map[string]execution.NetworkRule{"github": {Binaries: []string{"/usr/bin/git"}}},
			},
		}
		_, err := d.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = d.CreateSandbox(ctx, ws, withEgress)
		Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "plain driver: %v", err)
		_, err = execution.WithoutEgress(d).CreateSandbox(ctx, ws, withEgress)
		Expect(err).NotTo(HaveOccurred())
	})

	It("recreates nothing on later passes", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		pass(ctx)
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, names.Sandbox) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		pass(ctx)
		pass(ctx)
		Expect(cl.Calls("CreateActor")).To(Equal(1), "the Substrate driver keeps no labels: the spec identity must hold across passes")
		Expect(cl.Calls("DeleteActor")).To(BeZero())
	})
})

// The launcher applies one fixed policy to every guest process (D1); the reconciler's SandboxPolicy
// is what OpenShell enforces per sandbox. The two cover the same paths, so the fixed policy cannot
// get narrower or wider than the per-sandbox one unseen (issue #1245). The guest runs the policy
// file of its tier ([harness.LauncherPrefix]), which replaces the lists of the default policy as a
// whole, so the files are compared too.
var _ = Describe("The launcher policy and the per-sandbox filesystem policy", func() {
	load := func(name string) guestlaunch.LandlockPolicy {
		f, err := os.Open("../../cmd/koinon-guest-launcher/" + name)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(f.Close)
		p, err := guestlaunch.ParsePolicy(f)
		Expect(err).NotTo(HaveOccurred())
		return p.Landlock
	}
	DescribeTable("cover the same paths with the same access",
		func(get func() guestlaunch.LandlockPolicy) {
			lp := get()
			fs := reconciler.SandboxPolicy(nil).Filesystem
			// Paths that only the launcher names: the guest is a full Linux root, not an OpenShell
			// supervisor view. Each needs a reason; a new entry here is a decision.
			launcherOnly := map[string]string{
				"/sbin": "same tree as /bin on the guest images", "/lib32": "same tree as /lib",
				"/sys": "read by runtimes for cpu and memory limits", "/run": "runtime sockets and pid files",
				"/var/lib": "package and tool state, read only", "/var/tmp": "scratch, like /tmp",
				"/home": "agent home", "/root": "agent home when the tier runs as root", "/var/cache": "tool caches",
				"/sandbox": "the working directory (IncludeWorkdir)",
			}
			// OpenShell names two device nodes; the launcher lists every device it allows, one by one.
			perSandboxDevices := map[string]bool{"/dev/urandom": true, "/dev/null": true}
			launcher := map[string]bool{}
			for _, p := range slices.Concat(lp.ReadExec, lp.ReadWrite, lp.Devices) {
				launcher[p] = true
			}
			sandbox := map[string]bool{"/sandbox": fs.IncludeWorkdir}
			for _, p := range slices.Concat(fs.ReadOnly, fs.ReadWrite) {
				sandbox[p] = true
			}
			for p := range sandbox {
				if perSandboxDevices[p] {
					Expect(lp.Devices).To(ContainElement(p), "device %s must be in the launcher's device list", p)
					continue
				}
				Expect(launcher).To(HaveKey(p), "%s is in the per-sandbox policy but not in the launcher policy", p)
			}
			for p := range launcher {
				if _, ok := launcherOnly[p]; ok || perSandboxDevices[p] || strings.HasPrefix(p, "/dev/") {
					continue
				}
				Expect(sandbox).To(HaveKey(p), "%s is in the launcher policy but not in the per-sandbox policy: add it there or to launcherOnly with a reason", p)
			}
			// Access, both ways. Writable per sandbox must be writable in the launcher (a narrower
			// policy breaks the agent); read-only per sandbox must not be writable there, but for the
			// known difference: the launcher lets the guest write /var/log. Listing it keeps the
			// difference visible: a new one fails, and so does the removal of this one.
			writable := slices.Concat(lp.ReadWrite, lp.Devices)
			rw := slices.Clone(fs.ReadWrite)
			if fs.IncludeWorkdir {
				rw = append(rw, "/sandbox")
			}
			for _, p := range rw {
				Expect(writable).To(ContainElement(p), "%s is read-write per sandbox but not writable in the launcher policy", p)
			}
			widened := []string{}
			for _, p := range fs.ReadOnly {
				if slices.Contains(lp.ReadWrite, p) {
					widened = append(widened, p)
				}
			}
			Expect(widened).To(Equal([]string{"/var/log"}), "paths read-only per sandbox but writable in the launcher")
		},
		Entry("default policy", func() guestlaunch.LandlockPolicy { return guestlaunch.DefaultPolicy().Landlock }),
		Entry("policy-vm.json", func() guestlaunch.LandlockPolicy { return load("policy-vm.json") }),
		Entry("policy-gvisor.json", func() guestlaunch.LandlockPolicy { return load("policy-gvisor.json") }),
	)
})
