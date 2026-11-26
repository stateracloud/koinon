// SPDX-License-Identifier: Apache-2.0

package koinongateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/gateway/stateserver"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/modelformat"
)

func TestKoinonGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-gateway")
}

const (
	stateURL = "http://koinon-api.koinon.svc:4318"
	// Obviously fake: the gitleaks rules never match it.
	fakeStreamToken = "koinon_test_not-a-real-token"
	fakeKey         = gateway.KeyPrefix + "koinon_test_not-a-real-key"
)

var _ = Describe("koinon-gateway's startup", func() {
	var tokenFile string
	BeforeEach(func() {
		tokenFile = filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(tokenFile, []byte(fakeStreamToken+"\n"), 0o600)).To(Succeed())
	})

	It("starts with the State stream URL and token file, from flags or the environment", func() {
		Expect(cli.Check(NewRootCommand(), []string{"--state-url", stateURL, "--state-token-file", tokenFile}, nil)).To(Succeed())
		Expect(cli.Check(NewRootCommand(), []string{}, map[string]string{
			"KOINON_STATE_URL": stateURL, "KOINON_STATE_TOKEN_FILE": tokenFile, "POD_NAME": "koinon-gateway-0",
		})).To(Succeed())
	})

	It("names each missing required setting", func() {
		for _, missing := range []string{"state-url", "state-token-file"} {
			args := []string{}
			if missing != "state-url" {
				args = append(args, "--state-url", stateURL)
			}
			if missing != "state-token-file" {
				args = append(args, "--state-token-file", tokenFile)
			}
			err := cli.Check(NewRootCommand(), args, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(missing))
		}
	})

	It("refuses a State URL that is not http(s) with a host, and a drain of no time", func() {
		for _, bad := range []string{"koinon-api:4318", "ftp://koinon-api", "http://", "http://u:p@koinon-api"} {
			Expect(cli.Check(NewRootCommand(), []string{"--state-url", bad, "--state-token-file", tokenFile}, nil)).NotTo(Succeed(), bad)
		}
		Expect(cli.Check(NewRootCommand(), []string{"--state-url", stateURL, "--state-token-file", tokenFile, "--drain-timeout", "0s"}, nil)).NotTo(Succeed())
		Expect(cli.Check(NewRootCommand(), []string{"--state-url", stateURL, "--state-token-file", tokenFile, "--shutdown-delay", "-1s"}, nil)).NotTo(Succeed())
	})

	It("refuses a drain shorter than twice the MCP call timeout, and accepts it on a model-only pod", func() {
		base := []string{"--state-url", stateURL, "--state-token-file", tokenFile}
		err := cli.Check(NewRootCommand(), append(base, "--drain-timeout", "3m"), nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("call timeout twice"))
		Expect(cli.Check(NewRootCommand(), append(base, "--drain-timeout", "4m"), nil)).To(Succeed())
		Expect(cli.Check(NewRootCommand(), base, nil)).To(Succeed(), "the default drain covers a call")
		model := append(base, "--mcp-path=false", "--model-path", "--drain-timeout", "30s")
		Expect(cli.Check(NewRootCommand(), model, nil)).To(Succeed())
	})

	It("takes the guard's database login from --database-url-file, and refuses an empty or missing file before it serves anything", func() {
		dsn := filepath.Join(GinkgoT().TempDir(), "dsn")
		Expect(os.WriteFile(dsn, []byte("postgres://koinon_gateway@db.example/koinon\n"), 0o600)).To(Succeed())
		Expect(cli.Check(NewRootCommand(), []string{"--state-url", stateURL, "--state-token-file", tokenFile, "--database-url-file", dsn}, nil)).To(Succeed())
		Expect(cli.Check(NewRootCommand(), []string{}, map[string]string{
			"KOINON_STATE_URL": stateURL, "KOINON_STATE_TOKEN_FILE": tokenFile, "KOINON_DATABASE_URL_FILE": dsn,
		})).To(Succeed())
		empty := filepath.Join(GinkgoT().TempDir(), "empty")
		Expect(os.WriteFile(empty, []byte("\n"), 0o600)).To(Succeed())
		for _, f := range []string{empty, filepath.Join(GinkgoT().TempDir(), "absent")} {
			err := run(context.Background(), config{stateURL: stateURL, stateTokenFile: tokenFile, databaseURLFile: f, listenAddr: "127.0.0.1:0", drain: time.Second})
			Expect(err).To(HaveOccurred())
		}
	})

	It("refuses an empty or missing token file before it serves anything", func() {
		empty := filepath.Join(GinkgoT().TempDir(), "empty")
		Expect(os.WriteFile(empty, []byte("\n"), 0o600)).To(Succeed())
		for _, f := range []string{empty, filepath.Join(GinkgoT().TempDir(), "absent")} {
			err := run(context.Background(), config{mcpPath: true, stateURL: stateURL, stateTokenFile: f, listenAddr: "127.0.0.1:0", drain: time.Second})
			Expect(err).To(HaveOccurred())
		}
	})
})

// memStore is the State stream's store: one revision, and the acks it received.
type memStore struct {
	mu   sync.Mutex
	cur  *gwv1.StateUpdate
	acks []int64
}

func (s *memStore) Latest(context.Context) (*gwv1.StateUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, nil
}

func (s *memStore) Ack(_ context.Context, _ string, rev int64, _ gwv1.Half) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = append(s.acks, rev)
	return nil
}

func (s *memStore) acked() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.acks...)
}

func freeAddr() string {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

var _ = Describe("koinon-gateway, served", func() {
	It("follows the State stream, serves the agent's tools, acks the revision and drains on the signal", func(specCtx SpecContext) {
		// An MCP server with one tool, the target of the config.
		up := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
		type none struct{}
		mcp.AddTool(up, &mcp.Tool{Name: "ping", Description: "ping"}, func(context.Context, *mcp.CallToolRequest, none) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})
		target := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
		DeferCleanup(target.Close)

		agent := gateway.AgentRef{Org: "acme", Name: "repo-helper"}
		u := staterev.Snapshot(gateway.State{
			Keys: []gateway.Key{{ID: "k1", Agent: agent, Hash: gateway.HashKey(fakeKey)}},
			MCPState: gateway.MCPState{
				MCP:   []gateway.MCPTarget{{Name: "tools", URL: target.URL}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "tools", Tools: []string{"ping"}}}},
			},
		})
		u.Revision = 7
		store := &memStore{cur: u}

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
		api := httptest.NewServer(ss.Mount()(http.NotFoundHandler()))
		DeferCleanup(api.Close)

		tokenFile := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(tokenFile, []byte(fakeStreamToken+"\n"), 0o600)).To(Succeed())
		cfg := config{
			listenAddr: freeAddr(), healthAddr: freeAddr(), stateURL: api.URL, stateTokenFile: tokenFile, mcpPath: true,
			podName: "koinon-gateway-0", drain: 5 * time.Second, shutdownDelay: 600 * time.Millisecond,
		}
		ctx, term := context.WithCancel(context.Background())
		DeferCleanup(term)
		done := make(chan error, 1)
		go func() { done <- run(ctx, cfg) }()

		status := func(path string) int {
			resp, err := http.Get("http://" + cfg.healthAddr + path)
			if err != nil {
				return 0
			}
			_ = resp.Body.Close()
			return resp.StatusCode
		}
		Expect(status("/healthz")).To(Or(Equal(http.StatusOK), Equal(0)))
		Eventually(func() int { return status("/readyz") }, 10*time.Second, 50*time.Millisecond).Should(Equal(http.StatusOK))
		Eventually(store.acked, 5*time.Second, 50*time.Millisecond).Should(ContainElement(int64(7)))

		client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0"}, nil)
		sess, err := client.Connect(specCtx, &mcp.StreamableClientTransport{
			Endpoint:             fmt.Sprintf("http://%s%s", cfg.listenAddr, mcpPath),
			HTTPClient:           &http.Client{Transport: bearerRT{key: fakeKey}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = sess.Close() })
		tools, err := sess.ListTools(specCtx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(tools.Tools).To(HaveLen(1))
		Expect(tools.Tools[0].Name).To(Equal("tools_ping"))

		// SIGTERM: not ready at once, but the gateway keeps serving for the shutdown delay, so a request
		// that is still routed here does not fail. Liveness stays 200. After the drain, new requests are
		// refused and run returns without an error.
		term()
		Eventually(func() int { return status("/readyz") }, 5*time.Second, 20*time.Millisecond).Should(Equal(http.StatusServiceUnavailable))
		Expect(status("/healthz")).To(Equal(http.StatusOK))
		_, err = sess.ListTools(specCtx, nil)
		Expect(err).NotTo(HaveOccurred(), "a request during the shutdown delay is served")
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
		_, err = sess.ListTools(specCtx, nil)
		Expect(err).To(HaveOccurred(), "a request after the drain is refused")
	}, SpecTimeout(40*time.Second))
})

var _ = Describe("koinon-gateway, restore hold", func() {
	It("is ready with no State revision and refuses every call (ADR 0107, issue #1358)", func(specCtx SpecContext) {
		// The State stream never answers with a revision: a held reconciler publishes none.
		api := httptest.NewServer(http.NotFoundHandler())
		DeferCleanup(api.Close)
		tokenFile := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(tokenFile, []byte(fakeStreamToken+"\n"), 0o600)).To(Succeed())
		cfg := config{
			listenAddr: freeAddr(), healthAddr: freeAddr(), stateURL: api.URL, stateTokenFile: tokenFile, mcpPath: true,
			restoreHold: true, podName: "koinon-gateway-2", drain: 5 * time.Second, shutdownDelay: 50 * time.Millisecond,
		}
		ctx, term := context.WithCancel(context.Background())
		DeferCleanup(term)
		go func() { _ = run(ctx, cfg) }()
		Eventually(func() int {
			resp, err := http.Get("http://" + cfg.healthAddr + "/readyz")
			if err != nil {
				return 0
			}
			_ = resp.Body.Close()
			return resp.StatusCode
		}, 10*time.Second, 50*time.Millisecond).Should(Equal(http.StatusOK))

		req, err := http.NewRequestWithContext(specCtx, http.MethodPost, fmt.Sprintf("http://%s%s", cfg.listenAddr, mcpPath), strings.NewReader("{}"))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+fakeKey)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(string(body)).To(ContainSubstring("held"))
	}, SpecTimeout(30*time.Second))

	It("is not ready without a revision when not held", func() {
		Expect(config{}.restoreHold).To(BeFalse())
	})
})

var _ = Describe("koinon-gateway, model path", func() {
	It("serves a model call, taps its usage and exports the record to koinon-api", func(specCtx SpecContext) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5}}`))
		}))
		DeferCleanup(up.Close)
		agent := gateway.AgentRef{Org: "acme", Name: "repo-helper"}
		u := staterev.Snapshot(gateway.State{
			Keys: []gateway.Key{{ID: "k1", Agent: agent, Hash: gateway.HashKey(fakeKey)}},
			ModelState: gateway.ModelState{Providers: []gateway.Provider{{
				Org: "acme", Name: "p", Type: "custom", BaseURL: up.URL + "/v1",
				Models: []gateway.Model{{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
			}}},
		})
		u.Revision = 3
		store := &memStore{cur: u}

		var got atomic.Int64
		logs := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != agentgateway.LogsPath || r.Header.Get("Authorization") != "Bearer "+fakeStreamToken {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			b, _ := io.ReadAll(r.Body)
			var req collogspb.ExportLogsServiceRequest
			Expect(proto.Unmarshal(b, &req)).To(Succeed())
			recs, _, _ := agentgateway.Decode(&req)
			for _, rec := range recs {
				if rec.KeyID == "k1" && rec.InputTokens == 11 && rec.OutputTokens == 5 {
					got.Add(1)
				}
			}
		})
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
		api := httptest.NewServer(ss.Mount()(logs))
		DeferCleanup(api.Close)

		tokenFile := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(tokenFile, []byte(fakeStreamToken+"\n"), 0o600)).To(Succeed())
		cfg := config{
			listenAddr: freeAddr(), healthAddr: freeAddr(), stateURL: api.URL, stateTokenFile: tokenFile, modelPath: true, mcpPath: false,
			podName: "koinon-gateway-1", drain: 5 * time.Second, shutdownDelay: 50 * time.Millisecond,
		}
		ctx, term := context.WithCancel(context.Background())
		DeferCleanup(term)
		done := make(chan error, 1)
		go func() { done <- run(ctx, cfg) }()
		Eventually(func() int {
			resp, err := http.Get("http://" + cfg.healthAddr + "/readyz")
			if err != nil {
				return 0
			}
			_ = resp.Body.Close()
			return resp.StatusCode
		}, 10*time.Second, 50*time.Millisecond).Should(Equal(http.StatusOK))

		req, _ := http.NewRequestWithContext(specCtx, http.MethodPost, "http://"+cfg.listenAddr+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+fakeKey)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Eventually(got.Load, 10*time.Second, 50*time.Millisecond).Should(Equal(int64(1)), "the usage record reaches koinon-api")

		term()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	}, SpecTimeout(40*time.Second))
})

type bearerRT struct{ key string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}
