// SPDX-License-Identifier: Apache-2.0

package kubernetes_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stateracloud/koinon/internal/gateway"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// breaking is a Store that damages the objects of one org before they are applied, as a bug or a
// controller-side refusal would: the controller or the proxy then refuses them.
type breaking struct {
	gwk8s.Store
	org    string
	damage func(*unstructured.Unstructured)
}

func (b breaking) Apply(ctx context.Context, o gwk8s.Object) (*unstructured.Unstructured, error) {
	if o.Org == b.org && o.U.GetKind() == gwk8s.KindModel {
		o = gwk8s.Object{Org: o.Org, U: o.U.DeepCopy()}
		b.damage(o.U)
	}
	return b.Store.Apply(ctx, o)
}

var _ = Describe("kubernetes mode: a rejected object (plan item G2)", Ordered, func() {
	var (
		g              *kubeGateway
		state          gateway.State
		keyA, keyG     gateway.IssuedKey
		agentA, agentG = gateway.AgentRef{Org: "acme", Name: "a1"}, gateway.AgentRef{Org: "globex", Name: "g1"}
	)
	const model = "fake/test-model"
	chat := func(ctx context.Context, material string) (int, string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/v1/chat/completions",
			bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	BeforeAll(func(ctx SpecContext) {
		c := needCluster()
		up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`))
		}))
		l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "0.0.0.0:0")
		Expect(err).NotTo(HaveOccurred())
		up.Listener = l
		up.Start()
		DeferCleanup(up.Close)
		_, port, _ := net.SplitHostPort(l.Addr().String())

		g, err = startKube(ctx, nil, nil, gwk8s.Options{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(g.stop)
		keyA, _ = gateway.NewKey(agentA)
		keyG, _ = gateway.NewKey(agentG)
		base := fmt.Sprintf("http://%s:%s/v1", c.host, port)
		price := &gateway.Price{Input: "1", Output: "1"}
		state = gateway.State{Providers: []gateway.Provider{
			{Org: "acme", Name: "up", Type: "fake", BaseURL: base, Models: []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: price}}},
			{Org: "globex", Name: "up", Type: "fake", BaseURL: base, Models: []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: price}}},
		}}
		state.AddKey(keyA.Key)
		state.AddKey(keyG.Key)
		_, err = g.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
		code, body := chat(ctx, keyA.Material)
		Expect(code).To(Equal(http.StatusOK), body)
	}, NodeTimeout(5*time.Minute))

	AfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && g != nil {
			AddReportEntry("proxy logs", g.logs(ctx))
		}
	})

	It("reports an object the controller refuses on its org and keeps serving the others", func(ctx SpecContext) {
		// An authorization rule that is not CEL: the controller answers ResolvedRefs=False.
		g.driver.Store = breaking{Store: g.store, org: "acme", damage: func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{"apiKey.org ==((("}, "spec", "policies", "authorization", "policy", "matchExpressions")
		}}
		// Change the model so that the apply is a real update.
		changed := state
		changed.Providers = append([]gateway.Provider(nil), state.Providers...)
		changed.Providers[0].Models = []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "2", Output: "2"}}}

		rev, err := g.driver.Apply(ctx, changed)
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue(), "got %v", err)
		Expect(rev).NotTo(BeEmpty(), "the revision is live for everything else")
		Expect(rej.Orgs()).To(Equal([]string{"acme"}))
		AddReportEntry("rejection", rej.Error())

		code, body := chat(ctx, keyG.Material)
		Expect(code).To(Equal(http.StatusOK), "globex is not held back: "+body)
	}, SpecTimeout(2*time.Minute))

	It("reports an object the controller never picks up on its org, after a grace period", func(ctx SpecContext) {
		g.driver.Store = breaking{Store: g.store, org: "acme", damage: func(u *unstructured.Unstructured) {
			// A parent that does not exist: the controller reports nothing for it.
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "no-such-gateway"}}, "spec", "parentRefs")
		}}
		g.driver.OrgGrace = 3 * time.Second
		DeferCleanup(func() { g.driver.OrgGrace = 0 })
		_, err := g.driver.Apply(ctx, state)
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue(), "got %v", err)
		Expect(rej.Orgs()).To(Equal([]string{"acme"}))
		Expect(rej.Error()).To(ContainSubstring("did not report"))
		code, body := chat(ctx, keyG.Material)
		Expect(code).To(Equal(http.StatusOK), body)
	}, SpecTimeout(2*time.Minute))

	It("recovers when the object is fixed", func(ctx SpecContext) {
		g.driver.Store = g.store
		_, err := g.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
		code, body := chat(ctx, keyA.Material)
		Expect(code).To(Equal(http.StatusOK), body)
	}, SpecTimeout(time.Minute))

	It("reports an object the API server refuses on its org", func(ctx SpecContext) {
		bad := state
		bad.Providers = append([]gateway.Provider(nil), state.Providers...)
		bad.Providers[0].BaseURL = "ftp://nowhere.example/v1" // the CRD accepts only http and https
		_, err := g.driver.Apply(ctx, bad)
		var rej *gateway.RejectedError
		Expect(errors.As(err, &rej)).To(BeTrue(), "got %v", err)
		Expect(rej.Orgs()).To(Equal([]string{"acme"}))
		code, body := chat(ctx, keyG.Material)
		Expect(code).To(Equal(http.StatusOK), body)
		_, err = g.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(time.Minute))
})
