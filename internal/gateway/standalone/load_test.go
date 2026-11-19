// SPDX-License-Identifier: Apache-2.0

//go:build load

package standalone_test

// The load suite of agentgateway, MCP path, the baseline Koinon's own gateway is compared with
// (issue 280). It runs only with the build tag "load" and the Ginkgo label "load": `make load-gateway`
// (docs/dev/load-tests.md).
//
// The load generator calls the container at its bridge address, and the container calls the fake MCP
// server at the bridge's host address, so no port forward sits in the path (a published port goes
// through docker-proxy for a call from the host, and testcontainers' host-port access, which the
// other tests use, goes through an SSH tunnel; either would be part of every number here). The
// cloud VM's kernel has no IPv6, and agentgateway then exits with "Address family not supported":
// the container gets IPV6_ENABLED=false (the data-plane bind), and the rendered config's [::]
// addresses (stats and readiness listeners) become 0.0.0.0 (v4Only). IPv4 is all this suite uses.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/api/types/container"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/loadtest"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/testing/agentgatewayimage"
	"github.com/stateracloud/koinon/internal/testing/cfgdeliver"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// v4Only moves every listener of a rendered config from the IPv6 wildcard to the IPv4 one.
func v4Only(cfg []byte) []byte { return bytes.ReplaceAll(cfg, []byte("[::]"), []byte("0.0.0.0")) }

// v4Deliverer delivers configs through [v4Only].
type v4Deliverer struct{ cfgdeliver.Deliverer }

func (d v4Deliverer) Deliver(ctx context.Context, config []byte, rev gateway.Revision) error {
	return d.Deliverer.Deliver(ctx, v4Only(config), rev)
}

var _ = Describe("agentgateway under load", Label("load"), Ordered, func() {
	const driver = "agentgateway"
	var (
		fake    *loadtest.FakeMCP
		ctr     testcontainers.Container
		results []loadtest.Result
		ep      loadtest.Endpoint
		// ip is the container's bridge address.
		ip string
	)
	agent := gateway.AgentRef{Org: "acme", Name: "repo-helper"}

	ready := func(ctx context.Context) error {
		for {
			resp, err := http.Get("http://" + ip + ":15021/healthz/ready")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		var err error
		fake, err = loadtest.StartFakeMCP(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(fake.Stop)

		secretsDir, err := os.MkdirTemp("", "koinon-agw-load-secrets-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(secretsDir) })
		Expect(os.Chmod(secretsDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(secretsDir, "mcp_key"), []byte(loadtest.FakeMCPToken), 0o644)).To(Succeed()) //nolint:gosec // a fake token in a test directory

		first, _, err := standalone.Render(gateway.State{}, options)
		Expect(err).NotTo(HaveOccurred())
		ctr, err = testcontainers.Run(ctx, agentgatewayimage.Ref(), append(cfgdeliver.ContainerOptions(v4Only(first)),
			testcontainers.WithEnv(map[string]string{"IPV6_ENABLED": "false"}),
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, secretsDir+":/secrets/upstream:ro")
			}),
		)...)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
		inspect, err := ctr.Inspect(ctx)
		Expect(err).NotTo(HaveOccurred())
		bridge := inspect.NetworkSettings.Networks["bridge"]
		Expect(bridge).NotTo(BeNil())
		hostIP := bridge.Gateway.String()
		ip = bridge.IPAddress.String()
		gwURL, statsURL := "http://"+ip+":4000", "http://"+ip+":15020"
		readyCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		Expect(ready(readyCtx)).To(Succeed())

		key, err := gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		state := gateway.State{MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{{
			Name: "tools", URL: fmt.Sprintf("http://%s:%d/mcp", hostIP, fake.Port),
			CredentialHeader: "X-Api-Key", CredentialFile: "/secrets/upstream/mcp_key", //nolint:gosec // a header name and a test key file
		}}}}
		state.AddKey(key.Key)
		state.GrantTools(agent, gateway.ToolGrant{Target: "tools", All: true})
		d := &standalone.Driver{
			Options:   options,
			Deliverer: v4Deliverer{cfgdeliver.Deliverer{Container: ctr}},
			Waiter:    standalone.ProbeWaiter{GatewayURL: gwURL, StatsURL: statsURL, Interval: 200 * time.Millisecond},
		}
		_, err = d.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
		ep = loadtest.Endpoint{
			Driver: driver, URL: gwURL + "/mcp", Headers: map[string]string{"Authorization": "Bearer " + key.Material},
			Tool: "tools_read_item", StreamTool: "tools_stream_progress",
		}
	}, NodeTimeout(5*time.Minute))

	AfterAll(func() { Expect(loadtest.Save(results)).To(Succeed()) })

	It("serves sustained concurrent tool calls", func(ctx SpecContext) {
		for _, conc := range loadtest.Levels() {
			res, err := ep.ToolCalls(ctx, conc, 2*time.Second, loadtest.Duration())
			Expect(err).NotTo(HaveOccurred())
			results = append(results, res)
		}
	}, SpecTimeout(30*time.Minute))

	It("holds long streams open", func(ctx SpecContext) {
		res, err := ep.Streams(ctx, 64, 20, 500*time.Millisecond, loadtest.Duration()+10*time.Second)
		Expect(err).NotTo(HaveOccurred())
		results = append(results, res)
	}, SpecTimeout(10*time.Minute))

	It("restarts: SIGTERM, then a new process, under load", func(ctx SpecContext) {
		const termAt = 4 * time.Second
		res, err := ep.Custom(ctx, loadtest.Spec{
			Name: "restart under load (stream 4x250ms)", Concurrency: 32, Duration: 25 * time.Second,
			NewWorker: func(ctx context.Context, _ int) (loadtest.Worker, error) {
				return ep.NewStreamWorker(ctx, 4, 250*time.Millisecond)
			},
			DuringAfter: termAt,
			During: func(ctx context.Context) error {
				stop := 30 * time.Second
				if err := ctr.Stop(ctx, &stop); err != nil {
					return err
				}
				if err := ctr.Start(ctx); err != nil {
					return err
				}
				return ready(ctx)
			},
		})
		Expect(err).NotTo(HaveOccurred())
		results = append(results, res)
	}, SpecTimeout(10*time.Minute))
})
