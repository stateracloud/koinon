// SPDX-License-Identifier: Apache-2.0

// Package runevidence exports what a recorded run left behind (issue #926): the run and its
// sandbox's network enforcement mode, the traces (one row per turn), the run's events, every
// decision of the MCP leak guard and the bytes that left for each external sink, as JSON Lines
// under one prefix of an object store. Credentials are scrubbed from event payloads with the trace
// router's scrubber (internal/tracing/scrub) and host names are replaced by stable pseudonyms
// (HostMasker). The decision records never held an argument or a matched text.
//
// Corpus turns an export into a replay corpus for the leak guard's measurement (#51).
package runevidence

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// The objects of one export, under its prefix.
const (
	FileRun       = "run.jsonl"
	FileTraces    = "traces.jsonl"
	FileEvents    = "run_events.jsonl"
	FileDecisions = "decisions.jsonl"
	FileOutbound  = "outbound.jsonl"
)

// Files lists the objects, in the order Export writes them.
var Files = []string{FileRun, FileTraces, FileEvents, FileDecisions, FileOutbound}

// Sink receives the objects of an export. Put replaces an object of the same name.
type Sink interface {
	Put(ctx context.Context, name string, data []byte) error
}

// Exporter reads one run's evidence.
type Exporter struct {
	Q *store.Queries
	// Scrubber cleans event payloads and free text. Required: an export never leaves unscrubbed.
	Scrubber *scrub.Scrubber
	// Hosts replaces host names. Required, for the same reason.
	Hosts *HostMasker
	// PageSize bounds one read of run events (default 500).
	PageSize int32
}

// Summary is what an export wrote.
type Summary struct {
	Prefix                             string
	Turns, Events, Decisions, Outbound int
	Enforcement                        string
	Objects                            []string
}

// Export writes the evidence of one run to sink.
func (e *Exporter) Export(ctx context.Context, org, runID pgtype.UUID, sink Sink) (Summary, error) {
	if e.Scrubber == nil || e.Hosts == nil {
		return Summary{}, errors.New("runevidence: an exporter needs a scrubber and a host masker")
	}
	run, err := e.Q.GetRunEvidenceRun(ctx, store.GetRunEvidenceRunParams{OrgID: org, RunID: runID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, fmt.Errorf("runevidence: no run %s in this org", uuidString(runID))
	}
	if err != nil {
		return Summary{}, err
	}
	var sum Summary
	bufs := map[string]*bytes.Buffer{}
	for _, f := range Files {
		bufs[f] = &bytes.Buffer{}
	}

	// run.jsonl: the run and its sandbox.
	sb, err := e.Q.GetRunEvidenceSandbox(ctx, store.GetRunEvidenceSandboxParams{OrgID: org, AgentID: run.AgentID})
	haveSandbox := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return sum, err
	}
	sandbox := map[string]any{"enforcement": "unknown"}
	sum.Enforcement = "unknown"
	if haveSandbox {
		sum.Enforcement = sb.Enforcement
		sandbox = map[string]any{"enforcement": sb.Enforcement, "home_generation": sb.HomeGeneration.String, "state": string(sb.State)}
	}
	runLine := map[string]any{
		"type": "run", "run_id": uuidString(run.ID), "agent_id": uuidString(run.AgentID), "task_id": uuidString(run.TaskID),
		"state": string(run.State), "started_at": ts(run.StartedAt), "finished_at": ts(run.FinishedAt),
		"outcome_reason": e.text(run.OutcomeReason.String), "sandbox": sandbox,
	}
	if err := line(bufs[FileRun], runLine); err != nil {
		return sum, err
	}

	// traces.jsonl: one row per turn, one trace each (ADR 0074).
	turns, err := e.Q.ListRunEvidenceTurns(ctx, store.ListRunEvidenceTurnsParams{OrgID: org, RunID: runID})
	if err != nil {
		return sum, err
	}
	for _, t := range turns {
		if err := line(bufs[FileTraces], map[string]any{
			"type": "trace", "run_id": uuidString(run.ID), "seq": t.Seq, "trace_id": hex.EncodeToString(t.TraceID),
			"started_at": ts(t.StartedAt), "ended_at": ts(t.EndedAt), "end_reason": e.text(t.EndReason.String),
		}); err != nil {
			return sum, err
		}
		sum.Turns++
	}

	// run_events.jsonl: in write order, payloads scrubbed.
	page := e.PageSize
	if page <= 0 {
		page = 500
	}
	var after int64
	for {
		evs, err := e.Q.ListRunEvidenceEvents(ctx, store.ListRunEvidenceEventsParams{OrgID: org, RunID: runID, AfterSeq: after, PageSize: page})
		if err != nil {
			return sum, err
		}
		for _, ev := range evs {
			if err := line(bufs[FileEvents], map[string]any{
				"type": "run_event", "run_id": uuidString(run.ID), "seq": ev.Seq, "epoch": ev.Epoch, "kind": ev.Kind,
				"created_at": ts(ev.CreatedAt), "payload": json.RawMessage(e.json(ev.Payload)),
			}); err != nil {
				return sum, err
			}
			after = ev.Seq
			sum.Events++
		}
		if len(evs) < int(page) {
			break
		}
	}

	// decisions.jsonl: every decision of the leak guard for the run, passes included.
	decs, err := e.Q.ListRunEvidenceDecisions(ctx, store.ListRunEvidenceDecisionsParams{OrgID: org, RunID: runID})
	if err != nil {
		return sum, err
	}
	generations := map[string]bool{}
	for i, d := range decs {
		generations[d.Generation] = true
		l := map[string]any{
			"type": "decision", "run_id": uuidString(run.ID), "n": i + 1, "at": ts(d.CreatedAt), "generation": d.Generation,
			"server": e.Hosts.Mask(d.Server), "tool": e.Hosts.Mask(d.Tool), "sink_class": d.SinkClass, "action": d.Action,
			"reason": e.Hosts.Mask(d.Reason), "reasons": e.maskAll(d.Reasons), "match_count": d.MatchCount, "match_length": d.MatchLength, "match_encoding": d.MatchEncoding,
			"payload_bytes": d.PayloadBytes,
		}
		if d.UntrustedInput.Valid {
			l["untrusted_input"], l["private_data"] = d.UntrustedInput.Bool, d.PrivateData.Bool
		}
		if err := line(bufs[FileDecisions], l); err != nil {
			return sum, err
		}
		sum.Decisions++
	}

	// outbound.jsonl: the bytes per external sink, for the run and for each home generation it
	// touched.
	out, err := e.Q.SumRunEvidenceOutbound(ctx, store.SumRunEvidenceOutboundParams{OrgID: org, RunID: runID})
	if err != nil {
		return sum, err
	}
	for _, o := range out {
		if err := line(bufs[FileOutbound], map[string]any{
			"type": "outbound", "scope": "run", "run_id": uuidString(run.ID), "generation": o.Generation, "server": e.Hosts.Mask(o.Server),
			"sink_class": o.SinkClass, "calls": o.Calls, "payload_bytes": o.PayloadBytes,
		}); err != nil {
			return sum, err
		}
		sum.Outbound++
	}
	// In a fixed order, so two exports of one run are the same bytes.
	for _, gen := range slices.Sorted(maps.Keys(generations)) {
		if gen == "" {
			continue
		}
		rows, err := e.Q.SumGenerationEvidenceOutbound(ctx, store.SumGenerationEvidenceOutboundParams{OrgID: org, AgentID: run.AgentID, Generation: gen})
		if err != nil {
			return sum, err
		}
		for _, o := range rows {
			if err := line(bufs[FileOutbound], map[string]any{
				"type": "outbound", "scope": "generation", "agent_id": uuidString(run.AgentID), "generation": gen, "server": e.Hosts.Mask(o.Server),
				"sink_class": o.SinkClass, "calls": o.Calls, "payload_bytes": o.PayloadBytes,
			}); err != nil {
				return sum, err
			}
			sum.Outbound++
		}
	}

	for _, f := range Files {
		if err := sink.Put(ctx, f, bufs[f].Bytes()); err != nil {
			return sum, fmt.Errorf("runevidence: writing %s: %w", f, err)
		}
		sum.Objects = append(sum.Objects, f)
	}
	return sum, nil
}

// text scrubs a free-text field.
// NewScrubber is the scrubber an export uses: the trace router's redaction level with the org's
// known secrets, which the entropy and pattern rules alone would not catch (a password, a short key).
// The command and its specs build it here, so what a spec proves is what the command runs.
func NewScrubber(secrets *scrub.Secrets) *scrub.Scrubber {
	return scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: secrets})
}

func (e *Exporter) maskAll(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = e.Hosts.Mask(v)
	}
	return out
}

func (e *Exporter) text(s string) string { return e.Hosts.Mask(e.Scrubber.Text(s)) }

// json scrubs a JSON payload. A payload that is not one JSON value is exported as a scrubbed string.
func (e *Exporter) json(raw []byte) []byte {
	clean, err := e.Scrubber.JSON(raw)
	if err != nil {
		clean, _ = json.Marshal(e.Scrubber.Text(string(raw)))
	}
	return []byte(e.Hosts.Mask(string(clean)))
}

func line(b *bytes.Buffer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.Write(raw)
	b.WriteByte('\n')
	return nil
}

func ts(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.UTC().Format(time.RFC3339Nano)
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
