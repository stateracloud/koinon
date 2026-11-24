// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"errors"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// The gateway scrubs an opened credential only from 8 bytes (ADR 0097), so koinon-api refuses a shorter one
// at save, with a reason a client can read (#1221). The fake values are short on purpose: 7 and 8 bytes.
var _ = Describe("a credential too short to scrub, in the UI API (#1221)", func() {
	const short, long = "fake-07", "fake-008"
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})
	tooShort := func(err error) {
		GinkgoHelper()
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "%v", err)
		reason, _ := reasonOf(err)
		Expect(reason).To(Equal(mcpservers.ReasonCredentialTooShort))
		var ce *connect.Error
		Expect(errors.As(err, &ce)).To(BeTrue())
		Expect(ce.Message()).To(ContainSubstring("at least 8 bytes"))
		Expect(ce.Message()).NotTo(ContainSubstring(short))
	}

	It("refuses 7 bytes and takes 8 when a server is created or its credential is replaced", func(ctx SpecContext) {
		_, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
			OrgId: org, Name: "wiki", Url: "https://mcp.example.com/mcp", CredentialPrefix: "Bearer ", CredentialValue: short,
		})
		tooShort(err)
		srv, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
			OrgId: org, Name: "wiki", Url: "https://mcp.example.com/mcp", CredentialPrefix: "Bearer ", CredentialValue: long,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.GetServer().GetHasCredential()).To(BeTrue())

		id := srv.GetServer().GetId()
		_, err = owner.mcps.UpdateServer(ctx, &mcpserverv1alpha1.UpdateServerRequest{OrgId: org, ServerId: id, CredentialValue: new(short)})
		tooShort(err)
		_, err = owner.mcps.UpdateServer(ctx, &mcpserverv1alpha1.UpdateServerRequest{OrgId: org, ServerId: id, CredentialValue: new("fake-x08")})
		Expect(err).NotTo(HaveOccurred())
	})
})
