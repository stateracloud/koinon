// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"bytes"
	"compress/gzip"
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
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
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
		str(gateway.LogFieldAgent, "acme/"+fixtureAgent), str(gateway.LogFieldKey, "k-aaaaaaaaaaaa"),
		str(gateway.LogFieldRequest, "6a0d1c58-7f7e-4b43-8d59-6f0a3c1f9a11"),
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
		entries, rejected, reason := ledger.Decode(decodeFixture())
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
		entries, rejected, reason := ledger.Decode(export(
			llmRecord(),
			llmRecord(str(gateway.LogFieldRequest, "")),
			llmRecord(str(gateway.LogFieldAgent, "no-slash")),
			llmRecord(str("agw.ai.usage.cost.total", "-1")),
			llmRecord(str(gateway.LogFieldAgent, "")), // unauthenticated: skipped
			&logspb.LogRecord{Attributes: []*commonpb.KeyValue{str("http.path", "/mcp")}},
		))
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].CostUSD).To(Equal("0.0012"))
		Expect(entries[0].Time).To(Equal(time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)))
		Expect(rejected).To(Equal(int64(3)))
		Expect(reason).To(ContainSubstring(gateway.LogFieldRequest))
	})

	It("drops the reload probe's records, even ones that look charged", func() {
		entries, rejected, reason := ledger.Decode(export(
			llmRecord(str(gateway.LogFieldKey, gateway.ReloadProbeKeyName)),
			llmRecord(str(gateway.LogFieldKey, gateway.ReloadProbeKeyName), str(gateway.LogFieldRequest, "")),
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
		id := func(n string) *commonpb.KeyValue { return str(gateway.LogFieldRequest, n) }
		entries, rejected, reason := ledger.Decode(export(
			// charged: usage under a 500 (an upstream that failed after working)
			llmRecord(num(gateway.LogFieldStatus, 500), id("r-500-usage")),
			// skipped: the gateway's own 429 and a 401, zero cost, no usage
			withZeroCost(llmRecord(num(gateway.LogFieldStatus, 429), id("r-429"))),
			withZeroCost(llmRecord(num(gateway.LogFieldStatus, 401), id("r-401"))),
			// estimated: an upstream 400 with a zero cost, a 2xx without usage, and a record
			// without a status (the client left before the headers)
			withZeroCost(llmRecord(num(gateway.LogFieldStatus, 400), num(gateway.LogFieldRequestBytes, 10), id("r-400"))),
			strip(llmRecord(num(gateway.LogFieldStatus, 200), num(gateway.LogFieldMaxTokens, 4000), num(gateway.LogFieldRequestBytes, 900), id("r-200")), noUsage...),
			strip(llmRecord(num(gateway.LogFieldRequestBytes, 71), id("r-gone")), noUsage...),
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

	It("reads the format, stream and error fields, with an absent one as OpenAI, unstreamed, no error", func() {
		id := func(n string) *commonpb.KeyValue { return str(gateway.LogFieldRequest, n) }
		streamed := &commonpb.KeyValue{Key: gateway.LogFieldStream, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}}
		entries, rejected, reason := ledger.Decode(export(
			llmRecord(id("r-cut"), str(gateway.LogFieldFormat, string(modelformat.Anthropic)), streamed, str(gateway.LogFieldError, "Upstream")),
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

	It("accepts numeric costs and tokens", func() {
		rec := llmRecord(&commonpb.KeyValue{
			Key:   "agw.ai.usage.cost.total",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.25}},
		},
			str("gen_ai.usage.input_tokens", "12"))
		entries, rejected, _ := ledger.Decode(export(rec))
		Expect(rejected).To(BeZero())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].CostUSD).To(Equal("0.25"))
		Expect(entries[0].InputTokens).To(Equal(int64(12)))
	})
})

var _ = Describe("Handler", func() {
	var srv *httptest.Server

	BeforeEach(func() {
		h := ledger.Handler(&ledger.Ingester{DB: db, Prices: prices.NewResolver(priceList)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		srv = httptest.NewServer(h)
		DeferCleanup(srv.Close)
	})

	post := func(ctx context.Context, body []byte, header http.Header) (*http.Response, *collogspb.ExportLogsServiceResponse) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+ledger.LogsPath, bytes.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/x-protobuf")
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		var out collogspb.ExportLogsServiceResponse
		if resp.StatusCode == http.StatusOK {
			Expect(proto.Unmarshal(b, &out)).To(Succeed())
		}
		return resp, &out
	}

	It("refuses JSON and garbage", func(ctx SpecContext) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+ledger.LogsPath, bytes.NewReader([]byte("{}")))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnsupportedMediaType))

		resp, _ = post(ctx, []byte{0xff, 0xff, 0xff}, nil)
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
	})

	It("records the captured export once, however often it is replayed", func(ctx SpecContext) {
		needDocker()
		e := newEnv(ctx, "acme", fixtureAgent, "10.00")

		resp, out := post(ctx, fixture(), nil)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(out.GetPartialSuccess().GetRejectedLogRecords()).To(BeZero())
		Expect(e.spendEntries(ctx)).To(Equal(2))

		// A replay, gzip-encoded this time: nothing new.
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(fixture())
		Expect(zw.Close()).To(Succeed())
		resp, _ = post(ctx, gz.Bytes(), http.Header{"Content-Encoding": {"gzip"}})
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(e.spendEntries(ctx)).To(Equal(2))

		var cost string
		var priced []bool
		Expect(db.QueryRow(ctx, `SELECT sum(cost_usd)::text FROM spend_entries WHERE org_id = $1`, e.org).Scan(&cost)).To(Succeed())
		Expect(ledger.FormatUSD(cost)).To(Equal("0.002"))
		rows, err := db.Query(ctx, `SELECT priced FROM spend_entries WHERE org_id = $1 ORDER BY model`, e.org)
		Expect(err).NotTo(HaveOccurred())
		for rows.Next() {
			var p bool
			Expect(rows.Scan(&p)).To(Succeed())
			priced = append(priced, p)
		}
		Expect(priced).To(Equal([]bool{true, false})) // fake/test-model, unpriced/model
	})

	It("stores nothing for the reload probe: no spend entry, run match or token usage", func(ctx SpecContext) {
		needDocker()
		e := newEnv(ctx, uniq("probe"), "", "10.00")
		at := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC) // llmRecord's time
		run := e.startRun(ctx, e.task(ctx), at.Add(-time.Minute))

		body, err := proto.Marshal(export(llmRecord(
			str(gateway.LogFieldKey, gateway.ReloadProbeKeyName),
			str(gateway.LogFieldAgent, e.ref().String()),
		)))
		Expect(err).NotTo(HaveOccurred())
		resp, out := post(ctx, body, nil)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(out.GetPartialSuccess().GetRejectedLogRecords()).To(BeZero())
		Expect(e.spendEntries(ctx)).To(BeZero())
		Expect(e.run(ctx, run.ID).TokensUsed).To(BeZero())

		// The same record under a real key is charged to the run.
		body, err = proto.Marshal(export(llmRecord(str(gateway.LogFieldAgent, e.ref().String()))))
		Expect(err).NotTo(HaveOccurred())
		resp, _ = post(ctx, body, nil)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(e.spendEntries(ctx)).To(Equal(1))
		Expect(e.run(ctx, run.ID).TokensUsed).To(Equal(int64(12)))
	})

	It("reports records of unknown agents as rejected", func(ctx SpecContext) {
		needDocker()
		body, err := proto.Marshal(export(llmRecord(str(gateway.LogFieldAgent, "no-such-org/"+fixtureAgent))))
		Expect(err).NotTo(HaveOccurred())
		resp, out := post(ctx, body, nil)
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
