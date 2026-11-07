// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Live chunks (old issue 344, ADR 0074). The text an agent is typing is not stored chunk by chunk (the
// run's events are completed items); it is streamed. agentd batches the chunks of a run about every
// ChunkInterval and sends each batch as a NOTIFY on Channel whose payload is
//
//	<org_id>/<run_id>/<JSON array of Chunk>
//
// under Postgres's 8,000-byte payload cap. Postgres delivers a NOTIFY to every API replica, the Hub
// parses it and hands the batch to the run's subscriptions (Subscription.Chunks), and WatchRun sends
// it on as a LiveChunks message. A notification without the third part is the plain wake-up.
// Chunks are best effort: a missed or dropped batch costs the viewer some live text, never an
// event, because the completed item arrives from the store with its whole text.

// ChunkInterval is how often agentd sends the chunks it has collected.
const ChunkInterval = 100 * time.Millisecond

// maxPayload is the size agentd keeps a NOTIFY payload under (Postgres refuses 8,000 bytes or more).
const maxPayload = 7500

// maxPiece bounds the text of one Chunk in a payload. JSON escaping can make a byte up to six, so
// a piece of this size always fits in maxPayload with its framing.
const maxPiece = 1000

// maxPending bounds the text a publisher holds between two sends; past it, chunks are dropped.
const maxPending = 1 << 20

// Chunk is a piece of text of a message or thought in progress. Adjacent chunks of one kind are
// joined by the publisher.
type Chunk struct {
	// Kind is the run event kind of the item it belongs to ("acp.agent_message_chunk" and so on).
	Kind string `json:"k"`
	Text string `json:"t"`
	// Item names the item the text belongs to; the run event stored for the item carries the same
	// name in its payload ("item"). A viewer that has the stored event drops the item's live text,
	// whatever order the two arrived in.
	Item string `json:"i,omitempty"`
}

// Execer is what a ChunkPublisher needs of the database; *pgxpool.Pool is one.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ChunkPublisher batches a run's live chunks and sends them as NOTIFYs. It is safe for concurrent use.
type ChunkPublisher struct {
	db     Execer
	prefix string // "<org>/<run>/"
	log    *slog.Logger

	sendMu  sync.Mutex // one send at a time, so Flush returns only after earlier batches are out
	mu      sync.Mutex
	pending []Chunk
	size    int

	stop chan struct{}
	done chan struct{}
}

// NewChunkPublisher starts a publisher for the run that sends every interval (ChunkInterval if not
// positive) while it has something to send. Close it when the run ends.
func NewChunkPublisher(db Execer, org, run pgtype.UUID, interval time.Duration, log *slog.Logger) *ChunkPublisher {
	if interval <= 0 {
		interval = ChunkInterval
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &ChunkPublisher{
		db: db, prefix: Key(org, run) + "/", log: log,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go p.loop(interval)
	return p
}

// Add queues the chunk of a harness update ("acp.<update>", the update's JSON) of the given item.
// It never blocks on the database. An update without text is ignored.
func (p *ChunkPublisher) Add(kind, item string, payload []byte) {
	var u struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(payload, &u) != nil || u.Content.Type != "text" || u.Content.Text == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.size+len(u.Content.Text) > maxPending {
		return
	}
	p.size += len(u.Content.Text)
	if n := len(p.pending); n > 0 && p.pending[n-1].Kind == kind && p.pending[n-1].Item == item {
		p.pending[n-1].Text += u.Content.Text
		return
	}
	p.pending = append(p.pending, Chunk{Kind: kind, Item: item, Text: u.Content.Text})
}

// Flush sends what is queued now and returns when it is sent (or dropped). The Owner calls it before
// it stores an item, so the tail of the item's live text usually reaches a watcher before the event
// that replaces it. That is an optimisation: a tail can still arrive after the event (a Hub hands
// over a notification and a stored item on separate paths), and the Item name on every chunk is what
// lets a viewer drop it.
func (p *ChunkPublisher) Flush() { p.flush() }

// Close sends what is queued and stops the publisher.
func (p *ChunkPublisher) Close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

func (p *ChunkPublisher) loop(interval time.Duration) {
	defer close(p.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.flush()
		case <-p.stop:
			p.flush()
			return
		}
	}
}

func (p *ChunkPublisher) flush() {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	p.mu.Lock()
	batch := p.pending
	p.pending, p.size = nil, 0
	p.mu.Unlock()
	for _, body := range Payloads(batch) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := p.db.Exec(ctx, "SELECT pg_notify($1, $2)", Channel, p.prefix+body)
		cancel()
		if err != nil {
			// Live text is best effort; the item is stored whole when it completes.
			p.log.Debug("dropped live chunks that could not be sent", "err", err)
			return
		}
	}
}

// Payloads encodes chunks as JSON arrays, each small enough for one NOTIFY payload with the
// "<org>/<run>/" prefix. Order is kept.
func Payloads(chunks []Chunk) []string {
	var out []string
	var cur []Chunk
	curSize := 2
	emit := func() {
		if len(cur) == 0 {
			return
		}
		b, _ := json.Marshal(cur)
		out = append(out, string(b))
		cur, curSize = nil, 2
	}
	for _, c := range chunks {
		for _, piece := range splitText(c.Text, maxPiece) {
			ch := Chunk{Kind: c.Kind, Item: c.Item, Text: piece}
			b, _ := json.Marshal(ch)
			if curSize+len(b)+1 > maxPayload-80 { // 80: the prefix "<org>/<run>/"
				emit()
			}
			cur = append(cur, ch)
			curSize += len(b) + 1
		}
	}
	emit()
	return out
}

// splitText cuts s into pieces of at most n bytes, on rune boundaries.
func splitText(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = n
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// parseChunks decodes the part of a notification payload after "<org>/<run>/".
func parseChunks(body string) []Chunk {
	var cs []Chunk
	if err := json.Unmarshal([]byte(body), &cs); err != nil {
		return nil
	}
	return cs
}

// splitPayload splits a notification payload into its run key and its chunk body ("" for a plain
// wake-up). ok is false for a payload that is neither.
func splitPayload(payload string) (key, body string, ok bool) {
	org, rest, found := strings.Cut(payload, "/")
	if !found {
		return "", "", false
	}
	run, body, _ := strings.Cut(rest, "/")
	return org + "/" + run, body, true
}
