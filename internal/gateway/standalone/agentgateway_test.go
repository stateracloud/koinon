// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/types/container"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/gatewaytest"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

// AgentgatewayImage is the agentgateway release Koinon targets (ADR 0048).
const AgentgatewayImage = "cr.agentgateway.dev/agentgateway:v1.6.0"

// skipWithoutDocker skips the spec when no container runtime is reachable.
func skipWithoutDocker(ctx context.Context) {
	p, err := testcontainers.NewDockerProvider()
	if err == nil {
		defer func() { _ = p.Close() }()
		err = p.Health(ctx)
	}
	if err != nil {
		Skip("Docker is not available: " + err.Error())
	}
}

var options = standalone.Options{DatabaseURL: "sqlite:///tmp/agentgateway.db?mode=rwc"}

// running is a started standalone agentgateway container.
type running struct {
	gatewaytest.Gateway
	driver   *standalone.Driver
	file     standalone.FileDeliverer
	cfgDir   string
	statsURL string
}

// startStandalone starts agentgateway in a container on an empty config; files land in the directory
// mounted at /secrets/upstream.
func startStandalone(ctx context.Context, hostPorts []int, files map[string]string) (*running, error) {
	skipWithoutDocker(ctx)
	cfgDir, err := os.MkdirTemp("", "koinon-agw-cfg-")
	if err != nil {
		return nil, err
	}
	secretsDir, err := os.MkdirTemp("", "koinon-agw-secrets-")
	if err != nil {
		return nil, err
	}
	for _, d := range []string{cfgDir, secretsDir} {
		if err := os.Chmod(d, 0o755); err != nil { //nolint:gosec // test directories
			return nil, err
		}
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(secretsDir, name), []byte(v), 0o644); err != nil { //nolint:gosec // dummy
			return nil, err
		}
	}
	file := standalone.FileDeliverer{Path: filepath.Join(cfgDir, "config.yaml")}
	first, _, err := standalone.Render(gateway.State{}, options)
	if err != nil {
		return nil, err
	}
	if err := file.Deliver(ctx, first, ""); err != nil {
		return nil, err
	}
	ctr, err := testcontainers.Run(ctx, AgentgatewayImage,
		testcontainers.WithCmd("-f", "/cfg/config.yaml"),
		testcontainers.WithExposedPorts("4000/tcp", "15020/tcp", "15021/tcp"),
		testcontainers.WithHostPortAccess(hostPorts...),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, cfgDir+":/cfg:ro", secretsDir+":/secrets/upstream:ro")
		}),
		testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForHTTP("/healthz/ready").WithPort("15021/tcp")),
	)
	r := &running{file: file, cfgDir: cfgDir}
	r.Stop = func(ctx context.Context) error {
		_ = os.RemoveAll(cfgDir)
		_ = os.RemoveAll(secretsDir)
		return testcontainers.TerminateContainer(ctr)
	}
	if err != nil {
		_ = r.Stop(ctx)
		return nil, err
	}
	r.Logs = func(ctx context.Context) string {
		l, err := ctr.Logs(ctx)
		if err != nil {
			return err.Error()
		}
		b, _ := io.ReadAll(l)
		return string(b)
	}
	if r.URL, err = ctr.PortEndpoint(ctx, "4000/tcp", "http"); err != nil {
		return nil, err
	}
	if r.statsURL, err = ctr.PortEndpoint(ctx, "15020/tcp", "http"); err != nil {
		return nil, err
	}
	r.driver = &standalone.Driver{
		Options:   options,
		Deliverer: file,
		Waiter:    standalone.ProbeWaiter{GatewayURL: r.URL, StatsURL: r.statsURL, Interval: 200 * time.Millisecond},
	}
	r.Driver = r.driver
	return r, nil
}

// The G1 suite (internal/gateway/gatewaytest), shared with the Kubernetes driver.
var _ = func() bool {
	gatewaytest.Register(gatewaytest.Env{
		Name:    "standalone",
		Host:    testcontainers.HostInternal,
		KeyFile: func(name string) string { return "/secrets/upstream/" + name },
		Start: func(ctx context.Context, ports []int, files map[string]string) (*gatewaytest.Gateway, error) {
			r, err := startStandalone(ctx, ports, files)
			if err != nil {
				return nil, err
			}
			return &r.Gateway, nil
		},
		GatewayBudgets:         true,
		OrgTargetsNotContacted: true,
	})
	return true
}()

var _ = Describe("agentgateway v1.6.0 reload detection (standalone only)", Ordered, func() {
	var (
		r     *running
		state gateway.State
		hits  atomic.Int64
		key   gateway.IssuedKey
		agent = gateway.AgentRef{Org: "acme", Name: "triage"}
	)
	const model = "fake/test-model"
	chat := func(ctx context.Context, material, name string) (int, string) {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, name)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/v1/chat/completions", bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	BeforeAll(func(ctx SpecContext) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`))
		}))
		DeferCleanup(upstream.Close)
		var err error
		r, err = startStandalone(ctx, []int{portOf(upstream)}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(r.Stop)
		key, err = gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		state = gateway.State{Providers: []gateway.Provider{{
			Org: "acme", Name: "upstream", Type: "fake", BaseURL: fmt.Sprintf("http://%s:%d/v1", testcontainers.HostInternal, portOf(upstream)),
			Models: []gateway.Model{{Name: model, Price: &gateway.Price{Input: "50", Output: "50"}}},
		}}}
		state.AddKey(key.Key)
		_, err = r.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))

	AfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && r != nil {
			AddReportEntry("agentgateway logs", r.Logs(ctx))
		}
	})

	It("detects reloads with the probe key, which never reaches the upstream", func(ctx SpecContext) {
		// BeforeAll's Apply waited on the initial revision's probe.
		Expect(hits.Load()).To(BeZero())
		_, rev, err := standalone.Render(state, options)
		Expect(err).NotTo(HaveOccurred())
		// Not even with a served model name: the zero budget (429, with a
		// database) or the authorization rule (403) refuses the key first.
		code, body := chat(ctx, standalone.ReloadProbeKey(rev), model)
		Expect(code).To(BeElementOf(http.StatusTooManyRequests, http.StatusForbidden), body)

		// A new revision: its probe key is accepted only once it is live.
		extra, err := gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		state.AddKey(extra.Key)
		next, err := r.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
		Expect(next).NotTo(Equal(rev))
		code, _ = chat(ctx, standalone.ReloadProbeKey(rev), model)
		Expect(code).To(Equal(http.StatusUnauthorized), "the previous revision's probe key is gone")
		code, body = chat(ctx, extra.Material, model)
		Expect(code).To(Equal(http.StatusOK), body)
		Expect(hits.Load()).To(Equal(int64(1)), "only the real key's request reached the upstream")

		// A revision that was never delivered is never live.
		short, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		Expect(r.driver.Waiter.Wait(short, "0000000000000000")).To(MatchError(context.DeadlineExceeded))

		Expect(state.RevokeKey(extra.ID)).To(BeTrue())
		_, err = r.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(2*time.Minute))

	It("reports a config agentgateway rejects", func(ctx SpecContext) {
		Expect(r.file.Deliver(ctx, []byte("llm: {models: [{name: 1, provider: nope}]}\n"), "")).To(Succeed())
		err := r.driver.Waiter.Wait(ctx, "never-rendered")
		Expect(err).To(MatchError(standalone.ErrConfigRejected))

		// The last good config keeps serving, and the next Apply recovers.
		_, err = r.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(time.Minute))
})

func portOf(s *httptest.Server) int {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

var _ = Describe("golden files", func() {
	It("are accepted by agentgateway --validate-only", func(ctx SpecContext) {
		skipWithoutDocker(ctx)
		dir, err := filepath.Abs("testdata")
		Expect(err).NotTo(HaveOccurred())
		// The full fixture names a provider key file; validation opens it.
		secrets := GinkgoT().TempDir()
		Expect(os.Chmod(secrets, 0o755)).To(Succeed())
		for _, name := range []string{"api_key", "mcp_wiki", "mcp_search"} {
			Expect(os.WriteFile(filepath.Join(secrets, name), []byte("not-a-real-key"), 0o644)).To(Succeed()) //nolint:gosec // dummy
		}
		for _, f := range fixtures() {
			ctr, err := testcontainers.Run(ctx, AgentgatewayImage,
				testcontainers.WithCmd("--validate-only", "-f", "/cfg/"+f.name+".yaml"),
				testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
					hc.Binds = append(hc.Binds, dir+":/cfg:ro", secrets+":/secrets/upstream:ro")
				}),
				testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForExit()),
			)
			DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
			Expect(err).NotTo(HaveOccurred())
			st, err := ctr.State(ctx)
			Expect(err).NotTo(HaveOccurred())
			logs, _ := ctr.Logs(ctx)
			out, _ := io.ReadAll(logs)
			Expect(st.ExitCode).To(Equal(0), "%s: %s", f.name, out)
		}
	}, NodeTimeout(3*time.Minute))
})
