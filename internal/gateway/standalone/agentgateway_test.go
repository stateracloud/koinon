// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/testing/agentgatewayimage"
	"github.com/stateracloud/koinon/internal/testing/cfgdeliver"
	"github.com/stateracloud/koinon/internal/testing/containerhost"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var options = standalone.Options{DatabaseURL: "sqlite:///tmp/agentgateway.db?mode=rwc"}

// running is a started standalone agentgateway container.
type running struct {
	gatewaytest.Gateway
	driver   *standalone.Driver
	file     standalone.Deliverer
	statsURL string
}

// startStandalone starts agentgateway in a container on an empty config; files land in the directory
// mounted at /secrets/upstream. guardURL is the MCP leak guard the driver renders ("" for none).
func startStandalone(ctx context.Context, hostPorts []int, files map[string]string, guardURL string) (*running, error) {
	testenv.Require(testenv.Docker)
	secretsDir, err := os.MkdirTemp("", "koinon-agw-secrets-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(secretsDir, 0o755); err != nil {
		return nil, err
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(secretsDir, name), []byte(v), 0o644); err != nil {
			return nil, err
		}
	}
	opts := options
	opts.GuardURL = guardURL
	first, _, err := standalone.Render(gateway.State{}, opts)
	if err != nil {
		return nil, err
	}
	// The config goes in through the Docker API, not a bind mount: OrbStack delivers no inotify events
	// for host-side writes into a bind mount, so agentgateway would miss every reload (#301).
	ctr, err := testcontainers.Run(ctx, agentgatewayimage.Ref(), append(cfgdeliver.ContainerOptions(first),
		testcontainers.WithExposedPorts("4000/tcp", "15020/tcp", "15021/tcp"),
		testcontainers.WithHostPortAccess(hostPorts...),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, secretsDir+":/secrets/upstream:ro")
		}),
		testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForHTTP("/healthz/ready").WithPort("15021/tcp")),
	)...)
	r := &running{}
	r.Stop = func(ctx context.Context) error {
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
	if r.URL, err = containerhost.Endpoint(ctx, ctr, "4000/tcp", "http"); err != nil {
		return nil, err
	}
	if r.statsURL, err = containerhost.Endpoint(ctx, ctr, "15020/tcp", "http"); err != nil {
		return nil, err
	}
	r.file = cfgdeliver.Deliverer{Container: ctr}
	r.driver = &standalone.Driver{
		Options:   opts,
		Deliverer: r.file,
		Waiter:    standalone.ProbeWaiter{GatewayURL: r.URL, StatsURL: r.statsURL, Interval: 200 * time.Millisecond},
	}
	r.Driver = r.driver
	return r, nil
}

// The G1 suite (internal/gateway/gatewaytest), shared with the Kubernetes driver.
var _ = func() bool {
	gatewaytest.Register(gatewaytest.Env{
		Name:      "standalone",
		KnownGaps: gatewaytest.AgentgatewayV170Gaps,
		Host:      testcontainers.HostInternal,
		KeyFile:   func(name string) string { return "/secrets/upstream/" + name },
		Start: func(ctx context.Context, ports []int, files map[string]string, guardURL string) (*gatewaytest.Gateway, error) {
			r, err := startStandalone(ctx, ports, files, guardURL)
			if err != nil {
				return nil, err
			}
			return &r.Gateway, nil
		},
		GatewayBudgets:  true,
		HeaderAllowlist: true,
		// agentgateway v1.7.0 answers these in the key stage, before any response policy runs (koinon-private#342).
		Unmarked: []string{gatewaytest.RefusalLLMKey, gatewaytest.RefusalLLMAuthorization, gatewaytest.RefusalMCPKey, gatewaytest.RefusalBudget},
	})
	return true
}()

var _ = Describe("agentgateway v1.7.0 reload detection (standalone only)", Ordered, func() {
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
		r, err = startStandalone(ctx, []int{portOf(upstream)}, nil, "")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(r.Stop)
		key, err = gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		state = gateway.State{Providers: []gateway.Provider{{
			Org: "acme", Name: "upstream", Type: "fake", BaseURL: fmt.Sprintf("http://%s:%d/v1", testcontainers.HostInternal, portOf(upstream)),
			Models: []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "50", Output: "50"}}},
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

	// reloadLog reads the reload errors from the container's own log, in agentgateway's real format.
	reloadLog := func() standalone.ReloadLog { return containerReloadLog{logs: r.Logs} }

	It("reports a config agentgateway rejects, with its reason", func(ctx SpecContext) {
		w := standalone.ProbeWaiter{GatewayURL: r.URL, StatsURL: r.statsURL, Interval: 200 * time.Millisecond, Reload: reloadLog()}
		prepared := w.Prepare(ctx)
		Expect(r.file.Deliver(ctx, []byte("llm: {models: [{name: 1, provider: nope}]}\n"), "")).To(Succeed())
		err := w.Wait(prepared, "never-rendered")
		Expect(err).To(MatchError(standalone.ErrConfigRejected))
		var rej *standalone.ConfigRejectedError
		Expect(errors.As(err, &rej)).To(BeTrue())
		Expect(rej.Reason).NotTo(BeEmpty(), "agentgateway's reload error is read from its log")

		// The last good config keeps serving, and the next Apply recovers.
		_, err = r.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(time.Minute))

	It("reports a good file that arrives long after a rejected one as live (issue koinon-private#245)", func(ctx SpecContext) {
		// A ConfigMap delivers a file 60 to 80 s after the write; the gauge reads 0 from the earlier
		// rejection until then.
		Expect(r.file.Deliver(ctx, []byte("llm: {models: [{name: 1, provider: nope}]}\n"), "")).To(Succeed())
		failures := func() int { return strings.Count(r.Logs(ctx), "Failed to reload config") }
		Eventually(failures).WithTimeout(30 * time.Second).Should(BeNumerically(">", 0))
		// The file watcher can report one delivery twice; let the second reload finish.
		time.Sleep(time.Second)

		extra, err := gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		next := state
		next.Keys = append(slices.Clone(state.Keys), extra.Key)
		slow := &standalone.Driver{
			Options:   options,
			Deliverer: delayedDeliverer{inner: r.file, delay: 8 * time.Second},
			Waiter:    standalone.ProbeWaiter{GatewayURL: r.URL, StatsURL: r.statsURL, Interval: 200 * time.Millisecond, Reload: reloadLog(), RejectAfter: 30 * time.Second},
		}
		_, err = slow.Apply(ctx, next)
		Expect(err).NotTo(HaveOccurred(), "four polls (3.5 s) used to call this rejected")
		code, body := chat(ctx, extra.Material, model)
		Expect(code).To(Equal(http.StatusOK), body)
	}, SpecTimeout(2*time.Minute))
})

// delayedDeliverer writes the file after delay, as a kubelet that syncs a ConfigMap volume late.
type delayedDeliverer struct {
	inner standalone.Deliverer
	delay time.Duration
}

func (d delayedDeliverer) Deliver(ctx context.Context, config []byte, rev gateway.Revision) error {
	go func() {
		select {
		case <-time.After(d.delay):
			_ = d.inner.Deliver(context.WithoutCancel(ctx), config, rev)
		case <-ctx.Done():
		}
	}()
	return nil
}

// containerReloadLog is a standalone.ReloadLog over a container's log.
type containerReloadLog struct{ logs func(context.Context) string }

func (c containerReloadLog) LatestReloadFailure(ctx context.Context, after time.Time) (standalone.ReloadFailure, bool, error) {
	return standalone.ParseReloadFailure(strings.NewReader(c.logs(ctx)), after)
}

func portOf(s *httptest.Server) int {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

var _ = Describe("golden files", func() {
	It("are accepted by agentgateway --validate-only", func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		dir, err := filepath.Abs("testdata")
		Expect(err).NotTo(HaveOccurred())
		// The full fixture names a provider key file; validation opens it.
		secrets := GinkgoT().TempDir()
		Expect(os.Chmod(secrets, 0o755)).To(Succeed())
		for _, name := range []string{"api_key", "mcp_wiki", "mcp_search"} {
			Expect(os.WriteFile(filepath.Join(secrets, name), []byte("not-a-real-key"), 0o644)).To(Succeed())
		}
		// And the OTLP bearer token file.
		otlp := GinkgoT().TempDir()
		Expect(os.Chmod(otlp, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(otlp, "token"), []byte("koinon_test_not-a-real-token"), 0o644)).To(Succeed())
		for _, f := range fixtures() {
			ctr, err := testcontainers.Run(ctx, agentgatewayimage.Ref(),
				testcontainers.WithCmd("--validate-only", "-f", "/cfg/"+f.name+".yaml"),
				testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
					hc.Binds = append(hc.Binds, dir+":/cfg:ro", secrets+":/secrets/upstream:ro", otlp+":/secrets/otlp:ro")
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

// ADR 0069 pins anthropic-version with llm.models[].requestHeaders.set. agentgateway v1.7.0 now
// forwards the client's anthropic-version instead of replacing it, so the pin must still win.
var _ = Describe("agentgateway v1.7.0 anthropic-version header (ADR 0069)", func() {
	It("a requestHeaders.set pin overrides the client's value; without one the client's is forwarded", func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		var mu sync.Mutex
		seen := map[string]string{}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[r.Header.Get("x-probe")] = r.Header.Get("anthropic-version")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		DeferCleanup(upstream.Close)

		dir := GinkgoT().TempDir()
		Expect(os.Chmod(dir, 0o755)).To(Succeed())
		secrets := GinkgoT().TempDir()
		Expect(os.Chmod(secrets, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(secrets, "api_key"), []byte("not-a-real-key"), 0o644)).To(Succeed())
		model := func(name string, pin bool) string {
			s := fmt.Sprintf(`    - name: %s
      provider:
        custom:
          formats:
            - type: messages
      params:
        baseUrl: http://%s:%d
        apiKey:
          file: /secrets/upstream/api_key
      requestTransformation:
        set:
          x-probe: '"%s"'
`, name, testcontainers.HostInternal, portOf(upstream), name)
			if pin {
				s += "      requestHeaders:\n        set:\n          anthropic-version: \"2023-06-01\"\n"
			}
			return s
		}
		cfg := "gateways:\n  default:\n    port: 4000\nconfig:\n  readinessAddr: '[::]:15021'\nllm:\n  models:\n" + model("pinned", true) + model("unpinned", false)
		Expect(os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644)).To(Succeed())

		ctr, err := testcontainers.Run(ctx, agentgatewayimage.Ref(),
			testcontainers.WithCmd("-f", "/cfg/config.yaml"),
			testcontainers.WithExposedPorts("4000/tcp", "15021/tcp"),
			testcontainers.WithHostPortAccess(portOf(upstream)),
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, dir+":/cfg:ro", secrets+":/secrets/upstream:ro")
			}),
			testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForHTTP("/healthz/ready").WithPort("15021/tcp")),
		)
		DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
		Expect(err).NotTo(HaveOccurred())
		url, err := containerhost.Endpoint(ctx, ctr, "4000/tcp", "http")
		Expect(err).NotTo(HaveOccurred())

		for _, name := range []string{"pinned", "unpinned"} {
			body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, name)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", bytes.NewBufferString(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("anthropic-version", "2099-01-01")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK), string(b))
		}
		mu.Lock()
		defer mu.Unlock()
		Expect(seen).To(HaveKeyWithValue("pinned", "2023-06-01"), "a requestHeaders.set pin overrides the client's version")
		Expect(seen).To(HaveKeyWithValue("unpinned", "2099-01-01"), "v1.7 forwards the client's version when nothing pins it")
	}, SpecTimeout(3*time.Minute))
})
