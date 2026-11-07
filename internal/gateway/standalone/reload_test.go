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
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/standalone"
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

	It("backs off: doubles the wait from Interval up to MaxInterval", func(ctx SpecContext) {
		var (
			mu    sync.Mutex
			times []time.Time
		)
		const polls = 7
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			times = append(times, time.Now())
			if len(times) < polls {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		DeferCleanup(srv.Close)

		w := standalone.ProbeWaiter{GatewayURL: srv.URL, Interval: 20 * time.Millisecond, MaxInterval: 80 * time.Millisecond}
		Expect(w.Wait(ctx, "abc123")).To(Succeed())

		mu.Lock()
		defer mu.Unlock()
		Expect(times).To(HaveLen(polls))
		want := []time.Duration{20, 40, 80, 80, 80, 80}
		for i, ms := range want {
			gap := times[i+1].Sub(times[i])
			Expect(gap).To(BeNumerically(">=", ms*time.Millisecond), "wait %d", i+1)
		}
		// The waits stop doubling at MaxInterval: uncapped, the last one would be 640 ms.
		Expect(times[polls-1].Sub(times[polls-2])).To(BeNumerically("<", 320*time.Millisecond))
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

	// gaugeServer answers the probe with 401 and the gauge with the next reading from values (the
	// last one repeats).
	gaugeServer := func(values ...int) *httptest.Server {
		var metrics atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/metrics") {
				n := int(metrics.Add(1))
				_, _ = fmt.Fprintf(w, "# TYPE agentgateway_config_synchronized gauge\nagentgateway_config_synchronized %d\n", values[min(n, len(values))-1])
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		}))
		DeferCleanup(srv.Close)
		return srv
	}

	It("reports a rejected config when the gauge stays 0 for RejectAfter", func(ctx SpecContext) {
		srv := gaugeServer(0)
		w := standalone.ProbeWaiter{GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond, RejectAfter: 100 * time.Millisecond}
		start := time.Now()
		err := w.Wait(ctx, "abc123")
		Expect(errors.Is(err, standalone.ErrConfigRejected)).To(BeTrue(), "%v", err)
		Expect(time.Since(start)).To(BeNumerically(">=", 100*time.Millisecond), "time, not a count of polls, decides")
	})

	It("does not call a good file rejected while the gauge still reads 0 from the previous rejection (old issue 245)", func(ctx SpecContext) {
		// The ConfigMap takes a minute to deliver the next file: the gauge reads 0 for that long, the
		// probe gets 401, and then the file loads.
		var polls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/metrics") {
				_, _ = fmt.Fprint(w, "agentgateway_config_synchronized 0\n")
				return
			}
			if polls.Add(1) < 15 { // about 300 ms of stale 0 readings: far over the old four polls
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		DeferCleanup(srv.Close)
		w := standalone.ProbeWaiter{GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond, MaxInterval: 20 * time.Millisecond, RejectAfter: time.Minute}
		Expect(w.Wait(ctx, "abc123")).To(Succeed())
	})

	It("returns agentgateway's reason as soon as its log shows a failed reload after the wait began", func(ctx SpecContext) {
		srv := gaugeServer(0)
		log := &fakeReloadLog{reason: "USD budget limits support at most 9 fractional digits", afterCalls: 2}
		w := standalone.ProbeWaiter{GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond, Reload: log}
		before := time.Now()
		err := w.Wait(w.Prepare(ctx), "abc123")
		Expect(errors.Is(err, standalone.ErrConfigRejected)).To(BeTrue(), "%v", err)
		var rej *standalone.ConfigRejectedError
		Expect(errors.As(err, &rej)).To(BeTrue())
		Expect(rej.Reason).To(Equal("USD budget limits support at most 9 fractional digits"))
		Expect(rej.Revision).To(BeEquivalentTo("abc123"))
		Expect(err.Error()).To(ContainSubstring("at most 9 fractional digits"))
		Expect(time.Since(before)).To(BeNumerically("<", 5*time.Second), "no wait for RejectAfter")
		Expect(log.after).To(BeTemporally("~", time.Now().Add(-time.Hour), time.Minute), "nothing logged before delivery: the window start is the baseline")
	})

	It("keeps waiting when the gauge reads 0 and the log shows no new failure", func(ctx SpecContext) {
		srv := gaugeServer(0)
		short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		w := standalone.ProbeWaiter{GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond, Reload: &fakeReloadLog{}}
		Expect(w.Wait(short, "abc123")).To(MatchError(context.DeadlineExceeded))
	})

	It("goes on when the log cannot be read", func(ctx SpecContext) {
		srv := gaugeServer(0)
		w := standalone.ProbeWaiter{
			GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond, RejectAfter: 80 * time.Millisecond,
			Reload: &fakeReloadLog{err: errors.New("forbidden")},
		}
		err := w.Wait(ctx, "abc123")
		Expect(errors.Is(err, standalone.ErrConfigRejected)).To(BeTrue(), "%v", err)
		var rej *standalone.ConfigRejectedError
		Expect(errors.As(err, &rej)).To(BeTrue())
		Expect(rej.Reason).To(BeEmpty())
	})

	It("does not attribute an older failure to this file when the baseline could not be read", func(ctx SpecContext) {
		srv := gaugeServer(0)
		// Prepare's read fails once; afterwards the log shows the previous file's failure.
		log := &fakeReloadLog{reason: "previous file's reason", errOnCalls: 1}
		w := standalone.ProbeWaiter{
			GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 10 * time.Millisecond, RejectAfter: 100 * time.Millisecond, Reload: log,
		}
		err := w.Wait(w.Prepare(ctx), "abc123")
		var rej *standalone.ConfigRejectedError
		Expect(errors.As(err, &rej)).To(BeTrue(), "%v", err)
		Expect(rej.Reason).To(BeEmpty(), "decided by RejectAfter alone, not by an old log line")
		Expect(log.calls).To(Equal(1), "Wait did not read the log")
	})

	It("resets the clock when the gauge reads 1", func(ctx SpecContext) {
		// Zero, one, then zero for good: RejectAfter counts from the last transition to 0.
		srv := gaugeServer(0, 0, 1, 0)
		w := standalone.ProbeWaiter{GatewayURL: srv.URL, StatsURL: srv.URL, Interval: 5 * time.Millisecond, MaxInterval: 20 * time.Millisecond, RejectAfter: 150 * time.Millisecond}
		Expect(errors.Is(w.Wait(ctx, "abc123"), standalone.ErrConfigRejected)).To(BeTrue())
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

type fakeReloadLog struct {
	reason     string
	err        error
	afterCalls int // the failure appears on this call
	errOnCalls int // the first calls fail with a read error
	calls      int
	after      time.Time
}

func (f *fakeReloadLog) LatestReloadFailure(_ context.Context, after time.Time) (standalone.ReloadFailure, bool, error) {
	f.calls++
	f.after = after
	if f.calls <= f.errOnCalls {
		return standalone.ReloadFailure{}, false, errors.New("log read failed")
	}
	if f.err != nil {
		return standalone.ReloadFailure{}, false, f.err
	}
	return standalone.ReloadFailure{At: time.Now(), Reason: f.reason}, f.reason != "" && f.calls >= f.afterCalls, nil
}
