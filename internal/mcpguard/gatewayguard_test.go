// SPDX-License-Identifier: Apache-2.0

package mcpguard_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/mcpguard"
)

// These specs need no database: the messages below never reach it.
var _ = Describe("the gateway guard adapter, without a database", func() {
	var gg *mcpguard.GatewayGuard
	ref := gateway.AgentRef{Org: "acme", Name: "reader"}
	BeforeEach(func() { gg = mcpguard.NewGatewayGuard(mcpguard.New(nil, mcpguard.Options{}), nil) })

	msg := func(dir mcpgateway.Direction, kind mcpgateway.Kind, method string) mcpgateway.Message {
		return mcpgateway.Message{Target: "acme-wiki", Agent: ref, Dir: dir, Kind: kind, Method: method, Raw: []byte(`{"jsonrpc":"2.0"}`)}
	}

	It("keeps the route closed until Load, and reports why", func() {
		Expect(gg.Loaded()).To(MatchError(mcpguard.ErrNotLoaded))
	})

	It("passes what the gateway sends and the answers that set no leg", func() {
		ctx := context.Background()
		for _, m := range []mcpgateway.Message{
			msg(mcpgateway.ToUpstream, mcpgateway.KindRequest, "tools/call"),
			msg(mcpgateway.ToUpstream, mcpgateway.KindRequest, "initialize"),
			msg(mcpgateway.FromUpstream, mcpgateway.KindResult, "initialize"),
			// A readable list sets no leg here: CheckToolList decides on what it carries (#1132).
			func() mcpgateway.Message {
				m := msg(mcpgateway.FromUpstream, mcpgateway.KindResult, "tools/list")
				m.Raw = []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
				return m
			}(),
			msg(mcpgateway.FromUpstream, mcpgateway.KindResult, "ping"),
			msg(mcpgateway.FromUpstream, mcpgateway.KindNotification, "notifications/progress"),
			msg(mcpgateway.FromUpstream, mcpgateway.KindRequest, "ping"),
			msg(mcpgateway.FromUpstream, mcpgateway.KindRequest, "roots/list"),
		} {
			Expect(gg.Inspect(ctx, m).Refuse).To(BeFalse(), "%v %s", m.Kind, m.Method)
		}
	})
})
