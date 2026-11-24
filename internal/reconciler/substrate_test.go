// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"log/slog"
	"slices"
	"sync"
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
const substrateImage = "ghcr.io/stateracloud/koinon/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// labelKeeper stands in for the sandbox labels that Agent Substrate does not have: the reconciler
// compares the LabelSpecHash of the sandbox with the hash of its spec, and the Substrate driver
// returns no labels, so without this every pass would delete and re-create the sandbox. The gap is
// a finding of issue #1245 (see its hand-back); remove this wrapper when the driver or the
// reconciler has a durable spec identity for a driver without labels.
type labelKeeper struct {
	execution.Backend
	mu     sync.Mutex
	labels map[string]map[string]string
}

func (k *labelKeeper) with(sb execution.Sandbox, err error) (execution.Sandbox, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if l, ok := k.labels[sb.Name]; ok {
		sb.Labels = l
	}
	return sb, err
}

func (k *labelKeeper) CreateSandbox(ctx context.Context, ws string, spec execution.SandboxSpec) (execution.Sandbox, error) {
	sb, err := k.Backend.CreateSandbox(ctx, ws, spec)
	if err == nil {
		k.mu.Lock()
		k.labels[spec.Name] = spec.Labels
		k.mu.Unlock()
	}
	return k.with(sb, err)
}
func (k *labelKeeper) GetSandbox(ctx context.Context, ws, name string) (execution.Sandbox, error) {
	return k.with(k.Backend.GetSandbox(ctx, ws, name))
}
func (k *labelKeeper) StartSandbox(ctx context.Context, ws, name string) (execution.Sandbox, error) {
	return k.with(k.Backend.StartSandbox(ctx, ws, name))
}
func (k *labelKeeper) StopSandbox(ctx context.Context, ws, name string) (execution.Sandbox, error) {
	return k.with(k.Backend.StopSandbox(ctx, ws, name))
}

// The reconciler against the fake Substrate (issue #1245): the spec it renders must be one that the
// Substrate driver accepts, as the production wiring (execution.WithoutEgress around the driver) gives it.
var _ = Describe("Reconciler on the Substrate driver", func() {
	var (
		cl   *fakeate.Cluster
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
		d := substrate.New(control, router, substrate.Config{
			PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond,
			// The class the reconciler requests (reconciler.Config.RuntimeClassName) has a tier.
			RuntimeClassTiers: map[string]harness.Tier{"kata": harness.TierVM},
		})
		now.Store(nil)
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		r, err = reconciler.New(pool, &labelKeeper{Backend: execution.WithoutEgress(d), labels: map[string]map[string]string{}}, &fakeModelGateway{},
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

		By("the template carries no provider, no network rule and no filesystem policy: the guest launcher holds the fixed policy")
		tmpl, ok := cl.Template(ws, cl.TemplateOf(ws, names.Sandbox))
		Expect(ok).To(BeTrue())
		Expect(tmpl.String()).NotTo(ContainSubstring("provider"))

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

	// The launcher applies one fixed policy to every guest process (D1); the reconciler's
	// SandboxPolicy is what OpenShell enforces per sandbox. The two cover the same paths, so the fixed
	// policy cannot get narrower or wider than the per-sandbox one unseen (issue #1245).
	It("has a launcher default policy over the same paths as the per-sandbox filesystem policy", func() {
		fs := reconciler.SandboxPolicy(nil).Filesystem
		lp := guestlaunch.DefaultPolicy().Landlock
		// Paths that only the launcher names: the guest is a full Linux root, not an OpenShell
		// supervisor view. Each needs a reason; a new entry here is a decision.
		launcherOnly := map[string]string{
			"/sbin": "same tree as /bin on the guest images", "/lib32": "same tree as /lib",
			"/sys": "read by runtimes for cpu and memory limits", "/run": "runtime sockets and pid files",
			"/var/lib": "package and tool state, read only", "/var/tmp": "scratch, like /tmp",
			"/home": "agent home", "/root": "agent home when the tier runs as root", "/var/cache": "tool caches",
			"/sandbox": "the working directory (IncludeWorkdir)",
		}
		// Paths that only the per-sandbox policy names: none. /dev entries are device lists in the launcher.
		perSandboxOnly := map[string]string{
			"/dev/urandom": "device list", "/dev/null": "device list",
		}
		launcher := map[string]bool{}
		for _, p := range append(append(append([]string{}, lp.ReadExec...), lp.ReadWrite...), lp.Devices...) {
			launcher[p] = true
		}
		sandbox := map[string]bool{"/sandbox": fs.IncludeWorkdir}
		for _, p := range append(append([]string{}, fs.ReadOnly...), fs.ReadWrite...) {
			sandbox[p] = true
		}
		for p := range sandbox {
			if _, devs := perSandboxOnly[p]; devs {
				Expect(lp.Devices).To(ContainElement(p), "device %s must be in the launcher's device list", p)
				continue
			}
			Expect(launcher).To(HaveKey(p), "%s is in the per-sandbox policy but not in the launcher default policy", p)
		}
		for p := range launcher {
			if _, ok := launcherOnly[p]; ok {
				continue
			}
			if _, isDev := perSandboxOnly[p]; isDev {
				continue
			}
			if len(p) >= 5 && p[:5] == "/dev/" {
				continue // the launcher lists every device it allows, OpenShell names two
			}
			Expect(sandbox).To(HaveKey(p), "%s is in the launcher default policy but not in the per-sandbox policy: add it there or to launcherOnly with a reason", p)
		}
		// Access modes: a path read-only per sandbox is read-only in the launcher too. /var/log is
		// the one known difference today: the launcher lets the guest write it (policy-vm.json and
		// policy-gvisor.json carry the same). Listing it here keeps the difference visible: a new one
		// fails, and so does the removal of this one without removing the entry.
		widened := []string{}
		for _, p := range fs.ReadOnly {
			if slices.Contains(lp.ReadWrite, p) {
				widened = append(widened, p)
			}
		}
		Expect(widened).To(Equal([]string{"/var/log"}), "paths read-only per sandbox but writable in the launcher")
	})
})
