// SPDX-License-Identifier: Apache-2.0

package gitpush_test

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
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/gitpush"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

// The push writer's network calls dial through the egress policy and refuse HTTP redirects (#860).
var _ = Describe("the push writer's egress", func() {
	var (
		f      *fakegitea.Fake
		repo   = codehost.Repo{Owner: "acme", Name: "app"}
		base   string
		ctx    = context.Background()
		files  = []codehost.FileWrite{{Path: "a.md", Mode: "100644", Content: []byte("a\n")}}
		hits   atomic.Int32 // requests that reached the redirect target
		target *httptest.Server
		// redirectWhen is the front server's switch: a request it matches is answered with a
		// redirect to the target instead of reaching the fake.
		redirectWhen atomic.Pointer[func(*http.Request) bool]
		front        *httptest.Server
		fronted      atomic.Int32 // requests that reached the front server
	)

	BeforeEach(func() {
		f = fakegitea.NewUnstarted()
		f.Server = httptest.NewUnstartedServer(nil)
		f.Server.Config.Handler = f.Handler
		f.Server.Start()
		DeferCleanup(f.Close)
		f.AddToken(token, "koinon-bot")
		f.AddRepo("app")
		base = f.Snapshot("app").Branches["main"]

		hits.Store(0)
		fronted.Store(0)
		redirectWhen.Store(nil)
		target = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		DeferCleanup(target.Close)
		front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fronted.Add(1)
			w.Header().Set("Connection", "close") // every request dials, so the dial check sees each one
			if m := redirectWhen.Load(); m != nil && (*m)(r) {
				http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
				return
			}
			f.Handler.ServeHTTP(w, r)
		}))
		DeferCleanup(front.Close)
	})

	writer := func(url string, dial gitpush.DialFunc) codehost.Access {
		return gitpush.New(reads{tok: token}, gitpush.Options{
			RemoteURL:   func(r codehost.Repo) string { return url + "/" + r.String() + ".git" },
			DialContext: dial,
		})
	}
	redirect := func(m func(*http.Request) bool) { redirectWhen.Store(&m) }
	isAdvert := func(service string) func(*http.Request) bool {
		return func(r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == service
		}
	}
	loopback := catalog.URLPolicy{AllowedHosts: []string{"127.0.0.1"}}.DialContext(&net.Dialer{Timeout: time.Second})

	DescribeTable("refuses a redirect",
		func(dial gitpush.DialFunc) {
			// Fetch: the pack request is redirected.
			redirect(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/git-upload-pack") })
			_, err := writer(front.URL, dial).WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
			Expect(err).To(MatchError(ContainSubstring("redirect")), "fetch")
			Expect(hits.Load()).To(BeZero(), "fetch followed the redirect")

			// List: the ref advertisement is redirected (the fetch falls back to a listing, which fails too).
			redirect(isAdvert("git-upload-pack"))
			_, err = writer(front.URL, dial).WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
			Expect(err).To(MatchError(ContainSubstring("listing refs")), "list")
			Expect(hits.Load()).To(BeZero(), "list followed the redirect")

			// Push: the commit is written, then the receive-pack advertisement is redirected.
			redirectWhen.Store(nil)
			w := writer(front.URL, dial)
			sha, err := w.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
			Expect(err).NotTo(HaveOccurred())
			redirect(isAdvert("git-receive-pack"))
			Expect(w.CreateBranch(ctx, repo, "koinon/x", sha)).NotTo(Succeed(), "push")
			Expect(hits.Load()).To(BeZero(), "push followed the redirect")
			Expect(f.Snapshot("app").Branches).NotTo(HaveKey("koinon/x"))
		},
		Entry("with the policy's dial check", gitpush.DialFunc(loopback)),
		Entry("without a dial check", gitpush.DialFunc(nil)),
	)

	It("refuses a destination the policy forbids before any request, on fetch, list and push", func() {
		var forbid atomic.Bool
		var refused atomic.Int32
		forbidding := catalog.URLPolicy{}.DialContext(&net.Dialer{})
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			if forbid.Load() {
				refused.Add(1)
				return forbidding(ctx, network, "127.0.0.1:1")
			}
			return loopback(ctx, network, addr)
		}
		w := writer(front.URL, dial)

		// Fetch and list: nothing reaches the host.
		forbid.Store(true)
		_, err := w.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(err).To(MatchError(ContainSubstring("listing refs")), "the fetch falls back to a listing, which is refused too")
		Expect(refused.Load()).To(BeNumerically(">=", 2))
		Expect(fronted.Load()).To(BeZero(), "a request reached the host")

		// Push: the commit exists, the destination is forbidden by the time of the push.
		forbid.Store(false)
		sha, err := w.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
		Expect(err).NotTo(HaveOccurred())
		reached := fronted.Load()
		forbid.Store(true)
		Expect(w.CreateBranch(ctx, repo, "koinon/x", sha)).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(fronted.Load()).To(Equal(reached), "the push reached the host")
		Expect(f.Snapshot("app").Branches).NotTo(HaveKey("koinon/x"))
	})

	It("refuses a name that now resolves to a forbidden address, and works once the address is allowed", func() {
		_, port, err := net.SplitHostPort(front.Listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		url := "http://git.rebind.test:" + port
		resolver := func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}

		denied := catalog.URLPolicy{Resolver: resolver}.DialContext(&net.Dialer{Timeout: time.Second})
		_, err = writer(url, denied).WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(fronted.Load()).To(BeZero())

		allowed := catalog.URLPolicy{Resolver: resolver, AllowedHosts: []string{"127.0.0.1"}}.DialContext(&net.Dialer{Timeout: time.Second})
		w := writer(url, allowed)
		sha, err := w.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.CreateBranch(ctx, repo, "koinon/ok", sha)).To(Succeed())
		Expect(f.Snapshot("app").Branches).To(HaveKeyWithValue("koinon/ok", sha))
	})

	It("keeps the dial check on a host with a private CA", func() {
		tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fronted.Add(1) }))
		DeferCleanup(tlsSrv.Close)
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw})
		w := gitpush.New(reads{tok: token}, gitpush.Options{
			RemoteURL:   func(r codehost.Repo) string { return tlsSrv.URL + "/" + r.String() + ".git" },
			CABundle:    ca,
			DialContext: catalog.URLPolicy{}.DialContext(&net.Dialer{Timeout: time.Second}),
		})
		_, err := w.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
		Expect(fronted.Load()).To(BeZero())
	})

	It("still fetches and pushes to a destination the policy allows", func() {
		w := writer(front.URL, loopback)
		sha, err := w.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "m\n", Files: files})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.CreateBranch(ctx, repo, "koinon/allowed", sha)).To(Succeed())
		Expect(f.Snapshot("app").Branches).To(HaveKeyWithValue("koinon/allowed", sha))
		Expect(hits.Load()).To(BeZero())
	})
})
