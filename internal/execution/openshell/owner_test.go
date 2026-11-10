// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"

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
		liveConn              grpc.ClientConnInterface
	)
	const name = "ka-0123456789abcdef"

	BeforeEach(func() {
		gw = fakegateway.New()
		conn, err := gw.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(gw.Stop)
		DeferCleanup(conn.Close)
		liveConn = conn
		legacy = openshell.New(conn)
		live = openshell.New(conn).WithOwner(execution.OwnerID("mark-live"))
		restore = openshell.New(conn).WithOwner(execution.OwnerID("mark-restore"))
		_, err = live.EnsureWorkspace(context.Background(), ws, nil)
		Expect(err).NotTo(HaveOccurred())
	})

	It("derives a different owner per install mark", func() {
		Expect(execution.OwnerID("mark-live")).NotTo(Equal(execution.OwnerID("mark-restore")))
		Expect(execution.OwnerID("mark-live")).To(Equal(execution.OwnerID("mark-live")))
	})

	It("stamps what it creates", func(ctx SpecContext) {
		sb, err := live.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.Labels).To(HaveKeyWithValue(execution.LabelInstall, execution.OwnerID("mark-live")))
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

	It("does not let the other install exec in or upload into the sandbox", func(ctx SpecContext) {
		_, err := live.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
		Expect(err).NotTo(HaveOccurred())

		_, err = restore.GetSandbox(ctx, ws, name)
		Expect(err).To(MatchError(execution.ErrForeign), "a foreign sandbox is not read as its own")
		_, err = restore.Exec(ctx, ws, name, execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(MatchError(execution.ErrForeign))
		Expect(restore.Upload(ctx, ws, name, "/sandbox", strings.NewReader(""))).To(MatchError(execution.ErrForeign))
		Expect(gw.Calls("ExecSandboxInteractive")).To(BeZero())
		Expect(gw.Calls("CreateSshSession")).To(BeZero())
	})

	It("tells two installs apart that share the same database and namespace by their marks", func(ctx SpecContext) {
		// Same install ID, same namespace name: only the per-deployment mark differs.
		a := openshell.New(liveConn).WithOwner(execution.OwnerID("mark-a"))
		b := openshell.New(liveConn).WithOwner(execution.OwnerID("mark-b"))
		_, err := a.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		_, err = b.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
		Expect(err).To(MatchError(execution.ErrForeign))
		Expect(b.DeleteSandbox(ctx, ws, name)).To(MatchError(execution.ErrForeign))
		Expect(gw.Sandbox(ws, name)).NotTo(BeNil())
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
