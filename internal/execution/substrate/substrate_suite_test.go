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
)

func TestSubstrate(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Substrate Driver Suite")
}

const ws = "org-acme"

// noContentDigest makes newDriver leave Config.ContentDigest nil, for the specs of the fail-closed rule.
var noContentDigest bool

// newDriver starts a fake Substrate and returns a driver on it, with the workspace made.
func newDriver(cfg ...substrate.Config) (*substrate.Driver, *fakeate.Cluster) {
	cl := fakeate.New()
	control, router, err := cl.Start()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(cl.Stop)
	DeferCleanup(control.Close)
	DeferCleanup(router.Close)
	c := substrate.Config{PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second}
	if len(cfg) > 0 {
		c = cfg[0]
	}
	if c.ContentDigest == nil && !noContentDigest {
		// The fake has no object store: the object's content is its address.
		c.ContentDigest = func(_ context.Context, uri string) (string, error) { return "fake-" + uri, nil }
	}
	d := substrate.New(control, router, c)
	_, err = d.EnsureWorkspace(context.Background(), ws, nil)
	Expect(err).NotTo(HaveOccurred())
	return d, cl
}

func spec(name string) execution.SandboxSpec {
	return execution.SandboxSpec{Name: name, Image: "img"}
}

// ready creates a sandbox and waits until it is ready.
func ready(ctx context.Context, d *substrate.Driver, name string) {
	_, err := d.CreateSandbox(ctx, ws, spec(name))
	Expect(err).NotTo(HaveOccurred())
	_, err = d.WaitPhase(ctx, ws, name, execution.PhaseReady)
	Expect(err).NotTo(HaveOccurred())
}
