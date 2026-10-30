// SPDX-License-Identifier: Apache-2.0

package router

import "sync"

// flight runs one call per key at a time: callers that arrive while it runs wait for its result
// instead of starting their own. It keeps a slow database from being asked the same question by every
// intake worker at once.
type flight struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  any
	err  error
}

func (f *flight) do(key string, fn func() (any, error)) (any, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*flightCall{}
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-c.done
		return c.val, c.err
	}
	c := &flightCall{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	c.val, c.err = fn()
	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	close(c.done)
	return c.val, c.err
}
