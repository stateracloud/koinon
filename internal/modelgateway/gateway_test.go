// SPDX-License-Identifier: Apache-2.0

package modelgateway_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/modelgateway"
)

var _ = Describe("the model path", func() {
	var (
		gw  *modelgateway.Gateway
		srv *httptest.Server
		s   *sink
		cfg gateway.ModelConfig
	)
	agent := gateway.AgentRef{Org: "acme", Name: "helper"}

	post := func(path, token, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp
	}

	BeforeEach(func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
		}))
		DeferCleanup(up.Close)
		s = &sink{}
		gw = modelgateway.New(modelgateway.Options{Sink: s.put})
		mux := http.NewServeMux()
		for _, p := range gateway.RoutePaths() {
			mux.Handle(p, gw.Handler())
		}
		srv = httptest.NewServer(mux)
		DeferCleanup(srv.Close)
		cfg = gateway.ModelConfig{}
		cfg.Providers = []gateway.Provider{{Org: "acme", Name: "p", Type: "custom", BaseURL: up.URL + "/v1", Models: []gateway.Model{
			{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}},
		}}}
	})

	It("serves a new key within a second of its apply, at the revision the apply returned", func(ctx SpecContext) {
		k, err := gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		cfg.Keys = []gateway.Key{k.Key}
		before := time.Now()
		rev, err := gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(rev).NotTo(BeEmpty())
		Expect(gw.Revision()).To(Equal(rev))
		resp := post("/v1/chat/completions", k.Material, `{"model":"m","messages":[]}`)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(time.Since(before)).To(BeNumerically("<", time.Second))
		Expect(s.of(k.Key.ID)).To(HaveLen(1))
		Expect(s.of(k.Key.ID)[0]).To(And(HaveField("InputTokens", BeEquivalentTo(7)), HaveField("OutputTokens", BeEquivalentTo(3))))
	})

	It("refuses an unknown key, a missing key, an unknown model and a key in the query, all marked", func(ctx SpecContext) {
		k, _ := gateway.NewKey(agent)
		cfg.Keys = []gateway.Key{k.Key}
		_, err := gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		for name, tc := range map[string]struct {
			path, token, body string
			want              int
		}{
			"no key":        {"/v1/chat/completions", "", `{"model":"m"}`, 401},
			"unknown key":   {"/v1/chat/completions", "kgw_nope", `{"model":"m"}`, 401},
			"unknown model": {"/v1/chat/completions", k.Material, `{"model":"zzz"}`, 404},
			"wrong format":  {"/v1/messages", k.Material, `{"model":"m"}`, 404},
			"key in query":  {"/v1/chat/completions?x=" + strings.Replace(k.Material, "_", "%5F", 1), k.Material, `{"model":"m"}`, 403},
		} {
			resp := post(tc.path, tc.token, tc.body)
			Expect(resp.StatusCode).To(Equal(tc.want), name)
			Expect(gateway.DoNotRetry(resp.Header)).To(BeTrue(), name)
		}
		Expect(s.of(k.ID)).To(BeEmpty(), "a refusal writes no usage record")
	})

	It("keeps the old config when an apply is refused", func(ctx SpecContext) {
		_, err := gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		rev := gw.Revision()
		bad := cfg
		bad.Keys = []gateway.Key{{ID: "BAD ID", Hash: "x"}}
		_, err = gw.ApplyModel(ctx, bad)
		Expect(err).To(MatchError(modelgateway.ErrInvalidConfig))
		Expect(gw.Revision()).To(Equal(rev))
	})

	It("answers 503 while draining", func(ctx SpecContext) {
		_, _ = gw.ApplyModel(ctx, cfg)
		gw.Drain()
		Expect(post("/v1/chat/completions", "x", `{}`).StatusCode).To(Equal(http.StatusServiceUnavailable))
	})
})
