// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// blockedConn is an mcp.Connection whose Write waits until release is closed.
type blockedConn struct {
	mcp.Connection
	release chan struct{}
	started chan struct{}
	id      jsonrpc.ID
}

func (b *blockedConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if r, ok := msg.(*jsonrpc.Response); !ok || r.ID != b.id {
		return nil // a refusal written by the gateway does not block
	}
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

func ping(id int64) *jsonrpc.Request {
	i, _ := jsonrpc.MakeID(float64(id))
	return &jsonrpc.Request{ID: i, Method: "ping"}
}

// A server request keeps its place in the bound until its reply write ends.
func TestOwedStaysUntilReplyWriteEnds(t *testing.T) {
	inner := &blockedConn{release: make(chan struct{}), started: make(chan struct{}, 1), id: ping(0).ID}
	c := &guardConn{inner: inner, guard: AllowGuard{}, owed: map[string]*owedEntry{}, pending: map[string]pendingCall{}}
	ctx := context.Background()
	for i := range int64(maxInboundWorkers) {
		if _, ok := c.serverRequest(ctx, ping(i)); !ok {
			t.Fatalf("request %d was not admitted", i)
		}
	}
	m, _ := c.serverRequest(ctx, ping(1000))
	if _, isReq := m.(*jsonrpc.Request); isReq {
		t.Fatal("request over the bound was admitted")
	}
	reply := &jsonrpc.Response{ID: ping(0).ID, Result: []byte("{}")}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = c.Write(ctx, reply) }()
	select {
	case <-inner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the reply write did not start")
	}
	// The write is blocked: its place is still taken.
	m, _ = c.serverRequest(ctx, ping(1001))
	if _, isReq := m.(*jsonrpc.Request); isReq {
		t.Fatal("a blocked reply write freed its place")
	}
	close(inner.release)
	wg.Wait()
	m, _ = c.serverRequest(ctx, ping(1002))
	if _, isReq := m.(*jsonrpc.Request); !isReq {
		t.Fatal("the place was not freed after the write")
	}
}

// A response with no room to inspect ends the connection; a server request is dropped.
func TestOverloadedEndsOnlyForAResponse(t *testing.T) {
	c := &guardConn{pending: map[string]pendingCall{}}
	id, err := jsonrpc.MakeID(float64(7))
	if err != nil {
		t.Fatal(err)
	}
	c.pending[idKey(id)] = pendingCall{method: "tools/call"}
	if err := c.overloaded(&jsonrpc.Request{ID: id, Method: "ping"}); err != nil {
		t.Fatalf("a request must be dropped, got %v", err)
	}
	if err := c.overloaded(&jsonrpc.Response{ID: id}); err == nil {
		t.Fatal("a response must end the connection")
	}
	if len(c.pending) != 0 {
		t.Fatal("the pending record must be removed")
	}
}

// A reply for an ID that is not owed leaves the owed entries alone: a late reply to a request that the
// timer already released must not remove a newer request.
func TestReplyForUnowedIDKeepsOwedEntries(t *testing.T) {
	c := &guardConn{inner: &blockedConn{id: ping(99).ID}, guard: AllowGuard{}, owed: map[string]*owedEntry{}, pending: map[string]pendingCall{}}
	ctx := context.Background()
	if _, ok := c.serverRequest(ctx, ping(1)); !ok {
		t.Fatal("request 1 was not admitted")
	}
	if err := c.Write(ctx, &jsonrpc.Response{ID: ping(2).ID, Result: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	n := len(c.owed)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("a reply for an unowed ID changed the owed entries: %d left, want 1", n)
	}
	if err := c.Write(ctx, &jsonrpc.Response{ID: ping(1).ID, Result: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	n = len(c.owed)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("the reply of the owed request left %d entries, want 0", n)
	}
}

// The timer of a request that was answered must not remove a newer request that reuses its ID.
func TestStaleOwedTimerKeepsNewerEntry(t *testing.T) {
	c := &guardConn{inner: &blockedConn{id: ping(99).ID}, guard: AllowGuard{}, owed: map[string]*owedEntry{}, pending: map[string]pendingCall{}, timeout: 600 * time.Millisecond}
	ctx := context.Background()
	if _, ok := c.serverRequest(ctx, ping(1)); !ok {
		t.Fatal("request 1 was not admitted")
	}
	if err := c.Write(ctx, &jsonrpc.Response{ID: ping(1).ID, Result: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := c.serverRequest(ctx, ping(1)); !ok { // a new request with the same ID
		t.Fatal("the new request was not admitted")
	}
	time.Sleep(450 * time.Millisecond) // the timer of the first request (600 ms) has fired, the second (900 ms) has not
	c.mu.Lock()
	n := len(c.owed)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("the stale timer removed the newer entry: %d left, want 1", n)
	}
}

// The guard has CallTimeout for a response, a server request and a notification, not a fixed 30 s.
func TestInboundLimitIsTheCallTimeout(t *testing.T) {
	for _, tc := range []struct {
		timeout time.Duration
		want    time.Duration
	}{{2 * time.Minute, 2 * time.Minute}, {5 * time.Second, 5 * time.Second}, {0, unsolicitedTimeout}} {
		c := &guardConn{life: context.Background(), timeout: tc.timeout, pending: map[string]pendingCall{}}
		for name, msg := range map[string]jsonrpc.Message{
			"server request": ping(1),
			"response":       &jsonrpc.Response{ID: ping(2).ID, Result: []byte("{}")},
		} {
			ctx, cancel, _ := c.contextFor(msg)
			dl, ok := ctx.Deadline()
			cancel()
			if !ok {
				t.Fatalf("%s: no deadline", name)
			}
			if got := time.Until(dl); got > tc.want || got < tc.want-2*time.Second {
				t.Fatalf("%s: limit %v, want about %v", name, got, tc.want)
			}
		}
	}
}

// A full notification queue drops the notification, and the drop is counted.
func TestFullNotificationQueueIsCounted(t *testing.T) {
	m := NewMetrics(nil)
	c := &guardConn{notes: make(chan jsonrpc.Message, 1), metrics: m}
	n := &jsonrpc.Request{Method: "notifications/message"}
	c.enqueueNote(n)
	c.enqueueNote(n)
	c.enqueueNote(n)
	if got := testutil.ToFloat64(m.NotesDropped); got != 2 {
		t.Fatalf("dropped %v notifications, want 2", got)
	}
}

// A pooled session does not judge a message under the snapshot of the request that connected it.
func TestPooledSessionContextDropsRequestSnapshot(t *testing.T) {
	g := New(Options{})
	old := &snapshot{rev: "old"}
	g.snap.Store(&snapshot{rev: "now"})
	reqCtx := context.WithValue(context.Background(), reqKey{}, old)
	if got := g.snapOf(reqCtx).rev; got != "old" {
		t.Fatalf("the request itself must see its snapshot, got %q", got)
	}
	if got := g.snapOf(detach(reqCtx)).rev; got != "now" {
		t.Fatalf("a pooled session kept the snapshot of the request that connected it: %q", got)
	}
}
