// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/harness"
)

func TestSubstrate(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Substrate Driver Suite")
}

const ws = "org-acme"

// noContentDigest makes newDriver leave Config.ContentDigest nil, for the specs of the fail-closed rule.
var noContentDigest bool

// noGatewayRoutes makes newDriver leave Config.GatewayRoutes nil, for the specs of the egress policy's fail-closed rule.
var noGatewayRoutes bool

// noTier makes newDriver leave the tier settings empty, for the specs of the launcher's fail-closed rule.
var noTier bool

// noActorNames makes newDriver leave Config.ActorNames nil, for the spec of the name ledger's fail-closed rule.
var noActorNames bool

// noSigner makes newDriver leave Config.GuestSigner nil.
var noSigner bool

// unguarded makes newDriver start a fake Substrate whose guest has no guest front.
var unguarded bool

// latency is the boot and resume time of the fake's actors (fakeate.Cluster.Latency; zero is 1 ms).
var latency time.Duration

// lastControl is the control connection of the fake that the latest newDriver started: a spec that checks the
// admission of the fake calls the control API through it.
var lastControl ateapiv1.ControlClient

// newDriver starts a fake Substrate and returns a driver on it, with the workspace made.
func newDriver(cfg ...substrate.Config) (*substrate.Driver, *fakeate.Cluster) {
	cl := fakeate.New()
	cl.Unguarded = unguarded
	cl.Latency = latency
	control, router, err := cl.Start()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(cl.Stop)
	DeferCleanup(control.Close)
	lastControl = ateapiv1.NewControlClient(control)
	DeferCleanup(router.Close)
	d := substrate.New(control, router, specConfig(cfg...))
	_, err = d.EnsureWorkspace(context.Background(), ws, nil)
	Expect(err).NotTo(HaveOccurred())
	return d, cl
}

// specConfig is the driver configuration of the specs: cfg (or the spec defaults) with every required field
// that it leaves empty filled, unless a no* switch keeps it empty.
func specConfig(cfg ...substrate.Config) substrate.Config {
	c := substrate.Config{PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond}
	if len(cfg) > 0 {
		c = cfg[0]
	}
	if c.ActorNames == nil && !noActorNames {
		c.ActorNames = fakeate.NewActorNames()
	}
	if c.DefaultTier == "" && c.RuntimeClassTiers == nil && !noTier {
		c.DefaultTier = harness.TierDev
		c.RuntimeClassTiers = map[string]harness.Tier{"kata": harness.TierVM, "gvisor": harness.TierHardened}
	}
	if c.GuestSigner == nil && !noSigner {
		c.GuestSigner = fakeate.Signer()
	}
	if c.GatewayRoutes == nil && !noGatewayRoutes {
		c.GatewayRoutes = gatewayRoutes
		c.CodeHostEgressRoute = ptr(substrate.EgressRoute(fakeate.GatewayHost, fakeate.EgressPort))
	}
	if c.ContentDigest == nil && !noContentDigest {
		// The fake has no object store: the object's content is its address.
		c.ContentDigest = func(_ context.Context, uri string) (string, error) { return "fake-" + uri, nil }
	}
	return c
}

// gatewayRoutes is the one gateway route of the specs' driver.
var gatewayRoutes = []substrate.GatewayRoute{substrate.ModelRoute(fakeate.GatewayHost, fakeate.GatewayPort)}

// img is the actor image of the specs: the driver refuses one that is not pinned by digest.
const img = "img" + digest

func spec(name string) execution.SandboxSpec {
	// Substrate refuses an actor template without resources (issue #1092).
	return execution.SandboxSpec{Name: name, Image: img, CPULimit: "500m", MemoryLimit: "512Mi"}
}

// ready creates a sandbox and waits until it is ready.
func ready(ctx context.Context, d *substrate.Driver, name string) {
	_, err := d.CreateSandbox(ctx, ws, spec(name))
	Expect(err).NotTo(HaveOccurred())
	_, err = d.WaitPhase(ctx, ws, name, execution.PhaseReady)
	Expect(err).NotTo(HaveOccurred())
}
