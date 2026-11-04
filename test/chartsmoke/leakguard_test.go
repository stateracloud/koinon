// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
	corev1 "k8s.io/api/core/v1"

	"github.com/stateracloud/koinon/internal/gen/agentgateway/extmcp"
)

// guardURLPattern finds the reconciler's mcpGuard.url in the rendered config.yaml.
var guardURLPattern = regexp.MustCompile(`(?m)^\s*mcpGuard:\s*\n\s*url:\s*(\S+)\s*$`)

// guardURL is the leak guard address the chart renders into the reconciler's config, which the
// reconciler renders into every org MCP route's mcpGuardrails (ADR 0091).
func guardURL(r *rendered) string {
	GinkgoHelper()
	for _, o := range r.Objects {
		cm, ok := o.(*corev1.ConfigMap)
		if !ok {
			continue
		}
		for _, v := range cm.Data {
			if m := guardURLPattern.FindStringSubmatch(v); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// checkLeakGuard proves the chart's koinon-api serves the MCP leak guard where the reconciler's
// config points: the --guard-addr port is the container port named guard and the port of the
// URL, and an ExtMcp call on it gets the guard's own refusal (a tool call to a target no org owns),
// not a transport error. A refusal there is what the gateway shows the agent; a transport error
// would make the gateway fail every org MCP call closed.
func checkLeakGuard(ctx context.Context, r *rendered, api *workload) {
	GinkgoHelper()
	raw := guardURL(r)
	Expect(raw).NotTo(BeEmpty(), "the chart renders mcpGuard.url into the reconciler's config; without it the gateway refuses every org MCP call")
	u, err := url.Parse(raw)
	Expect(err).NotTo(HaveOccurred())
	Expect(u.Scheme).To(Equal("http"))
	Expect(u.Hostname()).To(HaveSuffix(".svc"), "the guard is reached through its Service")
	urlPort, err := strconv.Atoi(u.Port())
	Expect(err).NotTo(HaveOccurred(), "mcpGuard.url %q names a port", raw)

	c := api.Spec
	i := slices.Index(c.Args, "--guard-addr")
	Expect(i).To(BeNumerically(">=", 0), "koinon-api gets --guard-addr")
	Expect(i + 1).To(BeNumerically("<", len(c.Args)))
	addrPort, err := strconv.Atoi(strings.TrimPrefix(c.Args[i+1], ":"))
	Expect(err).NotTo(HaveOccurred(), "--guard-addr %q is :port", c.Args[i+1])
	pi := slices.IndexFunc(c.Ports, func(p corev1.ContainerPort) bool { return p.Name == "guard" })
	Expect(pi).To(BeNumerically(">=", 0), "koinon-api names its guard port (the NetworkPolicy and the Service refer to it)")
	Expect(int(c.Ports[pi].ContainerPort)).To(Equal(addrPort), "the guard listens on the port the container declares")
	Expect(urlPort).To(Equal(addrPort), "the Service port in mcpGuard.url is the guard's port (the Service maps it by name)")

	mapped, err := api.Ctr.MappedPort(ctx, fmt.Sprintf("%d/tcp", addrPort))
	Expect(err).NotTo(HaveOccurred())
	conn, err := grpc.NewClient("127.0.0.1:"+mapped.Port(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close() }()
	meta, err := structpb.NewStruct(map[string]any{"agent": "chartsmoke/nobody", "tool": "write_item"})
	Expect(err).NotTo(HaveOccurred())
	params := []byte(`{"name":"write_item","arguments":{"text":"koinon_test_not-a-real-token"}}`)
	Eventually(func(g Gomega) {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		res, err := extmcp.NewExtMcpClient(conn).CheckRequest(cctx, &extmcp.McpRequest{
			ServiceNames: []string{"org-chartsmoke-unknown"}, Method: "tools/call",
			MetadataContext: meta, McpRequest: params,
		})
		g.Expect(err).NotTo(HaveOccurred(), "the guard answers on its port; logs:\n%s", api.Logs(ctx))
		e := res.GetError()
		g.Expect(e).NotTo(BeNil(), "a call to a target no org owns is refused, got %v", res)
		g.Expect(e.GetCode()).To(Equal(extmcp.AuthorizationError_PERMISSION_DENIED))
		g.Expect(e.GetReason()).NotTo(BeEmpty(), "the refusal carries text the agent reads")
	}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}
