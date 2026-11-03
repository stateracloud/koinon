// SPDX-License-Identifier: Apache-2.0

package openbao_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/openbao"
)

// answering is an OpenBao stand-in whose transit decrypt answers status and body.
func answering(status int, body string) string {
	GinkgoHelper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	DeferCleanup(s.Close)
	return s.URL
}

var _ = Describe("what an answer means", func() {
	aad := []byte("koinon-test-aad")

	// Only OpenBao's own refusal of the ciphertext is ErrOpen, which callers may treat as final;
	// a proxy's 400 page or an unrelated error is not, and neither is a probe's success.
	DescribeTable("a 400 to decrypt", func(body string, open bool) {
		ctx := context.Background()
		w, err := openbao.NewWithLogin(openbao.Config{Address: answering(http.StatusBadRequest, body), Key: testKey},
			static("koinon_test_not-a-real-client-token", nil))
		Expect(err).NotTo(HaveOccurred())
		_, err = w.Unwrap(ctx, []byte("vault:v1:AAAA"), aad)
		if open {
			Expect(err).To(MatchError(secretstore.ErrOpen))
			Expect(w.Probe(ctx)).To(Succeed())
			return
		}
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err).NotTo(MatchError(secretstore.ErrOpen))
		Expect(w.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable))
	},
		Entry("a proxy's HTML page", `<html><body>400 Bad Request</body></html>`, false),
		Entry("an empty body", ``, false),
		Entry("an unrelated OpenBao error", `{"errors":["unsupported path"]}`, false),
		Entry("a key version restored away", `{"errors":["invalid ciphertext: version is too new"]}`, false),
		Entry("a key version retired by policy", `{"errors":["ciphertext or signature version is disallowed by policy (too old)"]}`, false),
		Entry("the missing key", `{"errors":["encryption key not found"]}`, false),
		Entry("another key's ciphertext", `{"errors":["cipher: message authentication failed"]}`, true),
		Entry("a malformed ciphertext", `{"errors":["invalid ciphertext length"]}`, true),
		Entry("no prefix", `{"errors":["invalid ciphertext: no prefix"]}`, true),
	)

	Describe("a redirect", func() {
		// elsewhere is where a redirect points: it must never be called.
		var elsewhere *httptest.Server
		var called atomic.Int64
		BeforeEach(func() {
			called.Store(0)
			elsewhere = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
			DeferCleanup(elsewhere.Close)
		})
		redirecting := func() string {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			DeferCleanup(s.Close)
			return s.URL
		}

		It("is not followed on sign-in, which would re-send the service account token", func(ctx SpecContext) {
			p := filepath.Join(GinkgoT().TempDir(), "token")
			Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt"), 0o600)).To(Succeed())
			w, err := openbao.NewAnyHTTP(openbao.Config{Address: redirecting(), Key: testKey, Role: "koinon-api", TokenFile: p})
			Expect(err).NotTo(HaveOccurred())
			err = w.Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("redirect refused: set the active node's address"))
			Expect(called.Load()).To(BeZero())
		})

		It("is not followed on decrypt, which would re-send the client token", func(ctx SpecContext) {
			w, err := openbao.NewWithLogin(openbao.Config{Address: redirecting(), Key: testKey}, static("koinon_test_not-a-real-client-token", nil))
			Expect(err).NotTo(HaveOccurred())
			_, err = w.Unwrap(ctx, []byte("vault:v1:AAAA"), aad)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("redirect refused"))
			Expect(called.Load()).To(BeZero())
		})
	})
})

var _ = Describe("signing in under concurrency", func() {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	aad := []byte("koinon-test-aad")

	It("signs in once for many concurrent Wrap, Unwrap and Probe calls", func(ctx SpecContext) {
		var logins atomic.Int64
		w, err := openbao.NewWithLogin(openbao.Config{Address: bao, Key: testKey},
			func(context.Context) (string, time.Duration, error) {
				logins.Add(1)
				time.Sleep(50 * time.Millisecond) // a sign-in that takes a while: the others wait for it
				return adminToken, 0, nil
			})
		Expect(err).NotTo(HaveOccurred())
		wrapped, err := withToken(adminToken).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())

		var wg sync.WaitGroup
		errs := make(chan error, 30)
		for i := range 30 {
			wg.Go(func() {
				var err error
				switch i % 3 {
				case 0:
					_, err = w.Wrap(ctx, kek, aad)
				case 1:
					var got []byte
					got, err = w.Unwrap(ctx, wrapped, aad)
					if err == nil && !bytes.Equal(got, kek) {
						err = secretstore.ErrOpen
					}
				default:
					err = w.Probe(ctx)
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(logins.Load()).To(BeEquivalentTo(1))
	})

	It("lets each caller stop waiting for a sign-in on its own context, without failing the others", func(ctx SpecContext) {
		release := make(chan struct{})
		var logins atomic.Int64
		w, err := openbao.NewWithLogin(openbao.Config{Address: bao, Key: testKey},
			func(context.Context) (string, time.Duration, error) {
				logins.Add(1)
				<-release
				return decryptToken, 0, nil
			})
		Expect(err).NotTo(HaveOccurred())

		// The first caller starts the sign-in and gives up; a later one is still served by it.
		short, cancel := context.WithCancel(ctx)
		first := make(chan error, 1)
		go func() { first <- w.Probe(short) }()
		Eventually(logins.Load).Should(BeEquivalentTo(1))
		second := make(chan error, 1)
		go func() { second <- w.Probe(ctx) }()
		cancel()
		Eventually(first).Should(Receive(MatchError(context.Canceled)))
		Consistently(second, 100*time.Millisecond).ShouldNot(Receive())
		close(release)
		Eventually(second).Should(Receive(BeNil()))
		Expect(logins.Load()).To(BeEquivalentTo(1))
	})

	It("reads the service account token again on every sign-in (a projected token rotates)", func(ctx SpecContext) {
		var jwts []string
		var mu sync.Mutex
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
			var l struct {
				JWT string `json:"jwt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&l)
			mu.Lock()
			jwts = append(jwts, l.JWT)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"auth":{"client_token":"koinon_test_not-a-real-client-token","lease_duration":1}}`))
		})
		mux.HandleFunc("POST /v1/transit/decrypt/", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["invalid ciphertext length"]}`))
		})
		s := httptest.NewServer(mux)
		DeferCleanup(s.Close)

		p := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt-1"), 0o600)).To(Succeed())
		w, err := openbao.NewAnyHTTP(openbao.Config{Address: s.URL, Key: testKey, Role: "koinon-api", TokenFile: p})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt-2"), 0o600)).To(Succeed())
		time.Sleep(900 * time.Millisecond) // past 4/5 of the 1 s lease
		Expect(w.Probe(ctx)).To(Succeed())
		mu.Lock()
		defer mu.Unlock()
		Expect(jwts).To(Equal([]string{"koinon_test_not-a-real-jwt-1", "koinon_test_not-a-real-jwt-2"}))
	})
})
