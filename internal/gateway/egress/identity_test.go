// SPDX-License-Identifier: Apache-2.0

package egress_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// stubVerifier stands in for the Substrate verifier: each token maps to its claims or its error.
type stubVerifier map[string]struct {
	claims actortoken.SubstrateClaims
	err    error
}

func (s stubVerifier) Verify(token string) (actortoken.SubstrateClaims, error) {
	r, ok := s[token]
	if !ok {
		return actortoken.SubstrateClaims{}, actortoken.ErrSignature
	}
	return r.claims, r.err
}

var _ = Describe("egress routes with the Substrate identifier (issue #1386)", func() {
	const (
		host  = "git.example.test"
		known = "koinon-test-known-token"
		down  = "koinon-test-issuer-down-token"
	)
	var (
		gw      *egress.Gateway
		srv     *httptest.Server
		applier egress.StateRoutes
		metrics *egress.Metrics
		logs    *bytes.Buffer
		claims  = actortoken.SubstrateClaims{Atespace: "acme-space", ActorName: "ka-0123456789abcdef-0a1b2c3d", ActorUID: "koinon-test-uid-0001"}
		actor   = gateway.EgressActor{
			Atespace: "acme-space", Name: "ka-0123456789abcdef-0a1b2c3d", UID: "koinon-test-uid-0001",
			Org: "acme", Agent: "builder", Run: "run-1",
		}
		forge = route.Route{
			Name: "acme/forge", Class: route.ClassForge, Org: "acme", Host: host,
			Methods: []string{"GET"}, PathPrefixes: []string{"/acme"},
		}
	)

	BeforeEach(func() {
		actors := &route.ActorTable{}
		id, err := route.NewSubstrateIdentifier(stubVerifier{
			known: {claims: claims},
			down:  {err: actortoken.ErrUnavailable},
		}, actors)
		Expect(err).NotTo(HaveOccurred())
		metrics = egress.NewMetrics(prometheus.NewRegistry())
		logs = &bytes.Buffer{}
		gw, err = egress.New(egress.Options{
			Identifier: id,
			// No request in this suite reaches an upstream: every dial fails.
			Dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("koinon-test: no upstream")
			},
			Metrics: metrics,
			Log:     slog.New(slog.NewJSONHandler(logs, nil)),
		})
		Expect(err).NotTo(HaveOccurred())
		applier = egress.StateRoutes{Gateway: gw, Actors: actors}
		Expect(applier.ApplyEgress(context.Background(), gateway.EgressState{
			EgressRoutes: []route.Route{forge}, EgressActors: []gateway.EgressActor{actor},
		})).To(Succeed())
		srv = httptest.NewServer(gw.Handler())
		DeferCleanup(srv.Close)
	})

	do := func(tokens ...string) *http.Response {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/egress/"+host+"/acme/app.git/info/refs", nil)
		Expect(err).NotTo(HaveOccurred())
		for _, t := range tokens {
			req.Header.Add(route.TokenHeader, t)
		}
		resp, err := srv.Client().Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp
	}

	count := func(reason route.Reason) float64 {
		return testutil.ToFloat64(metrics.Requests.WithLabelValues("none", string(reason)))
	}

	It("passes the gate for a known actor: the request goes on to the upstream", func() {
		resp := do(known)
		// The gate allowed it; the fake dial then fails, so the upstream error is the answer.
		Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal(string(egress.ReasonUpstream)))
		Expect(logs.String()).To(ContainSubstring(`"org":"acme"`))
	})

	DescribeTable("refuses with a fixed status and reason",
		func(status int, reason route.Reason, tokenReason string, tokens ...string) {
			resp := do(tokens...)
			Expect(resp.StatusCode).To(Equal(status))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal(string(reason)))
			Expect(count(reason)).To(Equal(1.0))
			Expect(logs.String()).To(ContainSubstring(`"token_reason":"` + tokenReason + `"`))
			for _, t := range tokens {
				Expect(logs.String()).NotTo(ContainSubstring(t))
			}
		},
		Entry("the placeholder", http.StatusUnauthorized, route.ReasonBadToken, "placeholder", actortoken.Placeholder),
		Entry("two token values", http.StatusUnauthorized, route.ReasonBadToken, "multiple_tokens", known, known),
		Entry("a token the verifier refuses", http.StatusUnauthorized, route.ReasonBadToken, "signature", "koinon-test-forged-token"),
		Entry("issuer keys unavailable", http.StatusServiceUnavailable, route.ReasonIdentityUnavailable, "unavailable", down),
	)

	It("refuses an actor that a State revision removes, with 403 unknown_actor", func() {
		Expect(applier.ApplyEgress(context.Background(), gateway.EgressState{EgressRoutes: []route.Route{forge}})).To(Succeed())
		resp := do(known)
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal(string(route.ReasonUnknownActor)))
		Expect(count(route.ReasonUnknownActor)).To(Equal(1.0))
		// The text that goes to the guest names the reason only: no actor name, UID, org or agent (issue #1485).
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("koinon-gateway: egress refused: unknown_actor\n"))
	})

	It("refuses an actor with another UID", func() {
		other := actor
		other.UID = "koinon-test-uid-0002"
		Expect(applier.ApplyEgress(context.Background(), gateway.EgressState{
			EgressRoutes: []route.Route{forge}, EgressActors: []gateway.EgressActor{other},
		})).To(Succeed())
		Expect(do(known).StatusCode).To(Equal(http.StatusForbidden))
	})

	It("skips a bad actor row, keeps the good actors and still serves the routes (issue #1485)", func() {
		bad := actor
		bad.UID = "koinon-test-uid-0003"
		bad.Agent = ""
		err := applier.ApplyEgress(context.Background(), gateway.EgressState{
			EgressRoutes: []route.Route{forge}, EgressActors: []gateway.EgressActor{actor, bad},
		})
		Expect(err).To(MatchError(ContainSubstring("egress: the actors")))
		Expect(applier.Actors.Len()).To(Equal(1))
		// The gate allows the good actor; the fake dial then fails, so the upstream error is the answer.
		resp := do(known)
		Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal(string(egress.ReasonUpstream)))
	})

	It("removes the actors also when the routes of a revision are refused", func() {
		err := applier.ApplyEgress(context.Background(), gateway.EgressState{
			EgressRoutes: []route.Route{{Name: "no-slash", Org: "acme"}},
		})
		Expect(err).To(HaveOccurred())
		Expect(applier.Actors.Len()).To(BeZero())
	})
})
