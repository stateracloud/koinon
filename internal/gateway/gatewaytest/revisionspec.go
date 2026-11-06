// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// revisionSpec is the revision probe of ADR 0097 decision 4, stated as a behaviour: when Apply
// returns a revision, the gateway serves it. A key added by the Apply works on the very next
// request, and a key removed by it is refused on the very next request, with no wait in the test.
// Each driver probes in its own way (agentgateway: a probe key that only the new config admits);
// the contract is what the caller sees.
func revisionSpec(env Env) {
	Describe(env.Name+": a returned revision is served (ADR 0097)", Ordered, func() {
		var (
			gw    *Gateway
			state gateway.State
			hits  atomic.Int64
		)
		BeforeAll(func(ctx SpecContext) {
			upstream := fakeUpstream(&hits)
			DeferCleanup(upstream.Close)
			state = gateway.State{Providers: []gateway.Provider{{
				Org: "acme", Name: "revisions", Type: "fake", BaseURL: fmt.Sprintf("http://%s:%d/v1", env.Host, port(upstream)),
				Models: []gateway.Model{{Name: testModel, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
			}}}
			var err error
			gw, err = env.Start(ctx, []int{port(upstream)}, nil, "")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		status := func(ctx context.Context, key string) int {
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, testModel)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewBufferString(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			return resp.StatusCode
		}

		It("serves a key on the request after the Apply that adds it, and refuses it after the Apply that removes it", func(ctx SpecContext) {
			var revisions []gateway.Revision
			for i := range 4 {
				k, err := gateway.NewKey(gateway.AgentRef{Org: "acme", Name: fmt.Sprintf("rotating-%d", i)})
				Expect(err).NotTo(HaveOccurred())
				state.AddKey(k.Key)
				rev, err := gw.Driver.Apply(ctx, state)
				Expect(err).NotTo(HaveOccurred())
				Expect(revisions).NotTo(ContainElement(rev), "a changed state is a new revision")
				revisions = append(revisions, rev)
				Expect(status(ctx, k.Material)).To(Equal(http.StatusOK), "the key of revision %s, on the first request", rev)

				Expect(state.RevokeKey(k.ID)).To(BeTrue())
				_, err = gw.Driver.Apply(ctx, state)
				Expect(err).NotTo(HaveOccurred())
				Expect(status(ctx, k.Material)).To(Equal(http.StatusUnauthorized), "the revoked key, on the first request")
			}
		}, SpecTimeout(3*time.Minute))
	})
}
