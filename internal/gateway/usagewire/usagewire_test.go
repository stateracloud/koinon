// SPDX-License-Identifier: Apache-2.0

package usagewire_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/gateway/usagewire"
	"github.com/stateracloud/koinon/internal/modelformat"
)

func TestUsageWire(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "usagewire")
}

func record(id string) gateway.UsageRecord {
	return gateway.UsageRecord{
		RequestID: id, KeyID: "k1", Agent: gateway.AgentRef{Org: "acme", Name: "helper"},
		Time: time.Unix(1700000000, 0).UTC(), Model: "claude-test", Format: modelformat.Anthropic, Stream: true,
		Status: 200, UpstreamKnown: true, Upstream: true,
		InputTokens: 50, CacheReadTokens: 30, CacheWriteTokens: 20, InputKnown: true,
		OutputTokens: 15, ReasoningTokens: 25, ReasoningKnown: true, OutputKnown: true,
		MaxTokens: 64, RequestBytes: 300, ResponseBytes: -1, ResponseID: "gen-1",
	}
}

var _ = Describe("the usage record on the wire", func() {
	It("decodes to the record that was encoded, with the buckets disjoint", func() {
		in := []gateway.UsageRecord{record("r1")}
		out, rejected, _ := agentgateway.Decode(usagewire.Encode(in))
		Expect(rejected).To(BeZero())
		Expect(out).To(Equal(in))
	})

	It("keeps a failed record without usage, so the ledger estimates it", func() {
		in := gateway.UsageRecord{
			RequestID: "r2", KeyID: "k1", Agent: gateway.AgentRef{Org: "acme", Name: "helper"},
			Time: time.Unix(1700000000, 0).UTC(), Model: "gpt-test", Format: modelformat.OpenAI, Stream: true,
			Status: 200, UpstreamKnown: true, Upstream: true, Error: "Upstream", RequestBytes: 80, ResponseBytes: -1,
		}
		out, rejected, _ := agentgateway.Decode(usagewire.Encode([]gateway.UsageRecord{in}))
		Expect(rejected).To(BeZero())
		Expect(out).To(Equal([]gateway.UsageRecord{in}))
	})
})

var _ = Describe("the exporter", func() {
	It("posts batches to the receiver with the token, and keeps the records while it fails", func(ctx SpecContext) {
		var calls, fail atomic.Int64
		fail.Store(1)
		got := make(chan int, 10)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			Expect(r.URL.Path).To(Equal(agentgateway.LogsPath))
			Expect(r.Header.Get("Authorization")).To(Equal("Bearer koinon_test_not-a-real-token"))
			b, _ := io.ReadAll(r.Body)
			var req collogspb.ExportLogsServiceRequest
			Expect(proto.Unmarshal(b, &req)).To(Succeed())
			if fail.Load() == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			recs, _, _ := agentgateway.Decode(&req)
			got <- len(recs)
		}))
		DeferCleanup(srv.Close)
		e := usagewire.NewExporter(usagewire.ExporterOptions{
			URL: srv.URL, Token: func() (string, error) { return "koinon_test_not-a-real-token", nil }, Interval: 20 * time.Millisecond,
		})
		rctx, stop := context.WithCancel(ctx)
		go e.Run(rctx, time.Second)
		e.Record(record("a"))
		e.Record(record("b"))
		Eventually(calls.Load).Should(BeNumerically(">=", 1))
		Expect(e.Queued()).To(Equal(2), "the receiver failed: the records stay")
		fail.Store(0)
		Eventually(got, 5*time.Second).Should(Receive(Equal(2)))
		Eventually(e.Queued).Should(BeZero())
		stop()
		Eventually(e.Done()).Should(BeClosed())
	}, SpecTimeout(20*time.Second))

	It("does not remove records it never sent when a full queue drops its head during a post", func(ctx SpecContext) {
		release := make(chan struct{})
		var posts atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if posts.Add(1) == 1 {
				<-release
			}
		}))
		DeferCleanup(srv.Close)
		e := usagewire.NewExporter(usagewire.ExporterOptions{
			URL: srv.URL, Token: func() (string, error) { return "x", nil }, MaxQueue: 4, BatchSize: 2, Interval: 10 * time.Millisecond,
		})
		for _, id := range []string{"a", "b", "c", "d"} {
			e.Record(record(id))
		}
		rctx, stop := context.WithCancel(ctx)
		go e.Run(rctx, time.Second)
		Eventually(posts.Load).Should(BeNumerically(">=", 1))
		// The batch (a, b) is in flight. Two more records push both out of the full queue.
		e.Record(record("e"))
		e.Record(record("f"))
		close(release)
		Eventually(e.Queued).Should(BeZero())
		Expect(posts.Load()).To(BeNumerically(">=", 2), "c, d, e and f were still sent")
		stop()
	}, SpecTimeout(20*time.Second))

	It("drops the oldest record of a full queue and counts it", func() {
		e := usagewire.NewExporter(usagewire.ExporterOptions{URL: "http://127.0.0.1:1", Token: func() (string, error) { return "x", nil }, MaxQueue: 2})
		for _, id := range []string{"a", "b", "c"} {
			e.Record(record(id))
		}
		Expect(e.Queued()).To(Equal(2))
		Expect(e.Dropped()).To(Equal(int64(1)))
	})
})
