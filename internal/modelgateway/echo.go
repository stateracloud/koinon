// SPDX-License-Identifier: Apache-2.0

package modelgateway

import (
	"bytes"
	"io"
)

// minEchoKey is the shortest provider key the echo watch looks for. A shorter value would match ordinary
// text and make the alert noise.
const minEchoKey = 16

// echoWatch finds the provider key in a response body that streams past it (#1332, ADR 0097 section 15).
// It never changes and never holds back a byte: it keeps only the last len(key)-1 bytes, so a key that is
// split across two reads is still found. Memory is bounded by the key length, not by the body.
type echoWatch struct {
	key     []byte
	tail    []byte // at most len(key)-1 bytes: the end of the bytes seen so far
	scratch []byte // tail + the start of the next read, reused
	seen    bool
}

// newEchoWatch returns nil for a key too short to watch; a nil watch sees nothing.
func newEchoWatch(key string) *echoWatch {
	if len(key) < minEchoKey {
		return nil
	}
	k := len(key) - 1
	return &echoWatch{key: []byte(key), tail: make([]byte, 0, k), scratch: make([]byte, 0, 2*k)}
}

// feed looks at the next bytes of the body. It reports true once, at the first read that completes the key.
func (e *echoWatch) feed(p []byte) bool {
	if e == nil || e.seen || len(p) == 0 {
		return false
	}
	k := len(e.key) - 1
	if len(e.tail) > 0 {
		head := p[:min(len(p), k)]
		e.scratch = append(append(e.scratch[:0], e.tail...), head...)
		e.seen = bytes.Contains(e.scratch, e.key)
	}
	e.seen = e.seen || bytes.Contains(p, e.key)
	if len(p) >= k {
		e.tail = append(e.tail[:0], p[len(p)-k:]...)
	} else {
		e.tail = append(e.tail, p...)
		if over := len(e.tail) - k; over > 0 {
			e.tail = e.tail[:copy(e.tail, e.tail[over:])]
		}
	}
	return e.seen
}

// echoBody passes a response body through unchanged and calls seen once when the watch finds the key.
type echoBody struct {
	io.ReadCloser
	watch *echoWatch
	seen  func()
}

func (b *echoBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.watch.feed(p[:n]) {
		b.seen()
	}
	return n, err
}
