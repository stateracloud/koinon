// SPDX-License-Identifier: Apache-2.0

package runoutcome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Items, not chunks (old issue 344). A harness streams a message or a thought as hundreds of small ACP
// chunks (about 9,000 rows a run). The Owner stores one run event per completed item instead: the
// chunks of one message (or thought) are joined into one event of the same kind and payload shape
// as a chunk, so readers of the timeline (internal/runreply, the run page) work on it unchanged,
// and its text is the whole item. Chunks are never stored; they reach live subscribers through
// Config.Live.
//
// Every item has a name, "<epoch>-<n>", unique in the run (a takeover has a new epoch). The live
// chunks carry it, and the stored event carries it as "item" in its payload. A viewer that has the
// stored event drops the live text of that item, so a live tail that arrives after its event (the
// two travel separate paths) never shows twice.
//
// An item is complete when the next thing happens: a chunk of another kind, any other run event,
// a verb that ends the run, or the end of the prompt (FlushItems). An item that is cut off, because
// the turn was interrupted, canceled or the worker stopped mid-message, is stored with
// "partial": true. An item of more than maxItemBytes is stored in pieces, each a complete event;
// readers join adjacent message events as they joined chunks. The buffer is in memory: a worker
// that dies mid-message loses at most that one item, and its takeover starts a new one.

// maxItemBytes is the text size at which an item is stored although it continues.
const maxItemBytes = 64 << 10

// flushTimeout bounds the write of an item that is stored while the run is ending.
const flushTimeout = 10 * time.Second

// coalesced are the update kinds whose chunks are joined into items.
var coalesced = map[string]bool{
	"acp.agent_message_chunk": true,
	"acp.agent_thought_chunk": true,
	"acp.user_message_chunk":  true,
}

// IsChunkKind reports whether a run event kind is a chunk kind: its updates are joined into items.
func IsChunkKind(kind string) bool { return coalesced[kind] }

// pending is the item being built; the zero value is none.
type pending struct {
	kind   string
	item   string         // its name, in the live chunks and in the stored event's "item"
	first  map[string]any // the first chunk's update, with its text replaced at the flush
	text   strings.Builder
	chunks int
}

// RecordUpdate records a harness session update of the given run event kind ("acp.<update>"). A
// text chunk is added to the item in progress and sent to Config.Live, and nothing is stored until
// the item completes. Any other update completes the item in progress and is stored as RecordRaw
// does.
func (o *Owner) RecordUpdate(ctx context.Context, kind string, payload []byte) error {
	if !coalesced[kind] {
		return o.RecordRaw(ctx, kind, payload)
	}
	var u map[string]any
	var text string
	ok := json.Unmarshal(payload, &u) == nil
	if ok {
		c, _ := u["content"].(map[string]any)
		t, isText := c["text"].(string)
		ok = c != nil && isText && c["type"] == "text"
		text = t
	}
	if !ok {
		return o.RecordRaw(ctx, kind, payload) // an image or a resource: one event as it is
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.IsFenced() {
		return ErrFenced
	}
	if o.pend.kind != kind {
		if err := o.flushLocked(ctx, false, false); err != nil {
			return err
		}
		o.itemSeq++
		o.pend = pending{kind: kind, first: u, item: fmt.Sprintf("%d-%d", o.run.Epoch, o.itemSeq)}
	}
	if o.cfg.Live != nil {
		o.cfg.Live(kind, o.pend.item, payload) // does not block
	}
	o.pend.text.WriteString(text)
	o.pend.chunks++
	if o.pend.text.Len() >= maxItemBytes {
		return o.flushLocked(ctx, false, false)
	}
	return nil
}

// FlushItems stores the item in progress, if any. partial marks it cut off. The write does not
// stop with ctx: an item the run is ending on is worth its few seconds. A fenced Owner has nothing
// to store, and that is not an error, except after a cancel: the control plane ended the run, and
// the item the agent was in the middle of is still stored (partial), so the run keeps what was said.
func (o *Owner) FlushItems(ctx context.Context, partial bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cutOff := o.IsFenced() && o.FenceKind() == FenceCanceled
	if o.pend.kind == "" || o.IsFenced() && !cutOff {
		o.pend = pending{}
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()
	if err := o.flushLocked(ctx, partial, cutOff); err != nil && !errors.Is(err, ErrFenced) {
		o.cfg.Log.Warn("storing the item in progress failed", "err", err)
	}
}

// flushLocked stores the item in progress as one run event; the caller holds o.mu.
// cutOff is set by FlushItems alone, for the item stored after a cancel.
func (o *Owner) flushLocked(ctx context.Context, partial, cutOff bool) error {
	p := o.pend
	o.pend = pending{}
	if p.kind == "" || p.text.Len() == 0 {
		return nil
	}
	if o.cfg.LiveFlush != nil {
		o.cfg.LiveFlush() // the item's live tail goes out before the event that replaces it
	}
	c, _ := p.first["content"].(map[string]any)
	c["text"] = p.text.String()
	p.first["item"] = p.item
	if p.chunks > 1 {
		p.first["chunks"] = p.chunks
	}
	if partial {
		p.first["partial"] = true
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p.first); err != nil {
		return err
	}
	return o.recordLocked(ctx, p.kind, bytes.TrimSpace(b.Bytes()), cutOff)
}
