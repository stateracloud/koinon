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
	"github.com/stateracloud/koinon/internal/prices"
)

// These specs run the whole path from agentgateway's exported access log through the driver's
// mapping into the ledger, so they import the driver package (an external test package; the
// ledger itself imports none of it, see TestLedgerKnowsNoGatewayFields). The mapping is specified in
// internal/gateway/agentgateway.

// fixture is one OTLP/HTTP export captured from agentgateway v1.6.0; see
// testdata/agentgateway-v1.6.0-logs.config.yaml for how.
func fixture() []byte {
	b, err := os.ReadFile("../gateway/agentgateway/testdata/agentgateway-v1.6.0-logs.binpb")
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

var _ = Describe("Handler", func() {
	var srv *httptest.Server

	BeforeEach(func() {
		h := agentgateway.Handler(&ledger.Ingester{DB: db, Prices: prices.NewResolver(priceList)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		srv = httptest.NewServer(h)
		DeferCleanup(srv.Close)
	})

	post := func(ctx context.Context, body []byte, header http.Header) (*http.Response, *collogspb.ExportLogsServiceResponse) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+agentgateway.LogsPath, bytes.NewReader(body))
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
			str(agentgateway.LogFieldKey, gateway.ReloadProbeKeyName),
			str(agentgateway.LogFieldAgent, e.ref().String()),
		)))
		Expect(err).NotTo(HaveOccurred())
		resp, out := post(ctx, body, nil)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(out.GetPartialSuccess().GetRejectedLogRecords()).To(BeZero())
		Expect(e.spendEntries(ctx)).To(BeZero())
		Expect(e.run(ctx, run.ID).TokensUsed).To(BeZero())

		// The same record under a real key is charged to the run.
		body, err = proto.Marshal(export(llmRecord(str(agentgateway.LogFieldAgent, e.ref().String()))))
		Expect(err).NotTo(HaveOccurred())
		resp, _ = post(ctx, body, nil)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(e.spendEntries(ctx)).To(Equal(1))
		Expect(e.run(ctx, run.ID).TokensUsed).To(Equal(int64(12)))
	})

	It("reports records of unknown agents as rejected", func(ctx SpecContext) {
		needDocker()
		body, err := proto.Marshal(export(llmRecord(str(agentgateway.LogFieldAgent, "no-such-org/"+fixtureAgent))))
		Expect(err).NotTo(HaveOccurred())
		resp, out := post(ctx, body, nil)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(out.GetPartialSuccess().GetRejectedLogRecords()).To(Equal(int64(1)))
	})
})
