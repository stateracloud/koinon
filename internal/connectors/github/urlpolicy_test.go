// SPDX-License-Identifier: Apache-2.0

package github_test

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
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
)

// The default client dials through the egress policy and follows no redirect (issue #657).
var _ = Describe("Client egress policy", func() {
	var app github.App

	BeforeEach(func() {
		f := githubtest.New()
		DeferCleanup(f.Server.Close)
		key, err := github.ParsePrivateKey(f.PEM)
		Expect(err).NotTo(HaveOccurred())
		app = github.App{AppID: f.AppID, InstallationID: f.InstallationID, Key: key}
	})

	loopbackOK := catalog.URLPolicy{AllowedHosts: []string{"127.0.0.0/8"}}

	It("returns a 3xx and never dials the host it names", func() {
		var hits atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		DeferCleanup(other.Close)
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		}))
		DeferCleanup(first.Close)

		c := github.NewClient(github.Options{BaseURL: first.URL, DialContext: loopbackOK.DialContext(&net.Dialer{Timeout: time.Second})})
		_, err := c.GetApp(context.Background(), app)
		Expect(err).To(MatchError(ContainSubstring("302")))
		Expect(hits.Load()).To(BeZero())

		// A client derived for another API base keeps the rule.
		d, err := c.For(first.URL+"/api/v3", "")
		Expect(err).NotTo(HaveOccurred())
		_, err = d.GetApp(context.Background(), app)
		Expect(err).To(MatchError(ContainSubstring("302")))
		Expect(hits.Load()).To(BeZero())
	})

	It("refuses a name that resolves to a denied address at dial time", func() {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		DeferCleanup(srv.Close)
		_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())

		// The name resolved to a public address when the Connection was saved and to loopback now.
		rebound := catalog.URLPolicy{Resolver: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}}
		root := github.NewClient(github.Options{BaseURL: "http://platform.invalid", DialContext: rebound.DialContext(&net.Dialer{Timeout: time.Second})})
		c, err := root.For("http://ghe.rebind.test:"+port, "")
		Expect(err).NotTo(HaveOccurred())
		_, err = c.GetApp(context.Background(), app)
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(hits.Load()).To(BeZero())
	})

	It("dials the platform's own base URL unchecked, even on a private address", func() {
		// --github-api-url is the operator's setting (a private GHE, the e2e fake), not a Connection's.
		denyAll := catalog.URLPolicy{}
		f := githubtest.New()
		DeferCleanup(f.Server.Close)
		c := github.NewClient(github.Options{BaseURL: f.Server.URL, DialContext: denyAll.DialContext(&net.Dialer{Timeout: time.Second})})
		_, err := c.GetApp(context.Background(), app)
		Expect(err).NotTo(MatchError(ContainSubstring("egress policy denies")))
	})
})
