// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
)

// CountUnmarked reads the gateway, not the reconcile flow (issue 461): every sandbox, provider and
// profile in this install's workspaces counts when it has no install mark, whatever a pass did.
var _ = Describe("Backend.CountUnmarked", func() {
	var (
		gw                    *fakegateway.Gateway
		live, restore, legacy *openshell.Backend
	)
	mine := execution.WorkspaceName([16]byte{1})
	theirs := execution.WorkspaceName([16]byte{2})

	BeforeEach(func() {
		gw = fakegateway.New()
		conn, err := gw.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(gw.Stop)
		DeferCleanup(conn.Close)
		legacy = openshell.New(conn)
		live = openshell.New(conn).WithOwner(execution.OwnerID("mark-live"))
		restore = openshell.New(conn).WithOwner(execution.OwnerID("mark-restore"))
	})

	It("counts the objects without the mark and leaves out marked and foreign ones", func(ctx SpecContext) {
		_, err := legacy.EnsureWorkspace(ctx, mine, nil) // an old, unmarked workspace
		Expect(err).NotTo(HaveOccurred())
		for _, name := range []string{"ka-old1", "ka-old2"} {
			_, err = legacy.CreateSandbox(ctx, mine, execution.SandboxSpec{Name: name, Image: "img"})
			Expect(err).NotTo(HaveOccurred())
		}
		_, err = live.CreateSandbox(ctx, mine, execution.SandboxSpec{Name: "ka-new", Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		_, err = restore.CreateSandbox(ctx, mine, execution.SandboxSpec{Name: "ka-other", Image: "img"})
		Expect(err).NotTo(HaveOccurred())

		old := agentgatewayProvider("1")
		old.Name = "kp-old"
		old.Profile.ID = "koinon-old"
		Expect(legacy.EnsureProvider(ctx, mine, old)).To(Succeed())
		fresh := agentgatewayProvider("1")
		fresh.Name = "kp-new"
		Expect(live.EnsureProvider(ctx, mine, fresh)).To(Succeed())

		// Another install's workspace, and a workspace that is not Koinon's, are not this install's.
		_, err = restore.EnsureWorkspace(ctx, theirs, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = legacy.CreateSandbox(ctx, theirs, execution.SandboxSpec{Name: "ka-theirs", Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		_, err = legacy.EnsureWorkspace(ctx, "someone-else", nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = legacy.CreateSandbox(ctx, "someone-else", execution.SandboxSpec{Name: "x", Image: "img"})
		Expect(err).NotTo(HaveOccurred())

		counts, err := live.CountUnmarked(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(map[string]int{"sandbox": 2, "provider": 1, "profile": 1}))

		// Claiming the old provider and profile (recreated with the mark) and removing the old sandboxes empties it.
		old.ClaimLegacy = true
		Expect(live.EnsureProvider(ctx, mine, old)).To(Succeed())
		for _, name := range []string{"ka-old1", "ka-old2"} {
			Expect(live.DeleteSandbox(ctx, mine, name)).To(Succeed())
		}
		counts, err = live.CountUnmarked(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(map[string]int{"sandbox": 0, "provider": 0, "profile": 0}), "the profile is re-imported with the mark")
	})

	It("returns an error and no counts when any list fails", func(ctx SpecContext) {
		_, err := legacy.EnsureWorkspace(ctx, mine, nil)
		Expect(err).NotTo(HaveOccurred())
		for _, method := range []string{"ListWorkspaces", "ListSandboxes", "ListProviders", "ListProviderProfiles"} {
			gw.FailNext(method, errors.New("gateway unavailable"))
			counts, err := live.CountUnmarked(ctx)
			Expect(err).To(HaveOccurred(), method)
			Expect(counts).To(BeNil(), method)
		}
		counts, err := live.CountUnmarked(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(map[string]int{"sandbox": 0, "provider": 0, "profile": 0}))
	})

	It("does not count the platform and built-in profiles a workspace listing returns", func(ctx SpecContext) {
		_, err := legacy.EnsureWorkspace(ctx, mine, nil)
		Expect(err).NotTo(HaveOccurred())
		gw.AddPlatformProfile("claude-code")
		counts, err := live.CountUnmarked(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts["profile"]).To(Equal(0))

		old := agentgatewayProvider("1")
		old.Name = "kp-old"
		old.Profile.ID = "koinon-old"
		Expect(legacy.EnsureProvider(ctx, mine, old)).To(Succeed())
		counts, err = live.CountUnmarked(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts["profile"]).To(Equal(1), "only the workspace's own unmarked profile")
	})

	It("follows every page of every list", func(ctx SpecContext) {
		gw.ListPageSize = 2
		for _, ws := range []string{mine, execution.WorkspaceName([16]byte{3}), execution.WorkspaceName([16]byte{4})} {
			_, err := legacy.EnsureWorkspace(ctx, ws, nil)
			Expect(err).NotTo(HaveOccurred())
			for _, name := range []string{"ka-a", "ka-b", "ka-c"} {
				_, err = legacy.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: name, Image: "img"})
				Expect(err).NotTo(HaveOccurred())
			}
			for _, name := range []string{"kp-a", "kp-b", "kp-c"} {
				p := agentgatewayProvider("1")
				p.Name = name
				p.Profile.ID = "koinon-" + name
				Expect(legacy.EnsureProvider(ctx, ws, p)).To(Succeed())
			}
		}
		counts, err := live.CountUnmarked(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(map[string]int{"sandbox": 9, "provider": 9, "profile": 9}))
	})

	It("needs an install mark", func(ctx SpecContext) {
		_, err := legacy.CountUnmarked(ctx)
		Expect(err).To(HaveOccurred())
	})
})
