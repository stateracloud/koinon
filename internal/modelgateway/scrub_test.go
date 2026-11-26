// SPDX-License-Identifier: Apache-2.0

package modelgateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/modelgateway"
)

// A provider key is a credential kind of the one list (#1221): the model path gives each key it reads to the
// scrub, with its kind and the provider's org, before the key goes upstream.
var _ = Describe("the provider key scrub (#1221)", func() {
	type recorded struct {
		kind   credkind.Kind
		org    string
		values []string
	}

	It("records each provider key it reads with the provider-key kind and the provider's org", func(ctx SpecContext) {
		const key = "koinon_test_not-a-real-provider-key"
		var (
			mu    sync.Mutex
			recs  []recorded
			sent  []string
			early bool // a key went upstream before the scrub recorded it
		)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			sent = append(sent, r.Header.Get("Authorization"))
			early = early || len(recs) == 0
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}))
		DeferCleanup(up.Close)
		keyFile := filepath.Join(GinkgoT().TempDir(), "key")
		Expect(os.WriteFile(keyFile, []byte(key+"\n"), 0o600)).To(Succeed())

		gw := modelgateway.New(modelgateway.Options{Scrub: func(k credkind.Kind, org string, v ...string) {
			mu.Lock()
			defer mu.Unlock()
			recs = append(recs, recorded{kind: k, org: org, values: v})
		}})
		mux := http.NewServeMux()
		for _, p := range gateway.RoutePaths() {
			mux.Handle(p, gw.Handler())
		}
		srv := httptest.NewServer(mux)
		DeferCleanup(srv.Close)

		agent := gateway.AgentRef{Org: "acme", Name: "helper"}
		k, err := gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		cfg := gateway.ModelConfig{Keys: []gateway.Key{k.Key}}
		cfg.Providers = []gateway.Provider{{
			Org: "acme", Name: "p", Type: "custom", BaseURL: up.URL + "/v1", APIKeyFile: keyFile,
			Models: []gateway.Model{{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
		}}
		_, err = gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+k.Material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_, _ = io.Copy(io.Discard, resp.Body)
		Expect(resp.Body.Close()).To(Succeed())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		mu.Lock()
		defer mu.Unlock()
		Expect(sent).To(Equal([]string{"Bearer " + key}))
		Expect(early).To(BeFalse(), "the key went upstream before the scrub recorded it")
		Expect(recs).To(ConsistOf(recorded{kind: credkind.ProviderKey, org: "acme", values: []string{key}}))
	})
})
