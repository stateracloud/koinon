// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/preflight"
	"github.com/stateracloud/koinon/internal/substrateauthz"
	"github.com/stateracloud/koinon/internal/substrateinstall"
)

const bootstrapOwner = "system:serviceaccount:ate-system:ate-client"

// grantFake is a fake Substrate that enforces the access model, played as its bootstrap owner (ate-client).
type grantFake struct {
	cl    *fakeate.Cluster
	opens int
}

// grantCluster starts a fake Substrate. policy seeds the global policy; nil leaves it missing.
func grantCluster(policy []*ateapiv1.Binding) *grantFake {
	cl := fakeate.New()
	cl.EnforceAuthz(bootstrapOwner)
	if policy != nil {
		cl.SetGlobalPolicy(policy...)
	}
	if _, _, err := cl.Start(); err != nil {
		panic(err)
	}
	return &grantFake{cl: cl}
}

func (f *grantFake) dial(context.Context) (ateapiv1.ControlClient, func(), error) {
	f.opens++
	c, err := f.cl.ControlAs(bootstrapOwner)
	if err != nil {
		return nil, nil, err
	}
	return ateapiv1.NewControlClient(c), func() { _ = c.Close() }, nil
}

func installerWith(f *grantFake, out *bytes.Buffer) *substrateinstall.Installer {
	i, _, _ := setup("37", true, nil)
	i.Out = out
	i.Control = f.dial
	return i
}

const provisionerMember = "user:system:serviceaccount:koinon:koinon-provisioner"

// An upgrade of an install from before the provisioner adds the grant and keeps the members that exist.
func TestGrantKeepsTheOtherMembersOfTheGlobalPolicy(t *testing.T) {
	g := NewWithT(t)
	f := grantCluster([]*ateapiv1.Binding{
		{Role: "owner", Members: []string{"user:alice"}},
		{Role: "viewer", Members: []string{"user:bob", "user:system:serviceaccount:other:thing"}},
	})
	defer f.cl.Stop()
	var out bytes.Buffer
	changed, err := installerWith(f, &out).EnsureProvisionerGrant(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	p := f.cl.Policy("")
	g.Expect(substrateauthz.HasMember(p, "owner", provisionerMember)).To(BeTrue())
	g.Expect(substrateauthz.HasMember(p, "owner", "user:alice")).To(BeTrue())
	g.Expect(substrateauthz.HasMember(p, "viewer", "user:bob")).To(BeTrue())
	g.Expect(substrateauthz.HasMember(p, "viewer", "user:system:serviceaccount:other:thing")).To(BeTrue())
	g.Expect(out.String()).To(ContainSubstring(provisionerMember))
}

// A second run (every upgrade) changes nothing, and the grant is the provisioner only: nothing else of Koinon.
func TestGrantIsIdempotentAndGrantsNothingElse(t *testing.T) {
	g := NewWithT(t)
	f := grantCluster(nil)
	defer f.cl.Stop()
	i := installerWith(f, &bytes.Buffer{})
	_, err := i.EnsureProvisionerGrant(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	changed, err := i.EnsureProvisionerGrant(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeFalse())
	g.Expect(f.cl.Calls("UpdateGlobalAccessPolicy")).To(BeZero())
	p := f.cl.Policy("")
	g.Expect(p.GetBindings()).To(HaveLen(1))
	g.Expect(p.GetBindings()[0].GetMembers()).To(Equal([]string{provisionerMember}))
}

// The api server restarts after the authentication change: the grant waits for it, up to the install's timeout.
func TestGrantWaitsForTheAPIServer(t *testing.T) {
	g := NewWithT(t)
	f := grantCluster(nil)
	defer f.cl.Stop()
	i := installerWith(f, &bytes.Buffer{})
	tries := 0
	i.Control = func(ctx context.Context) (ateapiv1.ControlClient, func(), error) {
		if tries++; tries < 3 {
			return nil, nil, status.Error(codes.Unavailable, "no ready ate-api-server pod")
		}
		return f.dial(ctx)
	}
	changed, err := i.EnsureProvisionerGrant(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(tries).To(Equal(3))
}

func TestGrantGivesUpWhenTheAPIServerNeverComes(t *testing.T) {
	g := NewWithT(t)
	f := grantCluster(nil)
	defer f.cl.Stop()
	i := installerWith(f, &bytes.Buffer{})
	i.WaitTimeout, i.PollInterval = 20*time.Millisecond, time.Millisecond
	i.Control = func(context.Context) (ateapiv1.ControlClient, func(), error) {
		return nil, nil, status.Error(codes.Unavailable, "down")
	}
	_, err := i.EnsureProvisionerGrant(t.Context())
	g.Expect(err).To(MatchError(ContainSubstring(provisionerMember)))
}

// A dial that hangs does not hold the grant past WaitTimeout: the deadline reaches the dial.
func TestGrantIsBoundedByTheWaitTimeoutWhenTheDialHangs(t *testing.T) {
	g := NewWithT(t)
	f := grantCluster(nil)
	defer f.cl.Stop()
	i := installerWith(f, &bytes.Buffer{})
	i.WaitTimeout, i.PollInterval = 50*time.Millisecond, time.Millisecond
	i.Control = func(ctx context.Context) (ateapiv1.ControlClient, func(), error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	start := time.Now()
	_, err := i.EnsureProvisionerGrant(t.Context())
	g.Expect(err).To(MatchError(context.DeadlineExceeded))
	g.Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))
}

// Install runs the grant, so every install and every upgrade has it; it is the only Koinon member of the global policy.
func TestInstallGrantsTheProvisioner(t *testing.T) {
	g := NewWithT(t)
	f := grantCluster([]*ateapiv1.Binding{{Role: "owner", Members: []string{"user:alice"}}})
	defer f.cl.Stop()
	var crds []runtime.Object
	for _, n := range preflight.SubstrateCRDs {
		crds = append(crds, crd(n, true))
	}
	i, r, _ := setup("37", true, []runtime.Object{defaultClass("local"), router(), atelet()}, crds...)
	i.Profile = genericProfile()
	r.tarFiles = pinnedKindFiles()
	i.Out = io.Discard
	store, fetch := stageEnv(t)
	i.GVisorFetcher = fetch
	i.GVisorStore = func(context.Context) (substrateinstall.AssetStore, func(), error) { return store, func() {}, nil }
	i.Control = f.dial
	// The full preflight after the install fails (no Substrate api server in the fake cluster); the grant ran before it.
	g.Expect(i.Install(t.Context())).To(MatchError(preflight.ErrFailed))
	p := f.cl.Policy("")
	g.Expect(substrateauthz.HasMember(p, "owner", provisionerMember)).To(BeTrue())
	g.Expect(substrateauthz.HasMember(p, "owner", "user:alice")).To(BeTrue())
	g.Expect(p.GetBindings()).To(HaveLen(1))
}
