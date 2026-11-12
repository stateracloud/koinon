// SPDX-License-Identifier: Apache-2.0

package gitea_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
)

// The default client dials through the egress policy and follows no redirect (issue #657).
var _ = Describe("Host egress policy", func() {
	repo := codehost.Repo{Owner: "acme", Name: "app"}

	It("returns a 3xx and never dials the host it names", func() {
		var hits atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		DeferCleanup(other.Close)
		first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		}))
		DeferCleanup(first.Close)
		policy := catalog.URLPolicy{AllowedHosts: []string{"127.0.0.0/8"}}
		h := gitea.NewHost(gitea.Options{DialContext: policy.DialContext(&net.Dialer{Timeout: time.Second})})

		// The CA bundle derives a client from the base one; it keeps the rule.
		acc, err := h.Open(context.Background(), connection(first.URL, pemOf(first)), read)
		Expect(err).NotTo(HaveOccurred())
		_, err = acc.Repository(context.Background(), repo)
		Expect(err).To(MatchError(ContainSubstring("302")))
		Expect(hits.Load()).To(BeZero())
	})

	It("refuses a denied address without a CA bundle too", func() {
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		DeferCleanup(srv.Close)
		denyAll := catalog.URLPolicy{}
		h := gitea.NewHost(gitea.Options{DialContext: denyAll.DialContext(&net.Dialer{Timeout: time.Second})})
		acc, err := h.Open(context.Background(), connection(srv.URL, ""), read)
		Expect(err).NotTo(HaveOccurred())
		_, err = acc.Repository(context.Background(), repo)
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(hits.Load()).To(BeZero())
	})

	It("refuses a name that resolves to a denied address at dial time", func() {
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		DeferCleanup(srv.Close)
		_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())

		rebound := catalog.URLPolicy{Resolver: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}}
		h := gitea.NewHost(gitea.Options{DialContext: rebound.DialContext(&net.Dialer{Timeout: time.Second})})
		// The connection carries a CA bundle, so clientFor builds a per-bundle client: it keeps the check.
		acc, err := h.Open(context.Background(), connection("https://git.rebind.test:"+port, pemOf(srv)), read)
		Expect(err).NotTo(HaveOccurred())
		_, err = acc.Repository(context.Background(), repo)
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(hits.Load()).To(BeZero())
	})
})
