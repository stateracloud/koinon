// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	g "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

var _ = g.Describe("resultLegs (ADR 0091 decision 4)", func() {
	t, f := true, false

	// The worked table: a tool that declares a result leg replaces the server's leg; a tool that
	// leaves it unset keeps the server's. Each leg is decided alone.
	g.DescribeTable("untrusted-input leg",
		func(server bool, tool *bool, want bool) {
			d := sinkclass.Default()
			d.ResultUntrustedInput = tool
			Expect(resultLegs(store.McpServer{ExposesUntrustedInput: server}, d).UntrustedInput).To(Equal(want))
		},
		g.Entry("unset on a server without the leg", false, nil, false),
		g.Entry("unset on a server with the leg", true, nil, true),
		g.Entry("true on a server without the leg", false, &t, true),
		g.Entry("true on a server with the leg", true, &t, true),
		g.Entry("false on a server without the leg", false, &f, false),
		g.Entry("false on a server with the leg", true, &f, false),
	)

	g.DescribeTable("private-data leg",
		func(server bool, tool *bool, want bool) {
			d := sinkclass.Default()
			d.ResultPrivateData = tool
			Expect(resultLegs(store.McpServer{ExposesPrivateData: server}, d).PrivateData).To(Equal(want))
		},
		g.Entry("unset on a server without the leg", false, nil, false),
		g.Entry("unset on a server with the leg", true, nil, true),
		g.Entry("true on a server without the leg", false, &t, true),
		g.Entry("true on a server with the leg", true, &t, true),
		g.Entry("false on a server without the leg", false, &f, false),
		g.Entry("false on a server with the leg", true, &f, false),
	)

	g.It("keeps the legs apart: one leg declared does not change the other", func() {
		d := sinkclass.Default()
		d.ResultUntrustedInput = &f
		srv := store.McpServer{ExposesUntrustedInput: true, ExposesPrivateData: true}
		Expect(resultLegs(srv, d)).To(Equal(Legs{UntrustedInput: false, PrivateData: true}))
	})

	g.It("gives an unknown tool on a server nobody classified both legs (fail closed)", func() {
		// A server nobody classified carries both legs (mcpservers.DefaultDeclaration).
		srv := store.McpServer{ExposesUntrustedInput: true, ExposesPrivateData: true}
		Expect(sinkclass.Default().Class).To(Equal(sinkclass.ExternalHigh))
		Expect(resultLegs(srv, sinkclass.Default())).To(Equal(Legs{UntrustedInput: true, PrivateData: true}))
	})
})
