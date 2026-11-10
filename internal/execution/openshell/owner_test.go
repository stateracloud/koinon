// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
)

// Two installs share one gateway and compute the same object names (a restored database next to
// the live install). Issue 461: the second neither adopts nor changes nor deletes the first's.
var _ = Describe("Backend ownership", func() {
	var (
		live, restore, legacy *openshell.Backend
		gw                    *fakegateway.Gateway
	)
	const name = "ka-0123456789abcdef"

	BeforeEach(func() {
		gw = fakegateway.New()
		conn, err := gw.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(gw.Stop)
		DeferCleanup(conn.Close)
		legacy = openshell.New(conn)
		live = openshell.New(conn).WithOwner(execution.OwnerID("install-1", "koinon"))
		restore = openshell.New(conn).WithOwner(execution.OwnerID("install-1", "koinon-restore"))
		_, err = live.EnsureWorkspace(context.Background(), ws, nil)
		Expect(err).NotTo(HaveOccurred())
	})

	It("derives a different owner per namespace for one install ID", func() {
		Expect(execution.OwnerID("install-1", "koinon")).NotTo(Equal(execution.OwnerID("install-1", "koinon-restore")))
		Expect(execution.OwnerID("install-1", "koinon")).To(Equal(execution.OwnerID("install-1", "koinon")))
	})

	It("stamps what it creates", func(ctx SpecContext) {
		sb, err := live.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.Labels).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("install-1", "koinon")))
		Expect(gw.Calls("CreateWorkspace")).To(Equal(1))
	})

	It("does not adopt, change or delete the other install's sandbox", func(ctx SpecContext) {
		_, err := live.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
		Expect(err).NotTo(HaveOccurred())

		_, err = restore.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
		Expect(err).To(MatchError(execution.ErrForeign), "no adopt on AlreadyExists")
		_, err = restore.StopSandbox(ctx, ws, name)
		Expect(err).To(MatchError(execution.ErrForeign))
		_, err = restore.StartSandbox(ctx, ws, name)
		Expect(err).To(MatchError(execution.ErrForeign))
		_, err = restore.EnsurePolicy(ctx, ws, name, harnessPolicy())
		Expect(err).To(MatchError(execution.ErrForeign))
		Expect(restore.DeleteSandbox(ctx, ws, name)).To(MatchError(execution.ErrForeign))

		Expect(gw.Sandbox(ws, name)).NotTo(BeNil(), "the live sandbox is still there")
		Expect(gw.Calls("DeleteSandbox")).To(BeZero())
		Expect(gw.Calls("StopSandbox")).To(BeZero())
		Expect(live.DeleteSandbox(ctx, ws, name)).To(Succeed(), "its owner can")
	})

	It("deleting a sandbox that is not there is still fine", func(ctx SpecContext) {
		Expect(restore.DeleteSandbox(ctx, ws, "ka-missing")).To(Succeed())
	})

	It("does not adopt the other install's workspace or repoint its provider", func(ctx SpecContext) {
		_, err := live.EnsureWorkspace(ctx, "kw-shared", nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = restore.EnsureWorkspace(ctx, "kw-shared", nil)
		Expect(err).To(MatchError(execution.ErrForeign))

		p := agentgatewayProvider("1")
		p.Name = "kp-0123456789abcdef"
		Expect(live.EnsureProvider(ctx, ws, p)).To(Succeed())
		p.Credentials = map[string]string{"OPENAI_API_KEY": "koinon_test_not-a-real-token"}
		p.CredentialRevision = "2"
		Expect(restore.EnsureProvider(ctx, ws, p)).To(MatchError(execution.ErrForeign))
		Expect(restore.DeleteProvider(ctx, ws, p.Name)).To(MatchError(execution.ErrForeign))
		Expect(restore.DeleteProviderProfile(ctx, ws, p.Profile.ID)).To(MatchError(execution.ErrForeign))
		Expect(gw.Provider(ws, p.Name)).NotTo(BeNil())
		Expect(gw.Calls("UpdateProvider")).To(BeZero())
		Expect(gw.Calls("DeleteProvider")).To(BeZero())
		Expect(live.DeleteProvider(ctx, ws, p.Name)).To(Succeed())
	})

	It("keeps working with objects made before the label existed", func(ctx SpecContext) {
		_, err := legacy.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "ka-old", Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		_, err = live.StopSandbox(ctx, ws, "ka-old")
		Expect(err).NotTo(HaveOccurred())
		Expect(live.DeleteSandbox(ctx, ws, "ka-old")).To(Succeed())
	})
})
