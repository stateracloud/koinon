// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	c := &guardConn{inner: inner, guard: AllowGuard{}, owed: map[string]bool{}, pending: map[string]pendingCall{}}
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
	c := &guardConn{inner: &blockedConn{id: ping(99).ID}, guard: AllowGuard{}, owed: map[string]bool{}, pending: map[string]pendingCall{}}
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
