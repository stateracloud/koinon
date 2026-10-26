// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
)

var _ = Describe("metrics", func() {
	It("counts ingested and rejected records, and observes the ingest lag", func(ctx SpecContext) {
		needDocker()
		e := newEnv(ctx, uniq("metrics"), "", "10.00", 100_000)
		reg := prometheus.NewRegistry()
		m := ledger.NewMetrics(reg)
		srv := httptest.NewServer(ledger.Handler(&ledger.Ingester{DB: db, Metrics: m}, slog.New(slog.NewTextHandler(io.Discard, nil))))
		DeferCleanup(srv.Close)

		agent := e.slug + "/" + e.agent.ID.String()
		body, err := proto.Marshal(export(
			llmRecord(str(gateway.LogFieldAgent, agent), str(gateway.LogFieldRequest, uniq("req"))),
			llmRecord(str(gateway.LogFieldAgent, agent), str(gateway.LogFieldRequest, uniq("req"))),
			llmRecord(str(gateway.LogFieldAgent, agent), str(gateway.LogFieldRequest, uniq("req")), str(gateway.LogFieldKey, "")),
			llmRecord(str(gateway.LogFieldAgent, "no-such-org/"+e.agent.ID.String()), str(gateway.LogFieldRequest, uniq("req"))),
		))
		Expect(err).NotTo(HaveOccurred())
		post := func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+ledger.LogsPath, bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/x-protobuf")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}
		post()

		Expect(testutil.ToFloat64(m.Ingested)).To(Equal(2.0))
		Expect(testutil.ToFloat64(m.Rejected.WithLabelValues("missing_key"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.Rejected.WithLabelValues("unattributed"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.Rejected.WithLabelValues("invalid_agent"))).To(Equal(0.0))
		n, err := testutil.GatherAndCount(reg, "koinon_api_ledger_ingest_lag_seconds")
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))

		By("a replay records nothing new and adds no lag observation")
		post()
		Expect(testutil.ToFloat64(m.Ingested)).To(Equal(2.0))
	})
})
