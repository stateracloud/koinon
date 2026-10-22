// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/gateway/standalone"
)

var _ = Describe("ProbeWaiter", func() {
	// probeServer answers the probe with the next status from codes (the last
	// one repeats) and checks every probe request.
	probeServer := func(codes ...int) (*httptest.Server, *atomic.Int32) {
		var probes atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			Expect(r.Method).To(Equal(http.MethodPost))
			Expect(r.URL.Path).To(Equal("/v1/chat/completions"))
			Expect(r.Header.Get("Authorization")).To(Equal("Bearer " + standalone.ReloadProbeKey("abc123")))
			var body struct {
				Model string `json:"model"`
			}
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			Expect(body.Model).To(Equal(standalone.ReloadProbeModel))
			n := int(probes.Add(1))
			w.WriteHeader(codes[min(n, len(codes))-1])
		}))
		DeferCleanup(srv.Close)
		return srv, &probes
	}

	It("waits through 401 until the probe key is accepted (429)", func(ctx SpecContext) {
		srv, probes := probeServer(http.StatusUnauthorized, http.StatusUnauthorized, http.StatusTooManyRequests)
		w := standalone.ProbeWaiter{GatewayURL: srv.URL + "/", Interval: 10 * time.Millisecond}
		Expect(w.Wait(ctx, "abc123")).To(Succeed())
		Expect(probes.Load()).To(Equal(int32(3)))
	})

	It("treats any status but 401 as live", func(ctx SpecContext) {
		for _, code := range []int{http.StatusNotFound, http.StatusForbidden} {
			srv, probes := probeServer(code)
			w := standalone.ProbeWaiter{GatewayURL: srv.URL, Interval: 10 * time.Millisecond}
			Expect(w.Wait(ctx, "abc123")).To(Succeed())
			Expect(probes.Load()).To(Equal(int32(1)))
		}
	})

	It("retries while the gateway refuses connections", func(ctx SpecContext) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		addr := l.Addr().String()
		Expect(l.Close()).To(Succeed())

		done := make(chan error, 1)
		w := standalone.ProbeWaiter{GatewayURL: "http://" + addr, Interval: 10 * time.Millisecond}
		go func() { done <- w.Wait(ctx, "abc123") }()
		Consistently(done).WithTimeout(100 * time.Millisecond).ShouldNot(Receive())

		l, err = net.Listen("tcp", addr)
		Expect(err).NotTo(HaveOccurred())
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		_ = srv.Listener.Close()
		srv.Listener = l
		srv.Start()
		DeferCleanup(srv.Close)
		Eventually(done).WithTimeout(5 * time.Second).Should(Receive(BeNil()))
	})

	It("gives up when ctx expires", func(ctx SpecContext) {
		srv, _ := probeServer(http.StatusUnauthorized)
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		w := standalone.ProbeWaiter{GatewayURL: srv.URL, Interval: 10 * time.Millisecond}
		err := w.Wait(short, "abc123")
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(err).To(MatchError(ContainSubstring("not live yet")))
	})

	It("reports a rejected config from the synchronized gauge", func(ctx SpecContext) {
		var metrics atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/metrics") {
				metrics.Add(1)
				_, _ = fmt.Fprint(w, "# TYPE agentgateway_config_synchronized gauge\nagentgateway_config_synchronized 0\n")
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		}))
		DeferCleanup(srv.Close)

		w := standalone.ProbeWaiter{GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond}
		err := w.Wait(ctx, "abc123")
		Expect(errors.Is(err, standalone.ErrConfigRejected)).To(BeTrue(), "%v", err)
		Expect(metrics.Load()).To(Equal(int32(4)), "rejects after consecutive zeros, not the first")
	})

	It("keeps waiting while the gauge reads 1", func(ctx SpecContext) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/metrics") {
				_, _ = fmt.Fprint(w, "agentgateway_config_synchronized 1\n")
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		}))
		DeferCleanup(srv.Close)
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		w := standalone.ProbeWaiter{GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond}
		Expect(w.Wait(short, "abc123")).To(MatchError(context.DeadlineExceeded))
	})
})
