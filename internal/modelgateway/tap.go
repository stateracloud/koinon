// SPDX-License-Identifier: Apache-2.0

package modelgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/stateracloud/koinon/internal/modelformat"
)

// The usage tap reads a response as it passes and keeps no body: an SSE parser that holds one line at
// a time, and for a response that is not a stream a scanner that holds the one top-level "usage"
// object. Neither buffers a response, so a long stream costs the gateway the memory of its longest
// line.

// Reasons a record is marked failed (UsageRecord.Error). They are fixed strings, never provider text.
const (
	failUpstream = "Upstream" // the upstream cut the stream, or ended it with an error event
	failCanceled = "Canceled" // the client left before the response ended
)

// maxLine bounds one SSE line the tap holds. A longer line is skipped, not parsed: it cannot be a
// usage or error event, which are a few hundred bytes.
const maxLine = 1 << 20

// maxUsageObject bounds the "usage" object the JSON scanner keeps.
const maxUsageObject = 64 << 10

// counts is the usage as the provider reported it, in the record's disjoint buckets.
type counts struct {
	in, out, cacheRead, cacheWrite, reasoning int64
	rawOut                                    int64 // the output as reported, thinking included
	inKnown, outKnown, reasoningKnown         bool
}

// wireUsage is the union of the two formats' usage objects. A nil field was not reported.
type wireUsage struct {
	// Chat completions: totals, with the parts in details.
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// Messages: input excludes the cache, output includes the thinking.
	InputTokens   *int64 `json:"input_tokens"`
	OutputTokens  *int64 `json:"output_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	OutputDetails *struct {
		ThinkingTokens *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// merge folds w into c. A later event carries the cumulative value of what it reports and nothing
// for what it does not, so a field present replaces and a field absent stays.
func (c *counts) merge(f modelformat.Format, w *wireUsage) {
	if w == nil {
		return
	}
	switch f {
	case modelformat.Anthropic:
		if w.InputTokens != nil {
			c.in, c.inKnown = nonNeg(*w.InputTokens), true
		}
		if w.CacheRead != nil {
			c.cacheRead = nonNeg(*w.CacheRead)
		}
		if w.CacheCreation != nil {
			c.cacheWrite = nonNeg(*w.CacheCreation)
		}
		if w.OutputTokens != nil {
			c.rawOut, c.outKnown = nonNeg(*w.OutputTokens), true
			if w.OutputDetails != nil && w.OutputDetails.ThinkingTokens != nil {
				c.reasoning, c.reasoningKnown = nonNeg(*w.OutputDetails.ThinkingTokens), true
			}
		}
	default:
		if w.PromptTokens != nil {
			total := nonNeg(*w.PromptTokens)
			if w.PromptDetails != nil && w.PromptDetails.CachedTokens != nil {
				c.cacheRead = min(nonNeg(*w.PromptDetails.CachedTokens), total)
			}
			c.in, c.inKnown = total-c.cacheRead, true
		}
		if w.CompletionTokens != nil {
			total := nonNeg(*w.CompletionTokens)
			if w.CompletionDetails != nil && w.CompletionDetails.ReasoningTokens != nil {
				c.reasoning, c.reasoningKnown = min(nonNeg(*w.CompletionDetails.ReasoningTokens), total), true
			}
			c.rawOut, c.outKnown = total, true
		}
	}
	// The thinking is part of the output the provider reported; the record's buckets are disjoint.
	// Computed from the raw value each time, so a later event that reports no output cannot subtract twice.
	if c.outKnown {
		c.out = c.rawOut
		if c.reasoningKnown {
			c.out = max(c.rawOut-c.reasoning, 0)
		}
	}
}

func nonNeg(n int64) int64 { return max(n, 0) }

// tap is what a response body is read through. feed gets every chunk read; end is told once how the
// body ended.
type tap interface {
	feed(p []byte)
	// result returns the counts read and whether the response ended the way its format ends it: a
	// stream with its terminator and no error event; a JSON body that was read to its end.
	result() (c counts, complete bool)
}

// sseTap reads a server-sent event stream of one format.
type sseTap struct {
	format modelformat.Format

	line     []byte
	overflow bool
	event    string // the last "event:" field
	c        counts
	done     bool // the terminator was seen: [DONE], or Messages' message_stop
	errEvent bool // an error event was seen
}

func newSSETap(f modelformat.Format) *sseTap { return &sseTap{format: f} }

func (t *sseTap) feed(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.hold(p)
			return
		}
		t.hold(p[:i])
		t.endLine()
		p = p[i+1:]
	}
}

func (t *sseTap) hold(p []byte) {
	if t.overflow {
		return
	}
	if len(t.line)+len(p) > maxLine {
		t.overflow, t.line = true, t.line[:0]
		return
	}
	t.line = append(t.line, p...)
}

func (t *sseTap) endLine() {
	line, overflow := bytes.TrimSuffix(t.line, []byte("\r")), t.overflow
	t.overflow = false
	defer func() { t.line = t.line[:0] }()
	if overflow {
		return
	}
	if len(line) == 0 {
		t.event = "" // a blank line ends the event
		return
	}
	name, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(name) {
	case "event":
		t.event = string(value)
		if t.event == "error" {
			t.errEvent = true
		}
	case "data":
		t.data(value)
	}
}

// streamEvent is the part of an event the tap reads. Error is present on an error event of both
// formats and on an aggregator's error line.
type streamEvent struct {
	Type    string                      `json:"type"`
	Message *struct{ Usage *wireUsage } `json:"message"`
	Usage   *wireUsage                  `json:"usage"`
	Error   json.RawMessage             `json:"error"`
}

func (t *sseTap) data(v []byte) {
	if string(v) == "[DONE]" {
		t.done = true
		return
	}
	// Most events are text deltas: only an event that mentions usage or an error, or Messages' last
	// one, is parsed.
	if !bytes.Contains(v, []byte(`"usage"`)) && !bytes.Contains(v, []byte(`"error"`)) && !bytes.Contains(v, []byte("message_stop")) {
		return
	}
	var ev streamEvent
	if json.Unmarshal(v, &ev) != nil {
		return
	}
	if len(ev.Error) > 0 && string(ev.Error) != "null" {
		t.errEvent = true
	}
	if ev.Type == "error" {
		t.errEvent = true
	}
	if ev.Type == "message_stop" {
		t.done = true
	}
	if ev.Message != nil {
		t.c.merge(t.format, ev.Message.Usage)
	}
	if ev.Usage != nil {
		t.c.merge(t.format, ev.Usage)
		// A chat completions stream that carried its usage ended, whether or not the provider sent
		// [DONE] after it.
		if t.format != modelformat.Anthropic {
			t.done = true
		}
	}
}

func (t *sseTap) result() (counts, bool) { return t.c, t.done && !t.errEvent }

// jsonTap reads the top-level "usage" object of a JSON body.
type jsonTap struct {
	format modelformat.Format

	depth     int
	inStr     bool
	esc       bool
	isKey     bool // the string being read is a key at depth 1
	key       []byte
	lastKey   string
	expectKey bool
	capturing bool
	usage     []byte
	tooLong   bool
	c         counts
	sawStart  bool
}

func newJSONTap(f modelformat.Format) *jsonTap { return &jsonTap{format: f} }

func (t *jsonTap) feed(p []byte) {
	for _, ch := range p {
		if t.capturing {
			if len(t.usage) < maxUsageObject {
				t.usage = append(t.usage, ch)
			} else {
				t.tooLong = true
			}
		}
		if t.inStr {
			switch {
			case t.esc:
				t.esc = false
			case ch == '\\':
				t.esc = true
			case ch == '"':
				t.inStr = false
				if t.isKey {
					t.lastKey, t.isKey = string(t.key), false
				}
			case t.isKey && len(t.key) < 64:
				t.key = append(t.key, ch)
			}
			continue
		}
		switch ch {
		case '"':
			t.inStr = true
			if t.depth == 1 && t.expectKey {
				t.isKey, t.expectKey, t.key = true, false, t.key[:0]
			}
		case '{', '[':
			t.depth++
			t.sawStart = true
			switch {
			case t.depth == 1:
				t.expectKey = ch == '{'
			case t.depth == 2 && ch == '{' && t.lastKey == "usage" && !t.capturing && t.usage == nil:
				t.capturing, t.usage = true, append(make([]byte, 0, 256), '{')
			}
		case '}', ']':
			t.depth--
			if t.capturing && t.depth == 1 {
				t.capturing = false
				t.parse()
			}
		case ',':
			if t.depth == 1 {
				t.expectKey = true
			}
		}
	}
}

func (t *jsonTap) parse() {
	if t.tooLong {
		return
	}
	var w wireUsage
	if json.Unmarshal(t.usage, &w) == nil {
		t.c.merge(t.format, &w)
	}
}

// result reports a body complete when it was a whole top-level value.
func (t *jsonTap) result() (counts, bool) { return t.c, t.sawStart && t.depth == 0 && !t.inStr }

// tapBody is a response body that reports to its tap and tells finish how the response ended, once.
type tapBody struct {
	rc     io.ReadCloser
	tap    tap
	finish func(c counts, complete bool, readErr error, bytes int64)

	once sync.Once
	n    int64
}

func (b *tapBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.n += int64(n)
		b.tap.feed(p[:n])
	}
	if err != nil {
		b.end(err)
	}
	return n, err
}

// Close ends the response too: a body closed before its end is a client that left.
func (b *tapBody) Close() error {
	err := b.rc.Close()
	b.end(errClosedEarly)
	return err
}

var errClosedEarly = errors.New("modelgateway: response closed before its end")

func (b *tapBody) end(err error) {
	b.once.Do(func() {
		c, complete := b.tap.result()
		if errors.Is(err, io.EOF) {
			err = nil
		}
		b.finish(c, complete, err, b.n)
	})
}
