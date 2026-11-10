// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	tracev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/tracedest"
)

// A held install (restore-hold mode, issue #462, ADR 0107) refuses every call an admin can start that
// reaches outward. The specs need no database: the refusal comes before any read or write.
var _ = Describe("restore hold", func() {
	var (
		held, free *Server
		ctx        context.Context
		org        string
	)

	BeforeEach(func() {
		id := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
		org = "01010101-0101-0101-0101-010101010101"
		ctx = auth.WithPrincipal(context.Background(), &auth.Principal{
			PlatformAdmin: true,
			Memberships:   []auth.Membership{{OrgID: id, Role: store.OrgRoleOwner}},
		})
		wire := func(restoreHold bool) *Server {
			return &Server{
				restoreHold: restoreHold,
				catalog:     &catalog.Service{}, mcp: &mcpservers.Service{}, modelCheck: &modelcheck.Service{}, traces: &tracedest.Service{},
			}
		}
		held, free = wire(true), wire(false)
	})

	refused := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("install is held for restore; release the hold first"))
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("back up and restore"))
	}

	It("is nil when the install is not held", func() {
		Expect(free.refuseWhileHeld()).To(Succeed())
	})

	It("refuses the provider echo check, which calls a model", func() {
		_, err := held.CheckProviderEcho(ctx, &providerv1alpha1.CheckProviderEchoRequest{OrgId: org})
		refused(err)
	})

	It("refuses creating, re-keying and testing a code-host connection, and listing an installation", func() {
		_, err := held.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{OrgId: org})
		refused(err)
		_, err = held.CreateGiteaConnection(ctx, &connectionv1alpha1.CreateGiteaConnectionRequest{OrgId: org})
		refused(err)
		_, err = held.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{OrgId: org})
		refused(err)
		_, err = held.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org})
		refused(err)
		_, err = held.ListInstallationRepos(ctx, &connectionv1alpha1.ListInstallationReposRequest{OrgId: org})
		refused(err)
	})

	It("refuses the code-host check of an agent's repository bindings", func() {
		_, err := held.checkInstallation(ctx, nil, store.Connection{}, "bindings[0]", []string{"acme/app"})
		refused(err)
	})

	It("refuses reading an approval's diff from the code host", func() {
		_, err := held.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: org})
		refused(err)
	})

	It("refuses adding an MCP server and changing its address, whose preflight dials it", func() {
		_, err := held.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{OrgId: org})
		refused(err)
		url := "https://mcp.example.invalid/"
		_, err = held.UpdateServer(ctx, &mcpserverv1alpha1.UpdateServerRequest{OrgId: org, Url: &url})
		refused(err)
	})

	It("refuses saving and testing a trace destination, which send a synthetic trace", func() {
		api := traceAPI{held}
		_, err := api.CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org})
		refused(err)
		_, err = api.UpdateDestination(ctx, &tracev1alpha1.UpdateDestinationRequest{OrgId: org})
		refused(err)
		_, err = api.TestDestination(ctx, &tracev1alpha1.TestDestinationRequest{OrgId: org})
		refused(err)
		_, err = api.SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{})
		refused(err)
		_, err = api.TestPlatformDestination(ctx, &tracev1alpha1.TestPlatformDestinationRequest{})
		refused(err)
	})

	It("tells a caller who may not use the call nothing about the hold", func() {
		_, err := held.CheckProviderEcho(auth.WithPrincipal(context.Background(), &auth.Principal{}), &providerv1alpha1.CheckProviderEchoRequest{OrgId: org})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
	})
})
