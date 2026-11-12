// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
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

	It("keeps the dial check on a client derived with a CA bundle", func() {
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		DeferCleanup(srv.Close)
		ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

		denyAll := catalog.URLPolicy{}
		root := github.NewClient(github.Options{BaseURL: "http://platform.invalid", DialContext: denyAll.DialContext(&net.Dialer{Timeout: time.Second})})
		c, err := root.For(srv.URL, ca)
		Expect(err).NotTo(HaveOccurred())
		_, err = c.GetApp(context.Background(), app)
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(hits.Load()).To(BeZero())
	})

	DescribeTable("checks every base URL that is not exactly the platform's",
		func(nearMatch func(platform string) string) {
			f := githubtest.New()
			DeferCleanup(f.Server.Close)
			denyAll := catalog.URLPolicy{}
			root := github.NewClient(github.Options{BaseURL: f.Server.URL, DialContext: denyAll.DialContext(&net.Dialer{Timeout: time.Second})})
			c, err := root.For(nearMatch(f.Server.URL), "")
			Expect(err).NotTo(HaveOccurred())
			_, err = c.GetApp(context.Background(), app)
			Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		},
		Entry("another case", func(u string) string { return strings.ToUpper(u) }),
		Entry("another port", func(u string) string {
			host, _, _ := net.SplitHostPort(strings.TrimPrefix(u, "http://"))
			return "http://" + net.JoinHostPort(host, "1")
		}),
		Entry("a path", func(u string) string { return u + "/api/v3" }),
	)

	It("treats a trailing slash as the platform URL itself", func() {
		// For trims the slash, so it is the same base URL, and the same client comes back.
		f := githubtest.New()
		DeferCleanup(f.Server.Close)
		root := github.NewClient(github.Options{BaseURL: f.Server.URL, DialContext: catalog.URLPolicy{}.DialContext(&net.Dialer{Timeout: time.Second})})
		c, err := root.For(f.Server.URL+"/", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(c).To(BeIdenticalTo(root))
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
