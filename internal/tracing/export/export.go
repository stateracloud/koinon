// SPDX-License-Identifier: Apache-2.0

// Package export sends OTLP/HTTP protobuf traces to one destination (ADR 0074, decisions 7 to 9).
//
// A Client dials only what its dial function allows (the platform's egress policy, as
// internal/mcpservers/diagnose.go does), follows no redirects and reads partial success from the
// answer. It does not queue, retry or batch: the trace router owns the queues and decides what to do
// with a retryable error. Errors and results carry no secret value.
package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// DialContext is the shape of net.Dialer.DialContext; catalog.URLPolicy.DialContext returns one.
type DialContext = func(ctx context.Context, network, addr string) (net.Conn, error)

// maxAnswer is how much of a backend's answer is read.
const maxAnswer = 64 << 10

// DefaultTimeout bounds one request.
const DefaultTimeout = 10 * time.Second

// Client sends batches. It is safe for concurrent use and keeps connections to a destination open.
type Client struct {
	http *http.Client
}

// NewClient returns a Client that dials with dial. A nil dial is refused: a client must be given the
// egress policy explicitly.
func NewClient(dial DialContext, timeout time.Duration) (*Client, error) {
	if dial == nil {
		return nil, errors.New("export: a dial function from the egress policy is required")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{http: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dial,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			// A destination is reached directly. The environment's proxy settings would send the
			// traffic around the egress policy's dial check.
			Proxy: nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Close drops idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Kind says why an export failed, so the router knows whether to retry.
type Kind int

// The kinds of failure.
const (
	// KindNetwork: no answer (refused, timed out, name not resolved, policy refused the address).
	// Retryable.
	KindNetwork Kind = iota + 1
	// KindServer: HTTP 429 or 5xx. Retryable, after RetryAfter when the backend gave one.
	KindServer
	// KindRejected: another 4xx. The batch or the credentials are wrong; not retryable.
	KindRejected
	// KindRedirect: a 3xx. Redirects are never followed; not retryable.
	KindRedirect
	// KindInvalid: the destination or the batch cannot be sent. Not retryable.
	KindInvalid
)

// Error is a failed export. Message is safe to show: it has no credential and no backend text beyond
// a short, cleaned excerpt.
type Error struct {
	Kind       Kind
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Message }

// Retryable says a later attempt may succeed.
func (e *Error) Retryable() bool { return e.Kind == KindNetwork || e.Kind == KindServer }

// Result is an accepted batch.
type Result struct {
	Status int
	// Rejected is the number of spans the backend refused in a partial success.
	Rejected int64
	// Message is the backend's partial-success explanation, cleaned and cut; empty otherwise.
	Message string
}

// Export sends req to d. It returns an *Error for a transport failure or a non-2xx answer. A 2xx
// answer with rejected spans is a Result with Rejected above zero and no error: the batch was
// received, and the router counts what was refused.
func (c *Client) Export(ctx context.Context, d Destination, req *coltracepb.ExportTraceServiceRequest) (Result, error) {
	headers, err := d.Headers()
	if err != nil {
		return Result{}, &Error{Kind: KindInvalid, Message: err.Error()}
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return Result{}, &Error{Kind: KindInvalid, Message: "the batch cannot be encoded"}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, &Error{Kind: KindInvalid, Message: "the endpoint cannot be requested"}
	}
	for k, vs := range headers {
		hr.Header[k] = vs
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	hr.Header.Set("Accept", "application/x-protobuf")
	hr.Header.Set("User-Agent", "koinon-trace-exporter")

	secrets := secretValues(d)
	resp, err := c.http.Do(hr)
	if err != nil {
		return Result{}, &Error{Kind: KindNetwork, Message: "could not send: " + networkReason(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))

	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300:
		return parseSuccess(status, resp.Header.Get("Content-Type"), answer, secrets), nil
	case status >= 300 && status < 400:
		return Result{}, &Error{
			Kind: KindRedirect, Status: status,
			Message: fmt.Sprintf("the endpoint answered HTTP %d, a redirect, which is not followed; use the URL it redirects to", status),
		}
	case status == http.StatusTooManyRequests || status >= 500:
		return Result{}, &Error{
			Kind: KindServer, Status: status, RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
			Message: failureMessage(status, answer, secrets),
		}
	}
	return Result{}, &Error{Kind: KindRejected, Status: status, Message: failureMessage(status, answer, secrets)}
}

func parseSuccess(status int, contentType string, answer []byte, secrets []string) Result {
	res := Result{Status: status}
	// Only a protobuf answer is read; a backend that answers JSON or nothing accepted the batch.
	if len(answer) == 0 || !strings.Contains(strings.ToLower(contentType), "protobuf") {
		return res
	}
	var out coltracepb.ExportTraceServiceResponse
	if proto.Unmarshal(answer, &out) != nil {
		return res
	}
	if ps := out.GetPartialSuccess(); ps != nil {
		res.Rejected = ps.GetRejectedSpans()
		res.Message = clean(ps.GetErrorMessage(), secrets)
	}
	return res
}

// secretValues are the values that must never appear in a message.
func secretValues(d Destination) []string {
	out := make([]string, 0, len(d.Secrets))
	for _, v := range d.Secrets {
		if len(v) >= 4 {
			out = append(out, v)
		}
	}
	return out
}

// clean makes a backend's text fit for showing: secrets out, control characters out, cut short.
func clean(s string, secrets []string) string {
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "..."
	}
	return s
}

// failureMessage names the status and, when the backend sent a short text, quotes it cleaned.
func failureMessage(status int, answer []byte, secrets []string) string {
	msg := fmt.Sprintf("the backend answered HTTP %d", status)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " (the credentials were refused)"
	case http.StatusNotFound:
		msg += " (check the endpoint URL)"
	}
	if t := clean(string(answer), secrets); t != "" && !strings.ContainsAny(t, "<>") {
		msg += ": " + t
	}
	return msg
}

func retryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 && secs <= 3600 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// networkReason names a connection failure without quoting addresses or anything a server sent.
func networkReason(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dns):
		return "the name does not resolve"
	case strings.Contains(err.Error(), "egress policy"):
		return "the address is not allowed by the egress policy"
	case strings.Contains(err.Error(), "refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "tls"):
		return "TLS handshake failed"
	}
	return "network error"
}
