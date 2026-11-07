// SPDX-License-Identifier: Apache-2.0

package openbao_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/openbao"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

// static signs in with a fixed token, counting the sign-ins.
func static(token string, n *atomic.Int64) func(context.Context) (string, time.Duration, error) {
	return func(context.Context) (string, time.Duration, error) {
		if n != nil {
			n.Add(1)
		}
		return token, 0, nil
	}
}

func withToken(token string) *openbao.Wrapper {
	GinkgoHelper()
	w, err := openbao.NewWithLogin(openbao.Config{Address: bao, Key: testKey}, static(token, nil))
	Expect(err).NotTo(HaveOccurred())
	return w
}

// hanging is a listener that accepts connections and never answers.
func hanging() string {
	GinkgoHelper()
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(l.Close)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			DeferCleanup(c.Close)
		}
	}()
	return "http://" + l.Addr().String()
}

var _ = Describe("the OpenBao transit wrapper", func() {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	aad := []byte("koinon-test-aad")

	Describe("the wrapper contract", func() {
		secretstoretest.WrapperContract(
			func() secretstore.Wrapper { return withToken(adminToken) },
			func() secretstore.Wrapper {
				w, err := openbao.NewWithLogin(openbao.Config{Address: hanging(), Key: testKey}, static(adminToken, nil))
				Expect(err).NotTo(HaveOccurred())
				return w
			},
		)
	})

	Describe("the install binding contract (old issue 615)", func() {
		secretstoretest.InstallBindingContract(func() secretstore.Wrapper { return withToken(adminToken) })
	})

	It("names the transit mount and key", func() {
		w := withToken(adminToken)
		Expect(w.Backend()).To(Equal("openbao"))
		Expect(w.KeyID()).To(Equal("transit/" + testKey))
	})

	It("lets a decrypt-only identity unwrap and probe, and refuses its wrap as unreachable", func(ctx SpecContext) {
		wrapped, err := withToken(adminToken).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		ro := withToken(decryptToken)
		got, err := ro.Unwrap(ctx, wrapped, aad)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		Expect(ro.Probe(ctx)).To(Succeed())
		_, err = ro.Wrap(ctx, kek, aad)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).NotTo(ContainSubstring(decryptToken))
	})

	It("reports a missing key, a refused token and a stopped server as unreachable", func(ctx SpecContext) {
		missing, err := openbao.NewWithLogin(openbao.Config{Address: bao, Key: "koinon-test-missing"}, static(adminToken, nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(missing.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable))
		_, err = missing.Wrap(ctx, kek, aad)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))

		bad := withToken("koinon_test_not-a-real-token-revoked")
		err = bad.Probe(ctx)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).NotTo(ContainSubstring("not-a-real-token-revoked"))

		l, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		closed := "http://" + l.Addr().String()
		Expect(l.Close()).To(Succeed())
		down, err := openbao.NewWithLogin(openbao.Config{Address: closed, Key: testKey}, static(rootToken, nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(down.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable))
	})

	It("refuses a ciphertext of another transit key as not opening", func(ctx SpecContext) {
		root(ctx, "/v1/transit/keys/koinon-test-other", map[string]any{})
		other, err := openbao.NewWithLogin(openbao.Config{Address: bao, Key: "koinon-test-other"}, static(adminToken, nil))
		Expect(err).NotTo(HaveOccurred())
		wrapped, err := other.Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		_, err = withToken(adminToken).Unwrap(ctx, wrapped, aad)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("signs in again once when its token is refused", func(ctx SpecContext) {
		// A child token that is revoked after the first call: the wrapper must sign in again.
		out := root(ctx, "/v1/auth/token/create", map[string]any{"policies": []string{"root"}})
		child := out["auth"].(map[string]any)["client_token"].(string)
		var logins atomic.Int64
		w, err := openbao.NewWithLogin(openbao.Config{Address: bao, Key: testKey},
			func(context.Context) (string, time.Duration, error) {
				if logins.Add(1) == 1 {
					return child, 0, nil
				}
				return rootToken, 0, nil
			})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(logins.Load()).To(BeEquivalentTo(1))
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(logins.Load()).To(BeEquivalentTo(1), "a valid token is reused")
		root(ctx, "/v1/auth/token/revoke", map[string]any{"token": child})
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(logins.Load()).To(BeEquivalentTo(2))
	})

	It("binds a KEK to its ID and the install through secretstore.Wrappers", func(ctx SpecContext) {
		ws, err := secretstore.NewKeyAdminWrappers(secretstoretest.BoundOnlyUnset, withToken(adminToken))
		Expect(err).NotTo(HaveOccurred())
		ws = ws.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
		stored, err := ws.Wrap(ctx, "koinon-test-1", kek)
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Backend).To(Equal("openbao"))
		got, err := ws.Unwrap(ctx, "koinon-test-1", stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		_, err = ws.Unwrap(ctx, "koinon-test-2", stored)
		Expect(err).To(MatchError(secretstore.ErrOpen))

		svc, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, withToken(decryptToken))
		Expect(err).NotTo(HaveOccurred())
		svc = svc.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
		got, err = svc.Unwrap(ctx, "koinon-test-1", stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		_, err = svc.Wrap(ctx, "koinon-test-3", kek)
		Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))

		By("a decrypt-only service of another install gets a KEK that does not open, from a KMS that answered")
		other := svc.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallB))
		_, err = other.Unwrap(ctx, "koinon-test-1", stored)
		Expect(err).To(MatchError(secretstore.ErrOpen))
		Expect(err).NotTo(MatchError(secretstore.ErrUnreachable))
	})
})

var _ = Describe("Kubernetes sign-in", func() {
	type login struct {
		Role string `json:"role"`
		JWT  string `json:"jwt"`
	}

	// fake is an OpenBao that accepts one role's sign-in and answers transit decrypts as invalid.
	fake := func(lease int, logins *atomic.Int64, seen *atomic.Value) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
			var l login
			_ = json.NewDecoder(r.Body).Decode(&l)
			seen.Store(l)
			logins.Add(1)
			if l.Role != "koinon-api" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":["invalid role name"]}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"auth":{"client_token":"koinon_test_not-a-real-client-token","lease_duration":%d}}`, lease)
		})
		mux.HandleFunc("POST /v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Vault-Token") != "koinon_test_not-a-real-client-token" || r.Header.Get("X-Vault-Namespace") != "koinon" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["invalid ciphertext length"]}`))
		})
		s := httptest.NewServer(mux)
		DeferCleanup(s.Close)
		return s
	}

	tokenFile := func(jwt string) string {
		GinkgoHelper()
		p := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(p, []byte(jwt+"\n"), 0o600)).To(Succeed())
		return p
	}

	It("signs in with the role and the token file and reuses the token within its lease", func(ctx SpecContext) {
		var logins atomic.Int64
		var seen atomic.Value
		s := fake(3600, &logins, &seen)
		w, err := openbao.New(openbao.Config{
			Address: s.URL, Key: testKey, Role: "koinon-api", Namespace: "koinon",
			TokenFile: tokenFile("koinon_test_not-a-real-jwt"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(logins.Load()).To(BeEquivalentTo(1))
		Expect(seen.Load()).To(Equal(login{Role: "koinon-api", JWT: "koinon_test_not-a-real-jwt"}))
	})

	It("signs in again when the lease runs out", func(ctx SpecContext) {
		var logins atomic.Int64
		var seen atomic.Value
		s := fake(1, &logins, &seen)
		w, err := openbao.New(openbao.Config{
			Address: s.URL, Key: testKey, Role: "koinon-api", Namespace: "koinon",
			TokenFile: tokenFile("koinon_test_not-a-real-jwt"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Probe(ctx)).To(Succeed())
		time.Sleep(900 * time.Millisecond) // past 4/5 of the 1 s lease
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(logins.Load()).To(BeEquivalentTo(2))
	})

	It("reports a refused sign-in and a missing token file as unreachable", func(ctx SpecContext) {
		var logins atomic.Int64
		var seen atomic.Value
		s := fake(3600, &logins, &seen)
		w, err := openbao.New(openbao.Config{
			Address: s.URL, Key: testKey, Role: "koinon-other",
			TokenFile: tokenFile("koinon_test_not-a-real-jwt"),
		})
		Expect(err).NotTo(HaveOccurred())
		err = w.Probe(ctx)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).To(ContainSubstring("invalid role name"))
		Expect(err.Error()).NotTo(ContainSubstring("not-a-real-jwt"))

		w, err = openbao.New(openbao.Config{
			Address: s.URL, Key: testKey, Role: "koinon-api",
			TokenFile: filepath.Join(GinkgoT().TempDir(), "absent"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable))
	})
})

var _ = Describe("configuration", func() {
	DescribeTable("refuses", func(cfg openbao.Config) {
		_, err := openbao.New(cfg)
		Expect(err).To(HaveOccurred())
	},
		Entry("no role", openbao.Config{Address: "https://bao.example", Key: "k"}),
		Entry("no key", openbao.Config{Address: "https://bao.example", Role: "r"}),
		Entry("a key with a slash", openbao.Config{Address: "https://bao.example", Key: "a/b", Role: "r"}),
		Entry("a key that is ..", openbao.Config{Address: "https://bao.example", Key: "..", Role: "r"}),
		Entry("a mount with ..", openbao.Config{Address: "https://bao.example", Key: "k", Role: "r", TransitMount: "a/../b"}),
		Entry("no address", openbao.Config{Key: "k", Role: "r"}),
		Entry("another scheme", openbao.Config{Address: "ftp://bao.example", Key: "k", Role: "r"}),
		Entry("plain http off the loopback", openbao.Config{Address: "http://bao.example:8200", Key: "k", Role: "r"}),
		Entry("a namespace with ..", openbao.Config{Address: "https://bao.example", Key: "k", Role: "r", Namespace: "team/../root"}),
		Entry("a namespace with a space", openbao.Config{Address: "https://bao.example", Key: "k", Role: "r", Namespace: "team koinon"}),
		Entry("a missing CA bundle", openbao.Config{Address: "https://bao.example", Key: "k", Role: "r", CACertFile: "/nonexistent/ca.pem"}),
	)

	DescribeTable("accepts", func(cfg openbao.Config) {
		_, err := openbao.New(cfg)
		Expect(err).NotTo(HaveOccurred())
	},
		Entry("https", openbao.Config{Address: "https://bao.example:8200/", Key: "koinon-kek", Role: "r"}),
		Entry("http on 127.0.0.1", openbao.Config{Address: "http://127.0.0.1:8200", Key: "k", Role: "r"}),
		Entry("http on localhost", openbao.Config{Address: "http://localhost:8200", Key: "k", Role: "r"}),
		Entry("a nested namespace", openbao.Config{Address: "https://bao.example", Key: "k", Role: "r", Namespace: "team/koinon/"}),
		Entry("a nested mount", openbao.Config{Address: "https://bao.example", Key: "k", Role: "r", TransitMount: "koinon/transit"}),
	)
})
