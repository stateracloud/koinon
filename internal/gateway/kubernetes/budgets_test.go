// SPDX-License-Identifier: Apache-2.0

package kubernetes_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stateracloud/koinon/internal/gateway"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
)

// otlpSink is the spend ledger's OTLP/HTTP receiver in miniature: it decodes the access log with the
// ledger's own decoder and hands every charged request to a callback.
type otlpSink struct {
	*httptest.Server
	mu      sync.Mutex
	entries []sunk
	onEntry func(sunk)
}

type sunk struct {
	ledger.Entry
	At time.Time
}

func newSink(host string) *otlpSink {
	s := &otlpSink{}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "0.0.0.0:0")
	Expect(err).NotTo(HaveOccurred())
	s.Server = &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		entries, _, _ := ledger.Decode(&req)
		now := time.Now()
		for _, e := range entries {
			se := sunk{Entry: e, At: now}
			s.mu.Lock()
			s.entries = append(s.entries, se)
			cb := s.onEntry
			s.mu.Unlock()
			if cb != nil {
				cb(se)
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
		_, _ = w.Write(out)
	}), ReadHeaderTimeout: 10 * time.Second}}
	s.Start()
	return s
}

func (s *otlpSink) hostPort(host string) string {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	return net.JoinHostPort(host, p)
}

func (s *otlpSink) all() []sunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sunk(nil), s.entries...)
}

var _ = Describe("kubernetes mode: fallback budgets (plan items S2 and G2)", Ordered, func() {
	var (
		g              *kubeGateway
		sink           *otlpSink
		state          gateway.State
		upHits         atomic.Int64
		keyA, keyB, kC gateway.IssuedKey
		agentA         = gateway.AgentRef{Org: "acme", Name: "a1"}
		agentB         = gateway.AgentRef{Org: "acme", Name: "a2"}
		agentC         = gateway.AgentRef{Org: "globex", Name: "g1"}
	)
	const model = "fake/test-model"

	chat := func(ctx context.Context, material string) int {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/v1/chat/completions", bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	BeforeAll(func(ctx SpecContext) {
		c := needCluster()
		up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			upHits.Add(1)
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`))
		}))
		l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "0.0.0.0:0")
		Expect(err).NotTo(HaveOccurred())
		up.Listener = l
		up.Start()
		DeferCleanup(up.Close)
		sink = newSink(c.host)
		DeferCleanup(sink.Close)

		g, err = startKube(ctx, nil, nil, gwk8s.Options{AccessLogOTLP: &standalone.OTLPExport{HostPort: sink.hostPort(c.host)}})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(g.stop)

		for ref, dst := range map[gateway.AgentRef]*gateway.IssuedKey{agentA: &keyA, agentB: &keyB, agentC: &kC} {
			*dst, err = gateway.NewKey(ref)
			Expect(err).NotTo(HaveOccurred())
		}
		_, upPort, _ := net.SplitHostPort(up.Listener.Addr().String())
		base := fmt.Sprintf("http://%s:%s/v1", c.host, upPort)
		// $50 per million tokens: 20 tokens cost $0.001.
		price := &gateway.Price{Input: "50", Output: "50"}
		state = gateway.State{Providers: []gateway.Provider{
			{Org: "acme", Name: "up", Type: "fake", BaseURL: base, Models: []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: price}}},
			{Org: "globex", Name: "up", Type: "fake", BaseURL: base, Models: []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: price}}},
		}}
		for _, k := range []gateway.IssuedKey{keyA, keyB, kC} {
			state.AddKey(k.Key)
		}
		_, err = g.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(5*time.Minute))

	AfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && g != nil {
			AddReportEntry("proxy logs", g.logs(ctx))
		}
	})

	limit := func(agents map[gateway.AgentRef]money.USD) gateway.State {
		s := state
		s.Limits = nil
		for a, amount := range agents {
			s.SetLimit(a, &gateway.Limit{Unit: gateway.USD, Amount: amount, Window: 24 * time.Hour})
		}
		return s
	}

	It("sends the ledger what it needs in the access log", func(ctx SpecContext) {
		Expect(chat(ctx, keyA.Material)).To(Equal(http.StatusOK))
		Eventually(sink.all, 30*time.Second, 200*time.Millisecond).ShouldNot(BeEmpty())
		e := sink.all()[0]
		Expect(e.KeyID).To(Equal(keyA.ID))
		Expect(e.Agent).To(Equal(agentA))
		Expect(e.RequestID).NotTo(BeEmpty())
		Expect(e.InputTokens).To(Equal(int64(10)))
		Expect(e.OutputTokens).To(Equal(int64(10)))
		Expect(e.Status).To(Equal(int64(200)))
		cost, err := strconv.ParseFloat(e.CostUSD, 64)
		Expect(err).NotTo(HaveOccurred(), "priced from the catalog ConfigMap: %q", e.CostUSD)
		Expect(cost).To(BeNumerically("~", 0.001, 1e-9))
		Expect(e.Model).To(Equal(model))
	}, SpecTimeout(2*time.Minute))

	It("revokes the keys of an agent whose limit is spent, without touching others, and restores them", func(ctx SpecContext) {
		start := time.Now()
		_, err := g.driver.Apply(ctx, limit(map[gateway.AgentRef]money.USD{agentA: 0, agentB: money.FromCents(100)}))
		Expect(err).NotTo(HaveOccurred())
		AddReportEntry("revoke: Apply returned after", time.Since(start).String())
		Expect(chat(ctx, keyA.Material)).To(Equal(http.StatusUnauthorized))
		Expect(chat(ctx, keyB.Material)).To(Equal(http.StatusOK), "a limit with an amount left is the ledger's to watch")
		Expect(chat(ctx, kC.Material)).To(Equal(http.StatusOK))

		// The limit is raised (or its window resets): the next Apply puts the key back.
		_, err = g.driver.Apply(ctx, limit(map[gateway.AgentRef]money.USD{agentA: money.FromCents(50)}))
		Expect(err).NotTo(HaveOccurred())
		Expect(chat(ctx, keyA.Material)).To(Equal(http.StatusOK))
	}, SpecTimeout(2*time.Minute))

	It("revokes every key of an org whose cap is spent", func(ctx SpecContext) {
		_, err := g.driver.Apply(ctx, limit(map[gateway.AgentRef]money.USD{agentA: 0, agentB: 0, agentC: money.FromCents(100)}))
		Expect(err).NotTo(HaveOccurred())
		Expect(chat(ctx, keyA.Material)).To(Equal(http.StatusUnauthorized))
		Expect(chat(ctx, keyB.Material)).To(Equal(http.StatusUnauthorized))
		Expect(chat(ctx, kC.Material)).To(Equal(http.StatusOK), "the other org is not affected")
		_, err = g.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(2*time.Minute))

	It("measures how far a key overspends before the ledger loop revokes it", func(ctx SpecContext) {
		// The ledger loop in miniature: every charged request that reaches the OTLP receiver adds its
		// cost; once the total reaches the limit, the key is removed with an Apply. The request rate is
		// ~10 per second, the cost $0.001 per request, the limit $0.010 (ten requests).
		const (
			limitUSD = 0.010
			rate     = 100 * time.Millisecond
		)
		var (
			mu         sync.Mutex
			spent      float64
			crossedAt  time.Time
			appliedAt  time.Time
			exportLags []time.Duration
		)
		sink.mu.Lock()
		sink.onEntry = func(e sunk) {
			if e.KeyID != keyB.ID {
				return
			}
			c, _ := strconv.ParseFloat(e.CostUSD, 64)
			mu.Lock()
			spent += c
			// A request's log record is stamped when the proxy finished it; the lag to the receiver
			// is the export delay.
			exportLags = append(exportLags, e.At.Sub(e.Time))
			cross := spent >= limitUSD-1e-9 && crossedAt.IsZero()
			if cross {
				crossedAt = e.At
			}
			mu.Unlock()
			if cross {
				go func() {
					_, err := g.driver.Apply(context.WithoutCancel(ctx), limit(map[gateway.AgentRef]money.USD{agentB: 0}))
					mu.Lock()
					defer mu.Unlock()
					if err == nil {
						appliedAt = time.Now()
					}
				}()
			}
		}
		sink.mu.Unlock()
		DeferCleanup(func() { sink.mu.Lock(); sink.onEntry = nil; sink.mu.Unlock() })

		ok, firstRefused := 0, time.Time{}
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			switch chat(ctx, keyB.Material) {
			case http.StatusOK:
				ok++
			case http.StatusUnauthorized:
				firstRefused = time.Now()
			}
			if !firstRefused.IsZero() {
				break
			}
			time.Sleep(rate)
		}
		Expect(firstRefused).NotTo(BeZero(), "the key was never revoked")
		Eventually(func() bool { mu.Lock(); defer mu.Unlock(); return !appliedAt.IsZero() }, 30*time.Second).Should(BeTrue())
		mu.Lock()
		defer mu.Unlock()
		over := ok - 10
		AddReportEntry("limit", "$0.010 = 10 requests at 10 requests/s")
		AddReportEntry("requests served", strconv.Itoa(ok))
		AddReportEntry("requests beyond the limit", strconv.Itoa(over))
		AddReportEntry("overshoot USD", fmt.Sprintf("%.3f", float64(over)*0.001))
		AddReportEntry("limit crossed at receiver -> Apply returned", appliedAt.Sub(crossedAt).String())
		AddReportEntry("limit crossed at receiver -> first 401", firstRefused.Sub(crossedAt).String())
		if len(exportLags) > 0 {
			AddReportEntry("access-log export lag (log record -> receiver), first record", exportLags[0].String())
		}
		Expect(over).To(BeNumerically("<=", 25), "bounded by export lag plus revoke latency")
		_, err := g.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(3*time.Minute))

	It("keeps a revoked key revoked across a proxy pod replacement", func(ctx SpecContext) {
		// Spend truth lives in the ledger and the key list in a ConfigMap, so nothing in the proxy
		// pod can hand a key its limit back.
		_, err := g.driver.Apply(ctx, limit(map[gateway.AgentRef]money.USD{agentA: 0}))
		Expect(err).NotTo(HaveOccurred())
		Expect(chat(ctx, keyA.Material)).To(Equal(http.StatusUnauthorized))

		old := g.proxyPods(ctx)
		Expect(old).NotTo(BeEmpty())
		for _, p := range old {
			Expect(g.c.kube.CoreV1().Pods(g.ns).Delete(ctx, p.Name, metav1.DeleteOptions{})).To(Succeed())
		}
		// Throughout the replacement, no request with the revoked key is ever served.
		Eventually(func() bool {
			code := func() int {
				c, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(c, http.MethodPost, g.URL+"/v1/chat/completions", bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)))
				req.Header.Set("Authorization", "Bearer "+keyA.Material)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return 0
				}
				defer func() { _ = resp.Body.Close() }()
				return resp.StatusCode
			}()
			Expect(code).NotTo(Equal(http.StatusOK), "a revoked key was served during the replacement")
			for _, p := range g.proxyPods(ctx) {
				if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil && p.Name != old[0].Name {
					for _, cs := range p.Status.ContainerStatuses {
						if cs.Ready {
							return code == http.StatusUnauthorized
						}
					}
				}
			}
			return false
		}, 2*time.Minute, 200*time.Millisecond).Should(BeTrue())

		Expect(chat(ctx, keyA.Material)).To(Equal(http.StatusUnauthorized), "the new pod does not hand the key back")
		Expect(chat(ctx, keyB.Material)).To(Equal(http.StatusOK), "other keys survive the replacement")
		// An Apply after the replacement is confirmed against the new pod.
		_, err = g.driver.Apply(ctx, limit(map[gateway.AgentRef]money.USD{agentA: 0}))
		Expect(err).NotTo(HaveOccurred())
		_, err = g.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
		Expect(chat(ctx, keyA.Material)).To(Equal(http.StatusOK))
	}, SpecTimeout(5*time.Minute))
})
