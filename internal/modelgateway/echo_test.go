// SPDX-License-Identifier: Apache-2.0

package modelgateway_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing/iotest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/modelgateway"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// The fake provider key of these specs: obviously fake and low in entropy (gitleaks scans every commit).
const echoKey = "koinon_test_not-a-real-provider-key"

// A provider key that the upstream echoes in a response body is detected and counted, and the body reaches the
// agent unchanged: the maintainer accepted that risk for v0.2 (#1332, ADR 0097 section 15). The watch streams.
var _ = Describe("the provider key echo watch (#1332)", func() {
	Describe("the stream matcher", func() {
		It("finds the key in one chunk", func() {
			Expect(modelgateway.EchoSeen(echoKey, []byte(`{"error":"bad key `+echoKey+`"}`))).To(BeTrue())
		})

		It("finds the key split across two chunks, at every split point", func() {
			body := []byte(`{"error":"bad key ` + echoKey + `"}`)
			start := bytes.Index(body, []byte(echoKey))
			for cut := start + 1; cut < start+len(echoKey); cut++ {
				Expect(modelgateway.EchoSeen(echoKey, body[:cut], body[cut:])).To(BeTrue(), "cut at %d", cut)
			}
		})

		It("finds the key split across many small chunks", func() {
			out, n, err := modelgateway.EchoPass(echoKey,
				io.NopCloser(iotest.OneByteReader(strings.NewReader("data: "+echoKey+"\n\n"))))
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1), "the watch reports a body once")
			Expect(string(out)).To(Equal("data: " + echoKey + "\n\n"))
		})

		It("does not match a part of the key, or the key with one byte changed", func() {
			part := echoKey[:len(echoKey)-1]
			changed := echoKey[:5] + "X" + echoKey[6:]
			Expect(modelgateway.EchoSeen(echoKey, []byte(part), []byte(" "+part))).To(BeFalse())
			Expect(modelgateway.EchoSeen(echoKey, []byte(changed))).To(BeFalse())
		})

		It("passes a body with no key through with the same bytes", func() {
			body := bytes.Repeat([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"), 500)
			out, n, err := modelgateway.EchoPass(echoKey,
				io.NopCloser(iotest.HalfReader(bytes.NewReader(body))))
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero())
			Expect(out).To(Equal(body))
		})

		It("does not watch a key too short to tell from text", func() {
			Expect(modelgateway.EchoSeen("short", []byte("a short text"))).To(BeFalse())
		})
	})

	Describe("on the model path", func() {
		var (
			met  *modelgateway.Metrics
			logs *bytes.Buffer
		)

		// serve runs the model gateway in front of upstream, with the fake key as the provider key, and returns
		// the gateway's URL and an agent's gateway key.
		serve := func(ctx SpecContext, upstream http.Handler) (string, string) {
			up := httptest.NewServer(upstream)
			DeferCleanup(up.Close)
			keyFile := filepath.Join(GinkgoT().TempDir(), "key")
			Expect(os.WriteFile(keyFile, []byte(echoKey+"\n"), 0o600)).To(Succeed())
			met, logs = modelgateway.NewMetrics(prometheus.NewRegistry()), &bytes.Buffer{}
			gw := modelgateway.New(modelgateway.Options{
				Metrics: met,
				Logger:  slog.New(slog.NewTextHandler(logs, nil)),
			})
			mux := http.NewServeMux()
			for _, p := range gateway.RoutePaths() {
				mux.Handle(p, gw.Handler())
			}
			srv := httptest.NewServer(mux)
			DeferCleanup(srv.Close)
			k, err := gateway.NewKey(gateway.AgentRef{Org: "acme", Name: "helper"})
			Expect(err).NotTo(HaveOccurred())
			cfg := gateway.ModelConfig{Keys: []gateway.Key{k.Key}}
			cfg.Providers = []gateway.Provider{{
				Org: "acme", Name: "p", Type: "custom", BaseURL: up.URL + "/v1", APIKeyFile: keyFile,
				Models: []gateway.Model{{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
			}}
			_, err = gw.ApplyModel(ctx, cfg)
			Expect(err).NotTo(HaveOccurred())
			return srv.URL, k.Material
		}

		call := func(ctx SpecContext, url, material, body string) (int, string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions",
				strings.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+material)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			out, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			return resp.StatusCode, string(out)
		}

		// echoes reads koinon_gateway_model_key_echo_total{answer=...}, the counter the alert KoinonModelKeyEcho reads.
		echoes := func(answer string) float64 { return testutil.ToFloat64(met.KeyEcho.WithLabelValues(answer)) }

		fake := func(sc fakemodel.Script) http.Handler {
			s, err := fakemodel.New(sc)
			Expect(err).NotTo(HaveOccurred())
			return s
		}

		It("counts and logs a key the model echoes in a JSON answer, and the alert's counter rises", func(ctx SpecContext) {
			url, material := serve(ctx, fake(fakemodel.Script{Model: "m", Turns: []fakemodel.Turn{{Text: "your key is " + echoKey}}}))
			status, out := call(ctx, url, material, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			Expect(status).To(Equal(http.StatusOK))
			Expect(out).To(ContainSubstring(echoKey), "detect only: the body reaches the agent unchanged in v0.2")
			Expect(echoes("success")).To(Equal(1.0))
			Expect(echoes("error")).To(BeZero())
			Expect(logs.String()).To(ContainSubstring("provider key echoed in a model response"))
			Expect(logs.String()).NotTo(ContainSubstring(echoKey), "the log line never carries the key")
		})

		// The fake model sends the whole text in one delta, so the key is contiguous in the stream and only the
		// writes cut it. A key spread over several deltas has JSON between its parts and is not found (a known
		// limit, ADR 0097 section 15).
		It("counts a key in an SSE stream that arrives in 7-byte writes", func(ctx SpecContext) {
			url, material := serve(ctx, fake(fakemodel.Script{
				Model: "m", Faults: fakemodel.Faults{SplitWrites: true},
				Turns: []fakemodel.Turn{{Text: "your key is " + echoKey}},
			}))
			status, out := call(ctx, url, material,
				`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			Expect(status).To(Equal(http.StatusOK))
			Expect(out).To(ContainSubstring("data: "))
			Expect(echoes("success")).To(Equal(1.0))
		})

		It("counts a key that the upstream echoes in an error body", func(ctx SpecContext) {
			// The upstream answers with the fake key as a constant: a handler that copies request input into its
			// answer is reflected input for code scanning, and the key is the same bytes.
			url, material := serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"invalid key: Bearer `+echoKey+`"}}`)
			}))
			status, _ := call(ctx, url, material, `{"model":"m","messages":[]}`)
			Expect(status).To(Equal(http.StatusUnauthorized))
			Expect(echoes("error")).To(Equal(1.0))
			Expect(echoes("success")).To(BeZero())
		})

		It("counts a key echoed in a response header and in the body once per response", func(ctx SpecContext) {
			url, material := serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Debug-Auth", "Bearer "+echoKey)
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"message":"no access for `+echoKey+`"}}`)
			}))
			status, _ := call(ctx, url, material, `{"model":"m","messages":[]}`)
			Expect(status).To(Equal(http.StatusForbidden))
			Expect(echoes("error")).To(Equal(1.0))
			Expect(logs.String()).To(ContainSubstring("in=header"))
			Expect(logs.String()).NotTo(ContainSubstring(echoKey))
		})

		It("passes an answer with no key through with the same bytes and counts nothing", func(ctx SpecContext) {
			const answer = `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",` +
				`"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
			url, material := serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, answer)
			}))
			status, out := call(ctx, url, material, `{"model":"m","messages":[]}`)
			Expect(status).To(Equal(http.StatusOK))
			Expect(out).To(Equal(answer))
			Expect(echoes("success")).To(BeZero())
			Expect(echoes("error")).To(BeZero())
		})
	})
})
