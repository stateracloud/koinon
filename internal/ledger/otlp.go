// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/procoduck/koinon/internal/gateway"
)

// Access-log attributes agentgateway v1.6.0 sets on LLM requests
// (crates/agentgateway/src/telemetry/log.rs). The cost is the lossless decimal
// total from the model catalog, as a string; it is absent when the model could
// not be priced.
const (
	attrRequestModel = "gen_ai.request.model"
	attrInputTokens  = "gen_ai.usage.input_tokens"  //nolint:gosec // attribute name, not a credential
	attrOutputTokens = "gen_ai.usage.output_tokens" //nolint:gosec // attribute name, not a credential
	attrCostTotal    = "agw.ai.usage.cost.total"
)

// LogsPath is the OTLP/HTTP logs path.
const LogsPath = "/v1/logs"

// maxBody bounds an export request. agentgateway's batch processor sends at
// most 512 records per export, a few KiB each.
const maxBody = 16 << 20

var decimalRE = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,18})?$`)

// Decode extracts the charged LLM requests from an OTLP logs export. Records
// that are not charged requests (MCP calls, rejected requests, requests
// without usage) are skipped. Records that look charged but cannot be
// attributed are counted in rejected, with the first reason in reason.
func Decode(req *collogspb.ExportLogsServiceRequest) (entries []Entry, rejected int64, reason string) {
	reject := func(msg string) {
		rejected++
		if reason == "" {
			reason = msg
		}
	}
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := map[string]*commonpb.AnyValue{}
				for _, kv := range lr.GetAttributes() {
					attrs[kv.GetKey()] = kv.GetValue()
				}
				model := stringValue(attrs[attrRequestModel])
				in, inOK := intValue(attrs[attrInputTokens])
				out, outOK := intValue(attrs[attrOutputTokens])
				cost := decimalValue(attrs[attrCostTotal])
				if model == "" || (!inOK && !outOK && cost == "") {
					continue // not a charged LLM request
				}
				agent := stringValue(attrs[gateway.LogFieldAgent])
				if agent == "" {
					continue // unauthenticated: agentgateway rejected it before the model
				}
				e := Entry{
					RequestID:    stringValue(attrs[gateway.LogFieldRequest]),
					KeyID:        stringValue(attrs[gateway.LogFieldKey]),
					Model:        model,
					InputTokens:  in,
					OutputTokens: out,
					CostUSD:      cost,
					Time:         recordTime(lr.GetTimeUnixNano(), lr.GetObservedTimeUnixNano()),
				}
				org, name, ok := strings.Cut(agent, "/")
				e.Agent = gateway.AgentRef{Org: org, Name: name}
				switch {
				case !ok || e.Agent.Validate() != nil:
					reject(fmt.Sprintf("invalid %s %q", gateway.LogFieldAgent, agent))
					continue
				case e.RequestID == "" || len(e.RequestID) > 128:
					reject("missing or invalid " + gateway.LogFieldRequest)
					continue
				case e.KeyID == "":
					reject("missing " + gateway.LogFieldKey)
					continue
				case in < 0 || out < 0:
					reject("negative token counts")
					continue
				case attrs[attrCostTotal] != nil && cost == "":
					reject(fmt.Sprintf("invalid %s", attrCostTotal))
					continue
				}
				entries = append(entries, e)
			}
		}
	}
	return entries, rejected, reason
}

func recordTime(t, observed uint64) time.Time {
	for _, v := range []uint64{t, observed} {
		if v > 0 && v <= math.MaxInt64 {
			return time.Unix(0, int64(v)).UTC()
		}
	}
	return time.Now().UTC()
}

func stringValue(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	return v.GetStringValue()
}

func intValue(v *commonpb.AnyValue) (int64, bool) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_IntValue:
		return x.IntValue, true
	case *commonpb.AnyValue_DoubleValue:
		if x.DoubleValue == math.Trunc(x.DoubleValue) && math.Abs(x.DoubleValue) < 1<<53 {
			return int64(x.DoubleValue), true
		}
	case *commonpb.AnyValue_StringValue:
		n, err := strconv.ParseInt(x.StringValue, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// decimalValue returns the cost as a plain non-negative decimal string, or ""
// when it is absent or not one.
func decimalValue(v *commonpb.AnyValue) string {
	var s string
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		s = x.StringValue
	case *commonpb.AnyValue_DoubleValue:
		if x.DoubleValue < 0 || math.IsInf(x.DoubleValue, 0) || math.IsNaN(x.DoubleValue) {
			return ""
		}
		s = strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonpb.AnyValue_IntValue:
		s = strconv.FormatInt(x.IntValue, 10)
	}
	if !decimalRE.MatchString(s) {
		return ""
	}
	return s
}

// Handler serves OTLP/HTTP log exports (protobuf, optionally gzip) at
// LogsPath and records them through in. agentgateway exports in protobuf only
// (opentelemetry-otlp with http-proto), so JSON is refused with 415.
//
// Responses follow the OTLP/HTTP spec: 200 with partial_success for records
// that could not be attributed (retrying would not help), 400 for an
// undecodable body, and 503 when the database is unavailable, which the
// exporter may retry; replays are recorded once.
func Handler(in *Ingester, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+LogsPath, func(w http.ResponseWriter, r *http.Request) {
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct != "application/x-protobuf" {
			http.Error(w, "only application/x-protobuf is supported", http.StatusUnsupportedMediaType)
			return
		}
		body, err := readBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid ExportLogsServiceRequest", http.StatusBadRequest)
			return
		}
		entries, rejected, reason := Decode(&req)
		res, err := in.Ingest(r.Context(), entries)
		if err != nil {
			log.Error("spend ledger: ingest failed", "entries", len(entries), "err", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "ingest failed", http.StatusServiceUnavailable)
			return
		}
		rejected += int64(res.Unattributed)
		if reason == "" && res.Unattributed > 0 {
			reason = "unknown org or agent"
		}
		if rejected > 0 {
			log.Warn("spend ledger: rejected access-log records", "rejected", rejected, "reason", reason)
		}
		resp := &collogspb.ExportLogsServiceResponse{}
		if rejected > 0 {
			resp.PartialSuccess = &collogspb.ExportLogsPartialSuccess{RejectedLogRecords: rejected, ErrorMessage: reason}
		}
		out, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	})
	return mux
}

func readBody(r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(nil, r.Body, maxBody)
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, fmt.Errorf("invalid gzip body: %w", err)
		}
		defer func() { _ = zr.Close() }()
		src = io.LimitReader(zr, maxBody+1)
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
	b, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxBody {
		return nil, errors.New("body too large")
	}
	return b, nil
}

// Serve runs the OTLP receiver on addr until ctx ends.
func Serve(ctx context.Context, addr string, in *Ingester, log *slog.Logger) error {
	srv := &http.Server{Addr: addr, Handler: Handler(in, log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
