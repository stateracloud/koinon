// SPDX-License-Identifier: Apache-2.0

// Package stateclient is the gateway copy's side of the State stream (ADR 0097 decision 2.1, issue
// 279 slice 3). It opens StreamState on koinon-api, applies the model half and the MCP half of each
// revision through the half's own apply call, and acknowledges each half once that half is applied.
//
// The model after xDS (Envoy's aggregated discovery service): versioned full snapshots, the client
// acks what it applied, and a reconnect starts from the newest snapshot. It differs in three ways:
// one install-wide revision number for both halves (the ack names the half), no NACK (a half that
// fails to apply is not acked, and the failure is logged and counted), and no nonce (the copy id,
// unique per process incarnation, plays that part).
package stateclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"time"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1/gatewayv1alpha1connect"
)

// Defaults of Config.
const (
	DefaultMinBackoff   = time.Second
	DefaultMaxBackoff   = 30 * time.Second
	DefaultApplyTimeout = 2 * time.Minute
	ackAttempts         = 3
	// maxBase leaves room in stateserver.MaxCopyIDLen (253) for "-" and the 16-digit boot nonce.
	maxBase = 236
)

// NewCopyID returns a copy id for this process incarnation: base (for example the pod UID) and a
// random boot nonce. Call it once per process start; a copy must never reuse an id after a restart
// (acks are recorded under it).
func NewCopyID(base string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("stateclient: boot nonce: %w", err)
	}
	if base == "" {
		base = "gateway"
	}
	if len(base) > maxBase {
		cut := maxBase
		for cut > 0 && !utf8.RuneStart(base[cut]) { // never split a multi-byte rune
			cut--
		}
		base = base[:cut]
	}
	return base + "-" + hex.EncodeToString(b[:]), nil
}

// Config configures a Client.
type Config struct {
	// URL is the base URL of the koinon-api listener that serves the stream.
	URL string
	// Token returns the gateway's bearer token. It is called for each request, so a rotated token
	// is picked up without a restart.
	Token func() (string, error)
	// HTTP is the client to use; it must have no overall timeout (the stream is long-lived).
	HTTP *http.Client
	// CopyID is this process incarnation's id (NewCopyID).
	CopyID string
	// Model and MCP apply the two halves. A copy that serves one path only leaves the other nil, and
	// then does not ack that half.
	Model gateway.ModelPathGateway
	MCP   gateway.MCPGateway

	// MinBackoff and MaxBackoff bound the wait between connection attempts (defaults above).
	MinBackoff, MaxBackoff time.Duration
	// ApplyTimeout bounds one apply call (default DefaultApplyTimeout).
	ApplyTimeout time.Duration
	Metrics      *Metrics
	Log          *slog.Logger
}

// Client keeps one gateway copy in step with the State stream.
type Client struct {
	cfg Config
	log *slog.Logger
	api gatewayv1alpha1connect.GatewayStateServiceClient

	// applied is the revision each half last applied successfully, acked the last one acked, so a
	// reconnect that replays the same revision neither re-applies it nor loses a failed ack.
	applied map[gwv1.Half]int64
	acked   map[gwv1.Half]int64
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.URL == "":
		return nil, errors.New("stateclient: a URL is required")
	case cfg.Token == nil:
		return nil, errors.New("stateclient: a token source is required")
	case cfg.CopyID == "" || len(cfg.CopyID) > 253:
		return nil, errors.New("stateclient: the copy id must be 1 to 253 characters (see NewCopyID)")
	case cfg.Model == nil && cfg.MCP == nil:
		return nil, errors.New("stateclient: at least one half must be served")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{}
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(DefaultMaxBackoff, cfg.MinBackoff)
	}
	if cfg.ApplyTimeout <= 0 {
		cfg.ApplyTimeout = DefaultApplyTimeout
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewMetrics(nil)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	hc := *cfg.HTTP
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc.Transport = bearer{base: base, token: cfg.Token}
	return &Client{
		cfg: cfg, log: log,
		api:     gatewayv1alpha1connect.NewGatewayStateServiceClient(connect.NewClient(connecthttp.NewTransport(&hc, cfg.URL))),
		applied: map[gwv1.Half]int64{}, acked: map[gwv1.Half]int64{},
	}, nil
}

type bearer struct {
	base  http.RoundTripper
	token func() (string, error)
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, err := b.token()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return b.base.RoundTrip(r)
}

// Run follows the stream until ctx ends, reconnecting with backoff (and jitter) after any failure.
// On each connect the server sends the newest revision as a full snapshot, so a reconnect needs no
// state of its own. It returns ctx's error.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.MinBackoff
	for ctx.Err() == nil {
		n, handled, err := c.session(ctx)
		if ctx.Err() != nil {
			break
		}
		c.log.Warn("gateway State stream ended; reconnecting", "err", err, "revisions", n, "retry_in", backoff)
		if handled {
			backoff = c.cfg.MinBackoff // the stream worked: every revision it carried applied and acked
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff/2 + mrand.N(backoff/2+1)): //nolint:gosec // jitter, not a secret
		}
		backoff = min(backoff*2, c.cfg.MaxBackoff)
	}
	return ctx.Err()
}

// session runs one connection and returns how many revisions it received, whether the session
// ended with every received revision applied and acked (only then does Run reset its backoff; a
// persistent apply failure must keep growing it), and why it ended.
func (c *Client) session(ctx context.Context) (int, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.api.StreamState(ctx, &gwv1.StreamStateRequest{CopyId: c.cfg.CopyID})
	if err != nil {
		c.cfg.Metrics.Connects.WithLabelValues(connectFailed).Inc()
		return 0, false, err
	}
	defer func() { _ = stream.Close() }()
	n := 0
	for {
		u, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return n, n > 0, errors.New("the server closed the stream")
		}
		if err != nil {
			if n == 0 {
				c.cfg.Metrics.Connects.WithLabelValues(connectFailed).Inc()
			}
			return n, n > 0, err
		}
		if n == 0 {
			c.cfg.Metrics.Connects.WithLabelValues(connectEstablished).Inc()
		}
		n++
		if err := c.handle(ctx, u); err != nil {
			// An ack that could not be recorded: reconnecting replays the newest revision, which is
			// applied (idempotent) or skipped and acked again.
			return n, false, err
		}
	}
}

// handle applies both halves of u and acks each one that applied. The halves are independent: one
// failing to apply does not hold back the other. It returns an error for a lost ack or a failed apply, which ends the session so that Run
// reconnects and the replayed newest revision retries the half.
func (c *Client) handle(ctx context.Context, u *gwv1.StateUpdate) error {
	state := staterev.FromProto(u)
	var ackErr error
	if c.cfg.Model != nil {
		ackErr = errors.Join(ackErr, c.half(ctx, u.GetRevision(), gwv1.Half_HALF_MODEL, func(ctx context.Context) error {
			_, err := c.cfg.Model.ApplyModel(ctx, state.ModelConfig())
			return err
		}))
	}
	if c.cfg.MCP != nil {
		ackErr = errors.Join(ackErr, c.half(ctx, u.GetRevision(), gwv1.Half_HALF_MCP, func(ctx context.Context) error {
			_, err := c.cfg.MCP.ApplyMCP(ctx, state.MCPConfig())
			return err
		}))
	}
	return ackErr
}

func (c *Client) half(ctx context.Context, rev int64, h gwv1.Half, apply func(context.Context) error) error {
	name := halfLabel(h)
	// Skipped only for the very revision already applied. A lower number (a restored database) is
	// applied again, because the copy must follow the stream, not remember the highest.
	if got, ok := c.applied[h]; !ok || got != rev {
		actx, cancel := context.WithTimeout(ctx, c.cfg.ApplyTimeout)
		err := apply(actx)
		cancel()
		var rejected *gateway.RejectedError
		switch {
		case err == nil:
		case errors.As(err, &rejected):
			// The gateway serves everything but the rejected objects: the half is live, so it is acked
			// (the reconciler reports each rejection on its org).
			c.log.Warn("gateway rejected objects of a State revision", "half", name, "revision", rev, "err", err)
		default:
			c.cfg.Metrics.Applied.WithLabelValues(name, resultFailed).Inc()
			c.log.Error("applying a State revision failed; not acked", "half", name, "revision", rev, "err", err)
			delete(c.applied, h)
			// End the session: Run reconnects with backoff and the server replays the newest
			// revision, which applies this half again (the other half is skipped as applied).
			return fmt.Errorf("apply %s revision %d: %w", name, rev, err)
		}
		c.cfg.Metrics.Applied.WithLabelValues(name, resultOK).Inc()
		c.applied[h] = rev
	}
	if got, ok := c.acked[h]; ok && got == rev {
		return nil
	}
	var err error
	for attempt := 1; attempt <= ackAttempts; attempt++ {
		_, err = c.api.AckRevision(ctx, &gwv1.AckRevisionRequest{CopyId: c.cfg.CopyID, Revision: rev, Half: h})
		if err == nil {
			c.acked[h] = rev
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		switch connect.CodeOf(err) {
		case connect.CodeFailedPrecondition, connect.CodeInvalidArgument, connect.CodeUnauthenticated:
			// Waiting cannot make these succeed (a pruned revision, a bad request, a bad token):
			// end the session so Run reconnects and gets the newest revision.
			attempt = ackAttempts
		}
		if attempt == ackAttempts {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
		}
	}
	c.cfg.Metrics.AckFailures.WithLabelValues(name).Inc()
	return fmt.Errorf("ack %s revision %d: %w", name, rev, err)
}

const (
	connectEstablished = "established"
	connectFailed      = "failed"
	resultOK           = "ok"
	resultFailed       = "failed"
)

func halfLabel(h gwv1.Half) string {
	if h == gwv1.Half_HALF_MCP {
		return "mcp"
	}
	return "model"
}

// Metrics are the client's metrics. Labels are fixed sets: never the copy, an org or an agent.
type Metrics struct {
	Connects    *prometheus.CounterVec
	Applied     *prometheus.CounterVec
	AckFailures *prometheus.CounterVec
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Connects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_state_connects_total",
			Help: "State stream connection attempts by result (established, failed).",
		}, []string{"result"}),
		Applied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_state_revisions_applied_total",
			Help: "State revisions applied by half (model, mcp) and result (ok, failed).",
		}, []string{"half", "result"}),
		AckFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_state_ack_failures_total",
			Help: "State acks that could not be recorded after retries, by half.",
		}, []string{"half"}),
	}
	for _, r := range []string{connectEstablished, connectFailed} {
		m.Connects.WithLabelValues(r)
	}
	for _, h := range []string{"model", "mcp"} {
		m.AckFailures.WithLabelValues(h)
		for _, r := range []string{resultOK, resultFailed} {
			m.Applied.WithLabelValues(h, r)
		}
	}
	if reg != nil {
		reg.MustRegister(m.Connects, m.Applied, m.AckFailures)
	}
	return m
}
