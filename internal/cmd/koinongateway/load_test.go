// SPDX-License-Identifier: Apache-2.0

//go:build load

package koinongateway

// The load suite of koinon-gateway, MCP path (issue 280). It runs only with the build tag "load" and
// the Ginkgo label "load": `make load-gateway` (docs/dev/load-tests.md). The gateway runs in this
// process (run, as main calls it), follows a State stream of this process and calls the fake MCP
// server cmd/koinon-fakemcp, which runs as a process of its own.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/loadtest"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/gateway/stateserver"
)

var _ = Describe("koinon-gateway under load", Label("load"), Ordered, func() {
	const driver = "koinon-gateway"
	var (
		fake    *loadtest.FakeMCP
		store   *memStore
		api     *httptest.Server
		tokenFn string
		results []loadtest.Result
	)
	agent := gateway.AgentRef{Org: "acme", Name: "repo-helper"}

	endpoint := func(addr string) loadtest.Endpoint {
		return loadtest.Endpoint{
			Driver: driver, URL: "http://" + addr + mcpPath, Headers: map[string]string{"Authorization": "Bearer " + fakeKey},
			Tool: "tools_read_item", StreamTool: "tools_stream_progress",
		}
	}
	// replica starts one koinon-gateway process of this run. Cancelling its context is SIGTERM.
	replica := func(shutdownDelay time.Duration) (addr string, term func(), done <-chan error) {
		cfg := config{
			listenAddr: freeAddr(), healthAddr: freeAddr(), stateURL: api.URL, stateTokenFile: tokenFn,
			podName: "koinon-gateway-load", drain: 20 * time.Second, shutdownDelay: shutdownDelay,
		}
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan error, 1)
		fin := make(chan struct{})
		go func() { ch <- run(ctx, cfg); close(fin) }()
		DeferCleanup(func() {
			cancel()
			Eventually(fin, 30*time.Second).Should(BeClosed(), "the replica stops before the next spec")
		})
		Eventually(func() bool {
			resp, err := http.Get("http://" + cfg.healthAddr + "/readyz")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 15*time.Second, 50*time.Millisecond).Should(BeTrue())
		return cfg.listenAddr, cancel, ch
	}

	BeforeAll(func(ctx SpecContext) {
		var err error
		fake, err = loadtest.StartFakeMCP(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(fake.Stop)

		keyFile := filepath.Join(GinkgoT().TempDir(), "mcp_key")
		Expect(os.WriteFile(keyFile, []byte(loadtest.FakeMCPToken), 0o600)).To(Succeed())
		u := staterev.Snapshot(gateway.State{
			Keys: []gateway.Key{{ID: "k1", Agent: agent, Hash: gateway.HashKey(fakeKey)}},
			MCPState: gateway.MCPState{
				MCP: []gateway.MCPTarget{{
					Name: "tools", URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", fake.Port),
					CredentialHeader: "X-Api-Key", CredentialFile: keyFile, //nolint:gosec // a header name and a test key file
				}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "tools", All: true}}},
			},
		})
		u.Revision = 1
		store = &memStore{cur: u}
		auth := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+fakeStreamToken {
					http.Error(w, "no", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
			})
		}
		ss, err := stateserver.New(stateserver.Config{Store: store, Auth: auth, PollInterval: 50 * time.Millisecond})
		Expect(err).NotTo(HaveOccurred())
		srvCtx, stopSrv := context.WithCancel(context.Background())
		DeferCleanup(stopSrv)
		go ss.Run(srvCtx)
		api = httptest.NewServer(ss.Mount()(http.NotFoundHandler()))
		DeferCleanup(api.Close)
		tokenFn = filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(tokenFn, []byte(fakeStreamToken+"\n"), 0o600)).To(Succeed())
	}, NodeTimeout(3*time.Minute))

	AfterAll(func() { Expect(loadtest.Save(results)).To(Succeed()) })

	It("serves sustained concurrent tool calls", func(ctx SpecContext) {
		addr, _, _ := replica(time.Second)
		ep := endpoint(addr)
		for _, conc := range loadtest.Levels() {
			res, err := ep.ToolCalls(ctx, conc, 2*time.Second, loadtest.Duration())
			Expect(err).NotTo(HaveOccurred())
			results = append(results, res)
		}
	}, SpecTimeout(30*time.Minute))

	It("holds long streams open", func(ctx SpecContext) {
		addr, _, _ := replica(time.Second)
		res, err := endpoint(addr).Streams(ctx, 64, 20, 500*time.Millisecond, loadtest.Duration()+10*time.Second)
		Expect(err).NotTo(HaveOccurred())
		results = append(results, res)
	}, SpecTimeout(10*time.Minute))

	It("drains: serves for the shutdown delay after SIGTERM and lets calls in flight finish", func(ctx SpecContext) {
		const shutdownDelay = 2 * time.Second
		const termAt = 4 * time.Second
		addr, term, done := replica(shutdownDelay)
		ep := endpoint(addr)
		// Calls of one second each (stream tool), so some are in flight at the signal.
		res, err := ep.Custom(ctx, loadtest.Spec{
			Name: "drain after SIGTERM (stream 4x250ms)", Concurrency: 32, Duration: 9 * time.Second,
			NewWorker: func(ctx context.Context, _ int) (loadtest.Worker, error) {
				return ep.NewStreamWorker(ctx, 4, 250*time.Millisecond)
			},
			DuringAfter: termAt,
			During:      func(context.Context) error { term(); return nil },
		})
		Expect(err).NotTo(HaveOccurred())
		results = append(results, res)
		Eventually(done, 30*time.Second).Should(Receive(BeNil()), "run returns without an error")
		// The first failed call starts after the shutdown delay, not at the signal.
		Expect(res.Errors).To(BeNumerically(">", 0), "the replica is gone at the end of the run")
		Expect(res.FirstError).To(BeNumerically(">=", termAt+shutdownDelay-500*time.Millisecond))
	}, SpecTimeout(10*time.Minute))
})
