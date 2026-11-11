// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
)

// This file is the agentgateway side of [Gateway.Usage]: the one place in the suite that knows
// agentgateway's access log. The specs read gateway.UsageRecord and never a log field.

// unloggedModel stands in for gen_ai.request.model when an access-log line does not carry it (the
// Kubernetes proxy logs only the koinon.* fields). agentgateway.Decode needs a model to tell an LLM
// request from an MCP call; the specs do not read the model back.
const unloggedModel = "unlogged"

// UsageFromAgentgatewayLog reads the usage records of one key (Key.ID) from agentgateway's access
// log, oldest first. It is the default of [Gateway.Usage]. The log line goes through the same
// decoder as the OTLP export the spend ledger ingests (agentgateway.Decode), so the suite judges
// the record the ledger would read, with the same mapping of totals to disjoint buckets. A line
// the decoder rejects is an error: the ledger would not charge it.
func UsageFromAgentgatewayLog(log, keyID string) ([]gateway.UsageRecord, error) {
	var out []gateway.UsageRecord
	for _, rec := range accessLog(log, keyID) {
		entries, rejected, reason := agentgateway.Decode(otlpExport(rec))
		if rejected > 0 {
			return nil, fmt.Errorf("the usage decoder rejects the access-log record of key %s: %s", keyID, reason)
		}
		out = append(out, entries...)
	}
	return out, nil
}

// otlpExport wraps one access-log record as the OTLP export agentgateway would push.
func otlpExport(rec map[string]any) *collogspb.ExportLogsServiceRequest {
	if _, ok := rec["gen_ai.request.model"]; !ok {
		rec["gen_ai.request.model"] = unloggedModel
	}
	lr := &logspb.LogRecord{}
	for k, v := range rec {
		var val *commonpb.AnyValue
		switch x := v.(type) {
		case string:
			val = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: x}}
		case bool:
			val = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: x}}
		case float64:
			if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
				val = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(x)}}
			} else {
				val = &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: x}}
			}
		default:
			continue // a null or a nested value: no attribute, as when agentgateway logs none
		}
		lr.Attributes = append(lr.Attributes, &commonpb.KeyValue{Key: k, Value: val})
	}
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{lr}}},
	}}}
}

// logfmtField matches a koinon.* field, the model or the cost of a logfmt access-log line.
var logfmtField = regexp.MustCompile(`\b(koinon\.[a-z_]+|gen_ai\.request\.model|agw\.ai\.usage\.cost\.total)=("[^"]*"|\S+)`)

// accessLog returns the access-log records of the gateway log that carry the key ID. Standalone
// agentgateway logs JSON lines (config.logging.format json); the Kubernetes proxy logs logfmt
// (a JSON number reads as float64 in both).
func accessLog(log, keyID string) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, keyID) {
			continue
		}
		rec := map[string]any{}
		if strings.HasPrefix(line, "{") {
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
		} else {
			for _, m := range logfmtField.FindAllStringSubmatch(line, -1) {
				if n, err := strconv.ParseFloat(m[2], 64); err == nil {
					rec[m[1]] = n
				} else if b, err := strconv.ParseBool(m[2]); err == nil {
					rec[m[1]] = b
				} else {
					rec[m[1]] = strings.Trim(m[2], `"`)
				}
			}
		}
		if rec[agentgateway.LogFieldKey] == keyID {
			out = append(out, rec)
		}
	}
	return out
}
