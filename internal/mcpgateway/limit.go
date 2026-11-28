// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// DefaultMaxResponseBytes is the most one upstream MCP reply may carry when Options.MaxResponseBytes is
// zero. The pod limit is shared by every org's calls: without a bound, one org's server could make both
// copies run out of memory (issue 1347). The Go SDK client reads a JSON reply with io.ReadAll and no
// limit, and bounds only one SSE event (16 MiB); this bound covers both. A tool result is text for a
// model, so 4 MiB is far above any useful size.
const DefaultMaxResponseBytes = 4 << 20

// ErrResponseTooLarge is the error of a reply that went over the limit. The call fails with it.
var ErrResponseTooLarge = errors.New("mcpgateway: the upstream reply is larger than the gateway allows")

// limitTransport bounds each upstream reply. A reply of content type application/json (the stateless
// reply of a tool call) is bounded as a whole. A text/event-stream reply is a stream that may run for the
// whole call, so only each event is bounded, by the SDK's MaxEventSize, which connect sets to the same limit.
type limitTransport struct {
	base http.RoundTripper
	max  int64
	// onLimit runs once per reply that went over (it logs). It may be nil.
	onLimit func()
}

func (t *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || t.max <= 0 {
		return resp, err
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "text/event-stream" {
		return resp, nil
	}
	if resp.ContentLength > t.max {
		_ = resp.Body.Close()
		t.over()
		return nil, fmt.Errorf("%w (%d bytes, limit %d)", ErrResponseTooLarge, resp.ContentLength, t.max)
	}
	resp.Body = &limitedBody{rc: resp.Body, left: t.max, over: t.over, max: t.max}
	return resp, nil
}

func (t *limitTransport) over() {
	if t.onLimit != nil {
		t.onLimit()
	}
}

// limitedBody reads at most max bytes and then fails with [ErrResponseTooLarge].
type limitedBody struct {
	rc   io.ReadCloser
	left int64
	max  int64
	over func()
	done bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, ErrResponseTooLarge
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1] // read one byte past the limit to tell "exactly max" from "over"
	}
	n, err := b.rc.Read(p)
	if int64(n) > b.left {
		b.done = true
		b.over()
		return int(b.left), fmt.Errorf("%w (limit %d bytes)", ErrResponseTooLarge, b.max)
	}
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
