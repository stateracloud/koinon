// SPDX-License-Identifier: Apache-2.0

package agentgateway

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
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/safego"
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

// gatewayStatuses are the statuses agentgateway sets itself, before it calls the provider (bad or
// missing key, model not allowed or unknown, request too large, its own budget or rate limit). A
// record from a config that does not log koinon.upstream cannot say who answered, so such a record
// with one of these statuses and no usage is taken as the gateway's and never charged.
var gatewayStatuses = map[int64]bool{401: true, 403: true, 404: true, 405: true, 413: true, 415: true, 429: true}

func isZeroDecimal(s string) bool { return strings.Trim(s, "0.") == "" }

// LogsPath is the OTLP/HTTP logs path.
const LogsPath = "/v1/logs"

// maxBody bounds an export request. agentgateway's batch processor sends at
// most 512 records per export, a few KiB each.
const maxBody = 16 << 20

// Decode maps the charged LLM requests of an OTLP logs export to usage records. Records
// that are not charged requests (MCP calls, rejected requests, requests
// without usage) are skipped. Records that look charged but cannot be
// attributed are counted in rejected, with the first reason in reason.
func Decode(req *collogspb.ExportLogsServiceRequest) (entries []gateway.UsageRecord, rejected int64, reason string) {
	return decode(req, nil)
}

// decode is Decode that also reports each rejection's kind (one of the ledger.Reject* constants) to
// onReject, for the metrics.
func decode(req *collogspb.ExportLogsServiceRequest, onReject func(kind string)) (entries []gateway.UsageRecord, rejected int64, reason string) {
	reject := func(kind, msg string) {
		rejected++
		if reason == "" {
			reason = msg
		}
		if onReject != nil {
			onReject(kind)
		}
	}
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := map[string]*commonpb.AnyValue{}
				for _, kv := range lr.GetAttributes() {
					attrs[kv.GetKey()] = kv.GetValue()
				}
				if stringValue(attrs[LogFieldKey]) == gateway.ReloadProbeKeyName {
					continue // the gateway driver's reload probe, never charged or run-attributed
				}
				model := stringValue(attrs[attrRequestModel])
				// Usage in agentgateway's consistent semantics when the driver logged it
				// (koinon.*), else the gen_ai attributes (older configs and fixtures).
				in, inOK := intValue(attrs[LogFieldInput])
				if !inOK {
					in, inOK = intValue(attrs[attrInputTokens])
				}
				out, outOK := intValue(attrs[LogFieldOutput])
				if !outOK {
					out, outOK = intValue(attrs[attrOutputTokens])
				}
				cacheRead, _ := intValue(attrs[LogFieldCacheRead])
				cacheWrite, _ := intValue(attrs[LogFieldCacheWrite])
				// The split is absent, not 0, when the gateway had none to log (agentgateway v1.7.0
				// on Anthropic Messages, #78): the entry then records it as unknown. Interim until
				// agentgateway sets llm.reasoningTokens from Messages usage; #78 links the upstream
				// issue, and the exit condition is that field logged on a Messages call.
				reasoning, reasoningOK := intValue(attrs[LogFieldReasoning])
				// agentgateway reports totals (input includes cache reads and writes, output includes
				// reasoning); the usage record carries disjoint buckets (ADR 0097 decision 3), so the
				// parts leave the totals here, in the driver. Counts that do not add up clamp at zero.
				// A negative total is left alone so ledger.CheckRecord still rejects it.
				if inOK && in >= 0 {
					in = max(in-max(cacheRead, 0)-max(cacheWrite, 0), 0)
				}
				if outOK && reasoningOK && out >= 0 {
					out = max(out-max(reasoning, 0), 0)
				}
				status, statusOK := intValue(attrs[LogFieldStatus])
				cost := decimalValue(attrs[attrCostTotal])
				// Usage or a non-zero cost is charged whatever the status. agentgateway logs a zero
				// cost for requests it did not price, so a zero cost is not usage.
				usage := inOK || outOK || (cost != "" && !isZeroDecimal(cost))
				reqBytes, hasReqBytes := intValue(attrs[LogFieldRequestBytes])
				if model == "" {
					continue // not an LLM request (MCP, or refused before the model)
				}
				upstreamKnown := attrs[LogFieldUpstream] != nil
				upstream := boolValue(attrs[LogFieldUpstream])
				if !usage {
					switch {
					case statusOK && upstreamKnown && !upstream:
						continue // answered by the gateway itself (a refused key, its own budget): never reached the provider
					case statusOK && !upstreamKnown && gatewayStatuses[status]:
						continue // a config that cannot say who answered: these statuses are the gateway's
					case statusOK, hasReqBytes:
						// A 2xx without usage (an aborted stream, a provider that reports none), any
						// other non-2xx, or no status at all (the client disconnected before the
						// headers): the provider may have done and billed the work, so the usage
						// is estimated, marked usage_missing and flagged. The status test alone
						// cannot see a record without a status, so records of this driver's
						// config (which always log the request bytes) count as such.
					default:
						continue // an older config's record without usage: not a charged request
					}
				}
				agent := stringValue(attrs[LogFieldAgent])
				if agent == "" {
					continue // unauthenticated: agentgateway rejected it before the model
				}
				e := gateway.UsageRecord{
					RequestID:    stringValue(attrs[LogFieldRequest]),
					KeyID:        stringValue(attrs[LogFieldKey]),
					Model:        model,
					InputTokens:  in,
					OutputTokens: out,
					InputKnown:   inOK, OutputKnown: outOK,
					CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
					ReasoningTokens: reasoning, ReasoningKnown: reasoningOK,
					CostUSD: cost, Status: status,
					UpstreamKnown: upstreamKnown, Upstream: upstream,
					RequestBytes: -1, ResponseBytes: -1,
					Time: recordTime(lr.GetTimeUnixNano(), lr.GetObservedTimeUnixNano()),
				}
				e.MaxTokens, _ = intValue(attrs[LogFieldMaxTokens])
				if hasReqBytes && reqBytes >= 0 {
					e.RequestBytes = reqBytes
				}
				if id := stringValue(attrs[LogFieldResponseID]); len(id) <= 256 {
					e.ResponseID = id
				}
				e.Format = modelformat.OpenAI
				if f := stringValue(attrs[LogFieldFormat]); f != "" {
					e.Format = modelformat.Format(f)
				}
				e.Stream = boolValue(attrs[LogFieldStream])
				e.Error = stringValue(attrs[LogFieldError])
				org, name, ok := strings.Cut(agent, "/")
				e.Agent = gateway.AgentRef{Org: org, Name: name}
				if !ok || e.Agent.Validate() != nil {
					reject(ledger.RejectInvalidAgent, fmt.Sprintf("invalid %s %q", LogFieldAgent, agent))
					continue
				}
				if kind, msg := ledger.CheckRecord(e); kind != "" {
					reject(kind, msg)
					continue
				}
				if attrs[attrCostTotal] != nil && cost == "" {
					reject(ledger.RejectInvalidCost, fmt.Sprintf("invalid %s", attrCostTotal))
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

// boolValue reads a boolean attribute, or the strings "true" and "false" (CEL may log either).
func boolValue(v *commonpb.AnyValue) bool {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_StringValue:
		return x.StringValue == "true"
	}
	return false
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
	if !ledger.ValidGatewayCost(s) {
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
func Handler(in *ledger.Ingester, log *slog.Logger) http.Handler {
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
		entries, rejected, reason := decode(&req, in.Metrics.Reject)
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

// Serve runs the OTLP receiver on addr until ctx ends. Each wrap wraps the whole handler, the
// first one outermost; koinon-api passes the bearer-token check (internal/otlpauth), which must
// cover every path of the listener.
func Serve(ctx context.Context, addr string, in *ledger.Ingester, log *slog.Logger, wrap ...func(http.Handler) http.Handler) error {
	if err := in.Validate(); err != nil {
		return err
	}
	h := Handler(in, log)
	for i := len(wrap) - 1; i >= 0; i-- {
		h = wrap[i](h)
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	safego.Go(ctx, log, "otlp-shutdown", func(context.Context) {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
