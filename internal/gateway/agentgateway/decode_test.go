// SPDX-License-Identifier: Apache-2.0

package agentgateway_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// fixture is one OTLP/HTTP export captured from agentgateway v1.6.0; see
// testdata/agentgateway-v1.6.0-logs.config.yaml for how.
func fixture() []byte {
	b, err := os.ReadFile("testdata/agentgateway-v1.6.0-logs.binpb")
	Expect(err).NotTo(HaveOccurred())
	return b
}

func decodeFixture() *collogspb.ExportLogsServiceRequest {
	var req collogspb.ExportLogsServiceRequest
	Expect(proto.Unmarshal(fixture(), &req)).To(Succeed())
	return &req
}

const fixtureAgent = "019a0000-0000-7000-8000-000000000001"

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func export(records ...*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
	}}}
}

func llmRecord(attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	base := []*commonpb.KeyValue{
		str("gen_ai.request.model", "fake/model"),
		num("gen_ai.usage.input_tokens", 5), num("gen_ai.usage.output_tokens", 7),
		str("agw.ai.usage.cost.total", "0.0012"),
		str(agentgateway.LogFieldAgent, "acme/"+fixtureAgent), str(agentgateway.LogFieldKey, "k-aaaaaaaaaaaa"),
		str(agentgateway.LogFieldRequest, "6a0d1c58-7f7e-4b43-8d59-6f0a3c1f9a11"),
	}
	byKey := map[string]int{}
	for i, kv := range base {
		byKey[kv.GetKey()] = i
	}
	for _, kv := range attrs {
		if i, ok := byKey[kv.GetKey()]; ok {
			base[i] = kv
		} else {
			base = append(base, kv)
		}
	}
	return &logspb.LogRecord{ObservedTimeUnixNano: uint64(time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC).UnixNano()), Attributes: base}
}

var _ = Describe("Decode", func() {
	It("extracts the charged requests from a captured agentgateway export", func() {
		entries, rejected, reason := agentgateway.Decode(decodeFixture())
		Expect(rejected).To(BeZero(), reason)
		// The 401 and the 404 (unknown model) are not charged; the priced and the unpriced
		// requests are.
		Expect(entries).To(HaveLen(2))

		priced := entries[0]
		Expect(priced.Agent).To(Equal(gateway.AgentRef{Org: "acme", Name: fixtureAgent}))
		Expect(priced.KeyID).To(Equal("k-aaaaaaaaaaaa"))
		Expect(priced.RequestID).To(Equal("8ded85cd-b31b-41ba-be42-0fe8139d68ee"))
		Expect(priced.Model).To(Equal("fake/test-model"))
		Expect(priced.InputTokens).To(Equal(int64(10)))
		Expect(priced.OutputTokens).To(Equal(int64(30)))
		// 10 x $50/1M + 30 x $50/1M, exactly as the gateway logged it.
		Expect(priced.CostUSD).To(Equal("0.0020"))
		Expect(priced.Time).To(BeTemporally("~", time.Date(2026, 10, 22, 2, 54, 35, 0, time.UTC), time.Minute))

		unpriced := entries[1]
		Expect(unpriced.Model).To(Equal("unpriced/model"))
		Expect(unpriced.CostUSD).To(BeEmpty())
		Expect(unpriced.InputTokens + unpriced.OutputTokens).To(Equal(int64(40)))
		Expect(unpriced.RequestID).NotTo(Equal(priced.RequestID))
	})

	It("rejects charged records it cannot attribute and skips the rest", func() {
		entries, rejected, reason := agentgateway.Decode(export(
			llmRecord(),
			llmRecord(str(agentgateway.LogFieldRequest, "")),
			llmRecord(str(agentgateway.LogFieldAgent, "no-slash")),
			llmRecord(str("agw.ai.usage.cost.total", "-1")),
			llmRecord(str(agentgateway.LogFieldAgent, "")), // unauthenticated: skipped
			&logspb.LogRecord{Attributes: []*commonpb.KeyValue{str("http.path", "/mcp")}},
		))
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].CostUSD).To(Equal("0.0012"))
		Expect(entries[0].Time).To(Equal(time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)))
		Expect(rejected).To(Equal(int64(3)))
		Expect(reason).To(ContainSubstring("request ID"))
	})

	It("drops the reload probe's records, even ones that look charged", func() {
		entries, rejected, reason := agentgateway.Decode(export(
			llmRecord(str(agentgateway.LogFieldKey, gateway.ReloadProbeKeyName)),
			llmRecord(str(agentgateway.LogFieldKey, gateway.ReloadProbeKeyName), str(agentgateway.LogFieldRequest, "")),
			llmRecord(),
		))
		Expect(rejected).To(BeZero(), reason)
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].KeyID).To(Equal("k-aaaaaaaaaaaa"))
	})

	It("charges usage whatever the status, skips only gateway refusals, and estimates the rest", func() {
		strip := func(r *logspb.LogRecord, keys ...string) *logspb.LogRecord {
			r.Attributes = withoutAttrs(r.Attributes, keys...)
			return r
		}
		noUsage := []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "agw.ai.usage.cost.total"}
		withZeroCost := func(r *logspb.LogRecord) *logspb.LogRecord {
			r.Attributes = append(withoutAttrs(r.Attributes, "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "agw.ai.usage.cost.total"), str("agw.ai.usage.cost.total", "0"))
			return r
		}
		id := func(n string) *commonpb.KeyValue { return str(agentgateway.LogFieldRequest, n) }
		entries, rejected, reason := agentgateway.Decode(export(
			// charged: usage under a 500 (an upstream that failed after working)
			llmRecord(num(agentgateway.LogFieldStatus, 500), id("r-500-usage")),
			// skipped: the gateway's own 429 and a 401, zero cost, no usage
			withZeroCost(llmRecord(num(agentgateway.LogFieldStatus, 429), id("r-429"))),
			withZeroCost(llmRecord(num(agentgateway.LogFieldStatus, 401), id("r-401"))),
			// estimated: an upstream 400 with a zero cost, a 2xx without usage, and a record
			// without a status (the client left before the headers)
			withZeroCost(llmRecord(num(agentgateway.LogFieldStatus, 400), num(agentgateway.LogFieldRequestBytes, 10), id("r-400"))),
			strip(llmRecord(num(agentgateway.LogFieldStatus, 200), num(agentgateway.LogFieldMaxTokens, 4000), num(agentgateway.LogFieldRequestBytes, 900), id("r-200")), noUsage...),
			strip(llmRecord(num(agentgateway.LogFieldRequestBytes, 71), id("r-gone")), noUsage...),
			// an older config's record without usage or status stays uncharged
			strip(llmRecord(id("r-old")), noUsage...),
		))
		Expect(rejected).To(BeZero(), reason)
		got := map[string]ledger.Entry{}
		for _, e := range entries {
			got[e.RequestID] = e
		}
		Expect(got).To(HaveLen(4))
		Expect(got["r-500-usage"].InputKnown).To(BeTrue())
		for _, id := range []string{"r-400", "r-200", "r-gone"} {
			Expect(got[id].InputKnown || got[id].OutputKnown).To(BeFalse(), id)
		}
		Expect(got["r-200"].MaxTokens).To(Equal(int64(4000)))
		Expect(got["r-200"].RequestBytes).To(Equal(int64(900)))
	})

	Describe("who answered a request without usage (koinon.upstream, ADR 0056 amendment 2026-10-29)", func() {
		boolean := func(k string, v bool) *commonpb.KeyValue {
			return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
		}
		noUsage := func(r *logspb.LogRecord) *logspb.LogRecord {
			r.Attributes = append(withoutAttrs(r.Attributes, "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "agw.ai.usage.cost.total"), str("agw.ai.usage.cost.total", "0"))
			return r
		}
		decode := func(recs ...*logspb.LogRecord) map[string]ledger.Entry {
			GinkgoHelper()
			entries, rejected, reason := agentgateway.Decode(export(recs...))
			Expect(rejected).To(BeZero(), reason)
			got := map[string]ledger.Entry{}
			for _, e := range entries {
				got[e.RequestID] = e
			}
			return got
		}
		id := func(n string) *commonpb.KeyValue { return str(agentgateway.LogFieldRequest, n) }

		It("keeps a status the provider answered, even one the gateway also uses", func() {
			got := decode(
				noUsage(llmRecord(id("up-403"), num(agentgateway.LogFieldStatus, 403), boolean(agentgateway.LogFieldUpstream, true))),
				noUsage(llmRecord(id("up-401"), num(agentgateway.LogFieldStatus, 401), boolean(agentgateway.LogFieldUpstream, true))),
				noUsage(llmRecord(id("up-400"), num(agentgateway.LogFieldStatus, 400), boolean(agentgateway.LogFieldUpstream, true), num(agentgateway.LogFieldRequestBytes, 10))),
			)
			Expect(got).To(HaveLen(3))
			Expect(got["up-403"]).To(And(HaveField("Upstream", true), HaveField("UpstreamKnown", true), HaveField("Status", int64(403))))
		})

		It("skips a status the gateway made itself", func() {
			got := decode(
				noUsage(llmRecord(id("gw-403"), num(agentgateway.LogFieldStatus, 403), boolean(agentgateway.LogFieldUpstream, false))),
				noUsage(llmRecord(id("gw-429"), num(agentgateway.LogFieldStatus, 429), boolean(agentgateway.LogFieldUpstream, false))),
				// a gateway 400 (an unparsable body) never reached the provider: it is not charged
				noUsage(llmRecord(id("gw-400"), num(agentgateway.LogFieldStatus, 400), boolean(agentgateway.LogFieldUpstream, false), num(agentgateway.LogFieldRequestBytes, 10))),
			)
			Expect(got).To(BeEmpty())
		})

		It("reads a record from a config that does not say as before: the gateway's statuses are skipped, the rest estimated", func() {
			got := decode(
				noUsage(llmRecord(id("old-403"), num(agentgateway.LogFieldStatus, 403))),
				noUsage(llmRecord(id("old-400"), num(agentgateway.LogFieldStatus, 400), num(agentgateway.LogFieldRequestBytes, 10))),
			)
			Expect(got).To(HaveLen(1))
			Expect(got["old-400"].UpstreamKnown).To(BeFalse())
		})
	})

	It("reads the format, stream and error fields, with an absent one as OpenAI, unstreamed, no error", func() {
		id := func(n string) *commonpb.KeyValue { return str(agentgateway.LogFieldRequest, n) }
		streamed := &commonpb.KeyValue{Key: agentgateway.LogFieldStream, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}}
		entries, rejected, reason := agentgateway.Decode(export(
			llmRecord(id("r-cut"), str(agentgateway.LogFieldFormat, string(modelformat.Anthropic)), streamed, str(agentgateway.LogFieldError, "Upstream")),
			llmRecord(id("r-plain")),
		))
		Expect(rejected).To(BeZero(), reason)
		got := map[string]ledger.Entry{}
		for _, e := range entries {
			got[e.RequestID] = e
		}
		Expect(got["r-cut"]).To(And(HaveField("Format", modelformat.Anthropic), HaveField("Stream", true), HaveField("Error", "Upstream")))
		Expect(got["r-plain"]).To(And(HaveField("Format", modelformat.OpenAI), HaveField("Stream", false), HaveField("Error", "")))
	})

	// agentgateway v1.7.0's Messages path sets no reasoning count, so the record carries no
	// koinon.reasoning (a failed log expression is left out); the output count then stays the
	// whole output, the thinking included (#78); with the split, the record's output is without it.
	It("records the thinking split as known only when the gateway logged it, 0 included", func() {
		id := func(n string) *commonpb.KeyValue { return str(agentgateway.LogFieldRequest, n) }
		entries, rejected, reason := agentgateway.Decode(export(
			llmRecord(id("r-messages"), str(agentgateway.LogFieldFormat, string(modelformat.Anthropic)), num(agentgateway.LogFieldOutput, 900)),
			llmRecord(id("r-zero"), num(agentgateway.LogFieldReasoning, 0)),
			llmRecord(id("r-split"), num(agentgateway.LogFieldOutput, 900), num(agentgateway.LogFieldReasoning, 600)),
		))
		Expect(rejected).To(BeZero(), reason)
		got := map[string]ledger.Entry{}
		for _, e := range entries {
			got[e.RequestID] = e
		}
		Expect(got["r-messages"]).To(And(HaveField("ReasoningKnown", false), HaveField("ReasoningTokens", int64(0)), HaveField("OutputTokens", int64(900))))
		Expect(got["r-zero"]).To(And(HaveField("ReasoningKnown", true), HaveField("ReasoningTokens", int64(0))))
		Expect(got["r-split"]).To(And(HaveField("ReasoningKnown", true), HaveField("ReasoningTokens", int64(600)), HaveField("OutputTokens", int64(300))))
	})

	// ADR 0097 decision 3: agentgateway logs totals, the usage record carries disjoint buckets.
	It("maps agentgateway's totals to disjoint buckets", func() {
		id := func(n string) *commonpb.KeyValue { return str(agentgateway.LogFieldRequest, n) }
		entries, rejected, reason := agentgateway.Decode(export(
			llmRecord(id("r-all"), num(agentgateway.LogFieldInput, 100), num(agentgateway.LogFieldCacheRead, 30),
				num(agentgateway.LogFieldCacheWrite, 20), num(agentgateway.LogFieldOutput, 40), num(agentgateway.LogFieldReasoning, 25)),
			// Parts that exceed their total clamp at zero instead of going negative.
			llmRecord(id("r-over"), num(agentgateway.LogFieldInput, 10), num(agentgateway.LogFieldCacheRead, 30),
				num(agentgateway.LogFieldOutput, 5), num(agentgateway.LogFieldReasoning, 25)),
		))
		Expect(rejected).To(BeZero(), reason)
		got := map[string]ledger.Entry{}
		for _, e := range entries {
			got[e.RequestID] = e
		}
		Expect(got["r-all"]).To(And(
			HaveField("InputTokens", int64(50)), HaveField("CacheReadTokens", int64(30)), HaveField("CacheWriteTokens", int64(20)),
			HaveField("OutputTokens", int64(15)), HaveField("ReasoningTokens", int64(25))))
		Expect(got["r-over"]).To(And(HaveField("InputTokens", int64(0)), HaveField("OutputTokens", int64(0))))
	})

	It("still rejects a negative total instead of clamping it to zero", func() {
		id := func(n string) *commonpb.KeyValue { return str(agentgateway.LogFieldRequest, n) }
		entries, rejected, _ := agentgateway.Decode(export(
			llmRecord(id("r-neg-in"), num(agentgateway.LogFieldInput, -5), num(agentgateway.LogFieldCacheRead, 3)),
			llmRecord(id("r-neg-out"), num(agentgateway.LogFieldOutput, -5), num(agentgateway.LogFieldReasoning, 3)),
		))
		Expect(entries).To(BeEmpty())
		Expect(rejected).To(Equal(int64(2)))
	})

	It("accepts numeric costs and tokens", func() {
		rec := llmRecord(&commonpb.KeyValue{
			Key:   "agw.ai.usage.cost.total",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.25}},
		},
			str("gen_ai.usage.input_tokens", "12"))
		entries, rejected, _ := agentgateway.Decode(export(rec))
		Expect(rejected).To(BeZero())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].CostUSD).To(Equal("0.25"))
		Expect(entries[0].InputTokens).To(Equal(int64(12)))
	})
})

var _ = Describe("Handler protocol", func() {
	var srv *httptest.Server

	BeforeEach(func() {
		// No database: these specs never reach a record that would be written.
		srv = httptest.NewServer(agentgateway.Handler(&ledger.Ingester{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
		DeferCleanup(srv.Close)
	})

	post := func(ctx context.Context, body []byte, contentType string) *http.Response {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+agentgateway.LogsPath, bytes.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		return resp
	}

	It("refuses JSON with 415 and garbage with 400", func(ctx SpecContext) {
		Expect(post(ctx, []byte("{}"), "application/json").StatusCode).To(Equal(http.StatusUnsupportedMediaType))
		Expect(post(ctx, []byte{0xff, 0xff, 0xff}, "application/x-protobuf").StatusCode).To(Equal(http.StatusBadRequest))
	})

	It("answers 200 with a partial success for records it cannot attribute, without touching a database", func(ctx SpecContext) {
		// A record with no request ID is rejected by the decoder before the ledger sees it.
		body, err := proto.Marshal(export(llmRecord(str(agentgateway.LogFieldRequest, ""))))
		Expect(err).NotTo(HaveOccurred())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+agentgateway.LogsPath, bytes.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		var out collogspb.ExportLogsServiceResponse
		Expect(proto.Unmarshal(b, &out)).To(Succeed())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(out.GetPartialSuccess().GetRejectedLogRecords()).To(Equal(int64(1)))
	})
})

func withoutAttrs(in []*commonpb.KeyValue, keys ...string) []*commonpb.KeyValue {
	var out []*commonpb.KeyValue
	for _, kv := range in {
		if !slices.Contains(keys, kv.GetKey()) {
			out = append(out, kv)
		}
	}
	return out
}
