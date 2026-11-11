// SPDX-License-Identifier: Apache-2.0

package otlpauth_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/otlpauth"
)

const (
	current  = "koinon_test_not-a-real-token-current-0001"
	previous = "koinon_test_not-a-real-token-previous-002"
)

var _ = Describe("otlpauth", func() {
	var (
		dir     string
		tokens  *otlpauth.Tokens
		metrics *otlpauth.Metrics
		srv     *httptest.Server
		reached int
		clock   time.Time
	)
	// advance moves the test clock past the reload interval, so the next request re-reads the files.
	advance := func() { clock = clock.Add(time.Second) }
	write := func(name, value string) {
		Expect(os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600)).To(Succeed())
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	do := func(path, auth string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		Expect(err).NotTo(HaveOccurred())
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp
	}
	rejected := func(reason string) float64 { return testutil.ToFloat64(metrics.Rejected.WithLabelValues(reason)) }

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		write(otlpauth.TokenFile, current+"\n")
		var err error
		tokens, err = otlpauth.NewTokens(dir, quiet)
		Expect(err).NotTo(HaveOccurred())
		clock = time.Unix(1_800_000_000, 0)
		otlpauth.SetClock(tokens, func() time.Time { return clock })
		tokens.Reload = time.Second
		metrics = otlpauth.NewMetrics(prometheus.NewRegistry())
		reached = 0
		// One mux with two paths: the middleware wraps all of it, as it will wrap /v1/traces.
		mux := http.NewServeMux()
		for _, p := range []string{"/v1/logs", "/v1/traces"} {
			mux.HandleFunc("POST "+p, func(w http.ResponseWriter, _ *http.Request) { reached++ })
		}
		srv = httptest.NewServer(otlpauth.Middleware(tokens, metrics)(mux))
		DeferCleanup(srv.Close)
	})

	It("refuses a request without a token, with no detail", func() {
		resp := do("/v1/logs", "")
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		body, _ := io.ReadAll(resp.Body)
		Expect(body).To(BeEmpty())
		Expect(do("/v1/logs", "Basic "+current).StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(do("/v1/logs", "Bearer ").StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(reached).To(BeZero())
		Expect(rejected("missing")).To(Equal(3.0))
	})

	It("refuses a wrong token, on every path", func() {
		for _, p := range []string{"/v1/logs", "/v1/traces", "/anything"} {
			Expect(do(p, "Bearer "+current+"x").StatusCode).To(Equal(http.StatusUnauthorized), p)
		}
		Expect(reached).To(BeZero())
		Expect(rejected("invalid")).To(Equal(3.0))
	})

	It("accepts the right token on both OTLP paths, and 404s only after authenticating", func() {
		Expect(do("/v1/logs", "Bearer "+current).StatusCode).To(Equal(http.StatusOK))
		Expect(do("/v1/traces", "bearer "+current).StatusCode).To(Equal(http.StatusOK))
		Expect(reached).To(Equal(2))
		Expect(do("/nothing", "Bearer "+current).StatusCode).To(Equal(http.StatusNotFound))
		Expect(rejected("missing") + rejected("invalid")).To(BeZero())
	})

	It("accepts the previous token during a rotation, and not once it is removed", func() {
		write(otlpauth.TokenFile, previous)
		write(otlpauth.PreviousFile, current)
		advance()
		Expect(do("/v1/logs", "Bearer "+previous).StatusCode).To(Equal(http.StatusOK))
		Expect(do("/v1/logs", "Bearer "+current).StatusCode).To(Equal(http.StatusOK))

		Expect(os.Remove(filepath.Join(dir, otlpauth.PreviousFile))).To(Succeed())
		advance()
		Expect(do("/v1/logs", "Bearer "+previous).StatusCode).To(Equal(http.StatusOK))
		Expect(do("/v1/logs", "Bearer "+current).StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("keeps the tokens it has when the files become unreadable", func() {
		Expect(os.Remove(filepath.Join(dir, otlpauth.TokenFile))).To(Succeed())
		advance()
		Expect(do("/v1/logs", "Bearer "+current).StatusCode).To(Equal(http.StatusOK))
	})

	It("treats an empty or whitespace-only previous as absent, at start and later", func() {
		write(otlpauth.PreviousFile, " \n")
		t2, err := otlpauth.NewTokens(dir, quiet)
		Expect(err).NotTo(HaveOccurred())
		Expect(t2.Match(current)).To(BeTrue())
		Expect(t2.Match("")).To(BeFalse())
		Expect(t2.Match(previous)).To(BeFalse())
		write(otlpauth.PreviousFile, "")
		advance()
		Expect(do("/v1/logs", "Bearer "+current).StatusCode).To(Equal(http.StatusOK))
	})

	It("never lets a bad previous block a new token, and accepts the old one only through a valid previous", func() {
		write(otlpauth.TokenFile, previous)
		write(otlpauth.PreviousFile, "short")
		advance()
		Expect(do("/v1/logs", "Bearer "+previous).StatusCode).To(Equal(http.StatusOK))
		Expect(do("/v1/logs", "Bearer "+current).StatusCode).To(Equal(http.StatusUnauthorized))
		write(otlpauth.PreviousFile, current)
		advance()
		Expect(do("/v1/logs", "Bearer "+current).StatusCode).To(Equal(http.StatusOK))
	})

	It("starts with an invalid previous, ignoring it", func() {
		write(otlpauth.PreviousFile, "bad value with spaces")
		t2, err := otlpauth.NewTokens(dir, quiet)
		Expect(err).NotTo(HaveOccurred())
		Expect(t2.Match(current)).To(BeTrue())
	})

	It("does not start without a valid token", func() {
		_, err := otlpauth.NewTokens("", quiet)
		Expect(err).To(HaveOccurred())
		_, err = otlpauth.NewTokens(GinkgoT().TempDir(), quiet)
		Expect(err).To(HaveOccurred())
		bad := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(bad, otlpauth.TokenFile), []byte("short"), 0o600)).To(Succeed())
		_, err = otlpauth.NewTokens(bad, quiet)
		Expect(err).To(MatchError(ContainSubstring("at least")))
		Expect(err.Error()).NotTo(ContainSubstring("short"))
	})
})

// Two scopes on one listener (issue 55): the gateway's token for /v1/logs, agentd's for /v1/traces.
var _ = Describe("otlpauth scopes", func() {
	const (
		gateway = "koinon_test_not-a-real-token-gateway-0001"
		agentd  = "koinon_test_not-a-real-token-agentd-00001"
	)
	var (
		srv     *httptest.Server
		reached int
		metrics *otlpauth.Metrics
	)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	set := func(token string) *otlpauth.Tokens {
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, otlpauth.TokenFile), []byte(token), 0o600)).To(Succeed())
		t, err := otlpauth.NewTokens(dir, quiet)
		Expect(err).NotTo(HaveOccurred())
		return t
	}
	status := func(path, token string) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp.StatusCode
	}

	BeforeEach(func() {
		reached = 0
		metrics = otlpauth.NewMetrics(prometheus.NewRegistry())
		mux := http.NewServeMux()
		for _, p := range []string{"/v1/logs", "/v1/traces"} {
			mux.HandleFunc("POST "+p, func(w http.ResponseWriter, _ *http.Request) { reached++ })
		}
		srv = httptest.NewServer(otlpauth.ScopedMiddleware(set(gateway), map[string]*otlpauth.Tokens{"/v1/traces": set(agentd)}, metrics)(mux))
		DeferCleanup(srv.Close)
	})

	It("admits each token on its own path", func() {
		Expect(status("/v1/logs", gateway)).To(Equal(http.StatusOK))
		Expect(status("/v1/traces", agentd)).To(Equal(http.StatusOK))
		Expect(reached).To(Equal(2))
	})

	It("refuses the gateway's token on /v1/traces", func() {
		Expect(status("/v1/traces", gateway)).To(Equal(http.StatusUnauthorized))
		Expect(reached).To(BeZero())
	})

	It("refuses agentd's token on /v1/logs and on every other path", func() {
		for _, p := range []string{"/v1/logs", "/anything"} {
			Expect(status(p, agentd)).To(Equal(http.StatusUnauthorized), p)
		}
		Expect(reached).To(BeZero())
		Expect(testutil.ToFloat64(metrics.Rejected.WithLabelValues("invalid"))).To(Equal(2.0))
	})

	It("does not let a path variant of /v1/traces fall back to agentd's scope", func() {
		Expect(status("/v1/traces/", agentd)).To(Equal(http.StatusUnauthorized))
		Expect(status("//v1/traces", agentd)).To(Equal(http.StatusUnauthorized))
	})
})
