// SPDX-License-Identifier: Apache-2.0

package fakemodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// overloadedMessage is the text of scripted overload errors.
const overloadedMessage = "Overloaded"

// maxRequestBytes bounds request bodies; harness prompts with tool schemas stay far below this.
const maxRequestBytes = 32 << 20

// Request is a model request the server received, kept for test assertions.
type Request struct {
	// Path is the URL path, for example /v1/chat/completions.
	Path  string
	Model string
	// Body is the raw request body.
	Body []byte
	// Header is a copy of the request headers, for asserting what a gateway sent upstream.
	Header http.Header
}

// Server serves scripted model responses. The zero value is not usable; call [New].
type Server struct {
	mux *http.ServeMux

	mu       sync.Mutex
	scripts  map[string]*entry
	requests []Request
	seq      int
	created  int64
}

type entry struct {
	script  Script
	served  int // requests seen, for Faults.Unavailable
	limited int // 429s sent, for Faults.RateLimit
	// gate holds one token per released response in lockstep mode.
	gate    chan struct{}
	waiting int // requests held by the gate
}

// gateCapacity bounds how many releases may be outstanding at once.
const gateCapacity = 1024

// New returns a server with the given scripts registered.
func New(scripts ...Script) (*Server, error) {
	s := &Server{scripts: map[string]*entry{}, created: time.Now().Unix()}
	for _, sc := range scripts {
		if err := s.Register(sc); err != nil {
			return nil, err
		}
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	s.mux.HandleFunc("POST /v1/messages", s.messages)
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("GET /_fake/requests", s.listRequests)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return s, nil
}

// listRequests answers GET /_fake/requests with the path and model of every request received, in
// arrival order, so a test that reaches the binary over the network (e2e) can check which API a
// harness used. Bodies and headers stay in-process ([Server.Requests]).
func (s *Server) listRequests(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		Path  string `json:"path"`
		Model string `json:"model"`
	}
	out := []item{}
	for _, r := range s.Requests() {
		out = append(out, item{Path: r.Path, Model: r.Model})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// Register adds or replaces the script for sc.Model and resets its fault counters. Re-registering
// keeps the lockstep gate and the held requests, so Release and Waiting still see them.
func (s *Server) Register(sc Script) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.scripts[sc.Model]; ok {
		e.script, e.served, e.limited = sc, 0, 0
		return nil
	}
	s.scripts[sc.Model] = &entry{script: sc, gate: make(chan struct{}, gateCapacity)}
	return nil
}

// Release lets n held responses of a lockstep script through, in arrival order of the waiters.
// Releases made before the request arrives are kept, so a test may release first. It fails for
// an unknown model or when too many releases are outstanding.
func (s *Server) Release(model string, n int) error {
	s.mu.Lock()
	e, ok := s.scripts[model]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("fakemodel: unknown model %q", model)
	}
	for range n {
		select {
		case e.gate <- struct{}{}:
		default:
			return fmt.Errorf("fakemodel: more than %d unconsumed releases for %q", gateCapacity, model)
		}
	}
	return nil
}

// Waiting returns how many requests to model are held by its lockstep gate.
func (s *Server) Waiting(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.scripts[model]; ok {
		return e.waiting
	}
	return 0
}

// hold blocks a lockstep request until a response is released. It returns false when the
// client went away first.
func (s *Server) hold(r *http.Request, rep reply) bool {
	if !rep.faults.Lockstep {
		return true
	}
	s.mu.Lock()
	e := s.scripts[rep.model]
	e.waiting++
	gate := e.gate
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		e.waiting--
		s.mu.Unlock()
	}()
	select {
	case <-gate:
		return true
	case <-r.Context().Done():
		return false
	}
}

// Requests returns a copy of the model requests received so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// models lists the registered model names in OpenAI's shape, or Anthropic's when the request
// carries an anthropic-version header.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	names := make([]string, 0, len(s.scripts))
	for n := range s.scripts {
		names = append(names, n)
	}
	created := s.created
	s.mu.Unlock()
	slices.Sort(names)

	if r.Header.Get("anthropic-version") != "" {
		data := make([]map[string]any, 0, len(names))
		for _, n := range names {
			data = append(data, map[string]any{
				"type": "model", "id": n, "display_name": n,
				"created_at": time.Unix(created, 0).UTC().Format(time.RFC3339),
			})
		}
		body := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
		if len(names) > 0 {
			body["first_id"], body["last_id"] = names[0], names[len(names)-1]
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{"id": n, "object": "model", "created": created, "owned_by": "koinon-fakemodel"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// errorKind is an API-neutral error; each API renders it in its own body shape.
type errorKind int

const (
	errBadRequest errorKind = iota
	errNotFound
	errUnavailable
	errRateLimit
	errContextLength
	errOverloaded
)

// apiError is an error to send to the client.
type apiError struct {
	kind    errorKind
	message string
	// retryAfter is the Retry-After header in seconds, when positive.
	retryAfter int
}

func (e *apiError) Error() string { return e.message }

func (e *apiError) status() int {
	switch e.kind {
	case errNotFound:
		return http.StatusNotFound
	case errUnavailable, errOverloaded:
		return http.StatusServiceUnavailable
	case errRateLimit:
		return http.StatusTooManyRequests
	default:
		return http.StatusBadRequest
	}
}

// reply is the selected turn plus everything an API needs to render it.
type reply struct {
	model   string
	turn    Turn
	turnIdx int
	seq     int
	created int64
	faults  Faults
}

// readBody reads and records a request body, and decodes it into v.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, v any) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		return nil, &apiError{kind: errBadRequest, message: fmt.Sprintf("read body: %v", err)}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return nil, &apiError{kind: errBadRequest, message: fmt.Sprintf("invalid JSON body: %v", err)}
	}
	return body, nil
}

// plan records the request, applies Faults.Unavailable and selects the turn.
func (s *Server) plan(path string, header http.Header, model string, body []byte, conv conversation) (reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Path: path, Model: model, Body: body, Header: header.Clone()})
	e, ok := s.scripts[model]
	if !ok {
		return reply{}, &apiError{kind: errNotFound, message: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", model)}
	}
	e.served++
	if e.served <= e.script.Faults.Unavailable {
		return reply{}, &apiError{kind: errUnavailable, message: fmt.Sprintf("fakemodel: scripted outage %d of %d", e.served, e.script.Faults.Unavailable)}
	}
	if e.limited < e.script.Faults.RateLimit {
		e.limited++
		ra := max(1, e.script.Faults.RetryAfter)
		return reply{}, &apiError{kind: errRateLimit, retryAfter: ra, message: fmt.Sprintf("fakemodel: scripted rate limit %d of %d", e.limited, e.script.Faults.RateLimit)}
	}
	if e.script.Faults.ContextLength {
		return reply{}, &apiError{kind: errContextLength, message: fmt.Sprintf(
			"This model's maximum context length is 8192 tokens. However, your messages resulted in %d tokens. Please reduce the length of the messages.",
			max(8193, len(body)/4))}
	}
	idx := e.script.selectTurn(conv)
	if idx < 0 {
		return reply{}, &apiError{kind: errBadRequest, message: fmt.Sprintf(
			"fakemodel: no turn of script %q matches (tool results %q, %d assistant turns)",
			model, conv.toolResults, conv.assistantTurns)}
	}
	s.seq++
	t := e.script.Turns[idx]
	t.ToolCalls = resolveTools(t.ToolCalls, conv.tools)
	t.Usage = estimateUsage(t, len(body))
	return reply{model: model, turn: t, turnIdx: idx, seq: s.seq, created: time.Now().Unix(), faults: e.script.Faults}, nil
}

// estimateUsage fills zero usage fields with rough sizes (four bytes per token).
func estimateUsage(t Turn, requestBytes int) Usage {
	u := t.Usage
	if u.InputTokens == 0 {
		u.InputTokens = max(1, requestBytes/4)
	}
	if u.OutputTokens == 0 {
		n := len(t.Text)
		for _, c := range t.ToolCalls {
			n += len(c.Name) + len(c.argumentsJSON())
		}
		u.OutputTokens = max(1, n/4)
	}
	return u
}

func (c ToolCall) argumentsJSON() string {
	if len(c.Arguments) == 0 {
		return "{}"
	}
	b, err := json.Marshal(c.Arguments)
	if err != nil {
		// Validate-time YAML maps always marshal; this only guards odd Go-built values.
		return "{}"
	}
	return string(b)
}

// toolCallID returns the scripted ID or a deterministic one.
func (r reply) toolCallID(prefix string, i int) string {
	if id := r.turn.ToolCalls[i].ID; id != "" {
		return id
	}
	return fmt.Sprintf("%s_fake_%d_%d", prefix, r.turnIdx, i)
}

// wait applies Faults.Delay. It returns false when the client went away first.
func wait(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeBody writes a JSON response, or half of it followed by an aborted connection.
func writeBody(w http.ResponseWriter, v any, truncate bool) {
	if !truncate {
		writeJSON(w, http.StatusOK, v)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b[:len(b)/2])
	abort(w)
}

// midStreamError keeps about half of events and ends them with errEvent, so the client sees the
// stream fail partway and finish cleanly.
func midStreamError(events []sseEvent, errEvent sseEvent) []sseEvent {
	n := max(1, len(events)/2)
	return append(slices.Clone(events[:n]), errEvent)
}

func setRetryAfter(w http.ResponseWriter, e *apiError) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(e.retryAfter))
	}
}

// sseEvent is one server-sent event; an empty name writes a data-only event.
type sseEvent struct {
	name string
	data string
}

// writeSSE writes events, flushing after each. With truncate it writes about half of them and
// aborts the connection, so the client sees the stream end without its terminator.
func writeSSE(w http.ResponseWriter, events []sseEvent, truncate bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	n := len(events)
	if truncate {
		n = max(1, n/2)
	}
	var buf bytes.Buffer
	for _, e := range events[:n] {
		buf.Reset()
		if e.name != "" {
			fmt.Fprintf(&buf, "event: %s\n", e.name)
		}
		fmt.Fprintf(&buf, "data: %s\n\n", e.data)
		if _, err := w.Write(buf.Bytes()); err != nil {
			return
		}
		flush(w)
	}
	if truncate {
		abort(w)
	}
}

func flush(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}

// abort flushes what was written and drops the connection without finishing the response.
func abort(w http.ResponseWriter) {
	flush(w)
	panic(http.ErrAbortHandler)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("fakemodel: marshal: %v", err))
	}
	return string(b)
}

// chunkText splits text into word-sized pieces so streams carry several deltas.
func chunkText(s string) []string {
	return strings.SplitAfter(s, " ")
}

// splitHalf splits s into two pieces at a rune boundary near the middle, so streamed tool
// arguments arrive in more than one delta.
func splitHalf(s string) []string {
	i := len(s) / 2
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	if i == 0 {
		return []string{s}
	}
	return []string{s[:i], s[i:]}
}
