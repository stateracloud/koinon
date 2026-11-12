// SPDX-License-Identifier: Apache-2.0

// Package stateserver serves the gateway State stream (ADR 0097 decision 2.1, issue 279 slice 3):
// the GatewayStateService of koinon-api. A gateway copy opens StreamState and gets the newest stored
// revision at once and every later one; it confirms each half it serves with AckRevision.
//
// The service is not part of the session-authenticated API. It mounts on the internal listener that
// already carries the gateway's bearer token (internal/otlpauth), so the only caller that can reach
// it is a holder of the gateway's token; Config.Auth is required, and a Server without it does not
// start. Nothing here gates anything yet: acks are recorded, and the reconciler still pushes state
// with Apply (slice 4 moves the gate).
//
// The server does not depend on LISTEN/NOTIFY. One loop reads the newest revision at a fixed
// interval, and a notification only wakes that loop early: LISTEN fails behind transaction-mode
// PgBouncer (ADR 0097), and a missed notification must cost latency, not correctness.
package stateserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1/gatewayv1alpha1connect"
	"github.com/stateracloud/koinon/internal/safego"
)

const (
	// Prefix is the URL path prefix of every procedure of the service.
	Prefix = "/koinon.gateway.v1alpha1.GatewayStateService/"
	// DefaultPollInterval is how often the newest revision is read when no notification arrives.
	DefaultPollInterval = 5 * time.Second
	// MaxCopyIDLen is the longest copy id (see the proto comment).
	MaxCopyIDLen = 253

	notifyRetry = 5 * time.Second
)

// ErrUnknownRevision is what a Store returns for an ack of a revision it does not hold (pruned, or
// never published).
var ErrUnknownRevision = errors.New("stateserver: unknown revision")

// Store is what the server needs of the database.
type Store interface {
	// Latest returns the newest revision, or nil when none was published yet.
	Latest(ctx context.Context) (*gwv1.StateUpdate, error)
	// Ack records the confirmation; ErrUnknownRevision when the revision is not stored.
	Ack(ctx context.Context, copyID string, revision int64, half gwv1.Half) error
}

// NewPGStore returns the Store over the gateway_state_* tables.
func NewPGStore(db store.DBTX) Store { return pgStore{db} }

type pgStore struct{ db store.DBTX }

func (s pgStore) Latest(ctx context.Context) (*gwv1.StateUpdate, error) {
	return staterev.Latest(ctx, s.db)
}

func (s pgStore) Ack(ctx context.Context, copyID string, revision int64, half gwv1.Half) error {
	err := staterev.Ack(ctx, s.db, copyID, revision, half)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation: no such revision
		return ErrUnknownRevision
	}
	return err
}

// PoolNotify returns a Config.Notify that LISTENs on the publish channel over a connection taken out
// of pool. It needs a session-mode connection; behind transaction-mode PgBouncer it delivers
// nothing, and the poll covers that.
func PoolNotify(pool *pgxpool.Pool) func(ctx context.Context, wake func()) error {
	return func(ctx context.Context, wake func()) error {
		c, err := pool.Acquire(ctx)
		if err != nil {
			return err
		}
		raw := c.Hijack() // a listening connection never goes back to the pool
		defer func() { _ = raw.Close(context.WithoutCancel(ctx)) }()
		if _, err := raw.Exec(ctx, "LISTEN "+staterev.NotifyChannel); err != nil {
			return err
		}
		wake() // a publish between the last read and the LISTEN
		for {
			if _, err := raw.WaitForNotification(ctx); err != nil {
				return err
			}
			wake()
		}
	}
}

// Config configures a Server.
type Config struct {
	Store Store
	// Auth wraps the service's HTTP handler and must refuse every caller that is not a gateway copy
	// (koinon-api passes the bearer-token check of internal/otlpauth). Required.
	Auth func(http.Handler) http.Handler
	// Notify, when set, blocks and calls wake for each publish notification; it returns on any error
	// and is retried. Nil disables notifications: the poll alone drives the stream.
	Notify func(ctx context.Context, wake func()) error
	// PollInterval is how often the newest revision is read (default DefaultPollInterval).
	PollInterval time.Duration
	Metrics      *Metrics
	Log          *slog.Logger
}

// Server serves the GatewayStateService. Run must be running for streams to see later revisions.
type Server struct {
	cfg Config
	log *slog.Logger

	refreshMu sync.Mutex // serialises the read and the swap of cur, so a slow read cannot move cur back
	mu        sync.Mutex
	cur       *gwv1.StateUpdate
	changed   chan struct{} // closed and replaced when cur's revision changes

	stopped chan struct{} // closed when Run returns, so streams end and clients reconnect
	handler http.Handler
}

// New returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("stateserver: a store is required")
	}
	if cfg.Auth == nil {
		return nil, errors.New("stateserver: an authentication wrapper is required: the State stream is never served open")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewMetrics(nil)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, log: log, changed: make(chan struct{}), stopped: make(chan struct{})}
	srv := connect.NewServer()
	gatewayv1alpha1connect.RegisterGatewayStateServiceHandler(srv, rpcHandler{s})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	s.handler = cfg.Auth(mux)
	return s, nil
}

// Mount returns a wrapper for the internal listener: requests under Prefix go to the service,
// behind Config.Auth; every other request goes to next, untouched.
func (s *Server) Mount() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, Prefix) {
				s.handler.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Run reads the newest revision at the poll interval, and on each notification, until ctx ends.
// Streams that are open when it returns end, so their clients reconnect.
func (s *Server) Run(ctx context.Context) {
	defer close(s.stopped)
	wakes := make(chan struct{}, 1)
	wake := func() {
		select {
		case wakes <- struct{}{}:
		default:
		}
	}
	if s.cfg.Notify != nil {
		safego.Go(ctx, s.log, "gateway-state-notify", func(ctx context.Context) {
			for ctx.Err() == nil {
				if err := s.cfg.Notify(ctx, wake); err != nil && ctx.Err() == nil {
					s.log.Warn("gateway State notifications stopped; the poll covers them", "err", err)
				}
				select {
				case <-ctx.Done():
				case <-time.After(notifyRetry):
				}
			}
		})
	}
	tick := time.NewTicker(s.cfg.PollInterval)
	defer tick.Stop()
	s.refreshLogged(ctx, sourcePoll)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wakes:
			s.refreshLogged(ctx, sourceNotify)
		case <-tick.C:
			s.refreshLogged(ctx, sourcePoll)
		}
	}
}

func (s *Server) refreshLogged(ctx context.Context, source string) {
	s.cfg.Metrics.Refreshes.WithLabelValues(source).Inc()
	if _, err := s.refresh(ctx); err != nil && ctx.Err() == nil {
		s.cfg.Metrics.RefreshErrors.Inc()
		s.log.Warn("reading the newest gateway State revision failed", "err", err)
	}
}

// refresh reads the newest revision and, when its number differs from the cached one, replaces the
// cache and wakes the streams. "Differs", not "is higher": a restored database can hold a lower
// number, and a copy must follow it.
func (s *Server) refresh(ctx context.Context) (*gwv1.StateUpdate, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	u, err := s.cfg.Store.Latest(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if (u == nil) != (s.cur == nil) || (u != nil && u.GetRevision() != s.cur.GetRevision()) {
		s.cur = u
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return s.cur, nil
}

func (s *Server) view() (*gwv1.StateUpdate, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.changed
}

type rpcHandler struct{ s *Server }

func validCopyID(id string) error {
	if id == "" || len(id) > MaxCopyIDLen {
		return fmt.Errorf("copy_id must be 1 to %d characters", MaxCopyIDLen)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return errors.New("copy_id must not hold control characters")
		}
	}
	return nil
}

func (h rpcHandler) StreamState(ctx context.Context, req *gwv1.StreamStateRequest, stream gatewayv1alpha1connect.GatewayStateServiceStreamStateServerStream) error {
	s := h.s
	if err := validCopyID(req.GetCopyId()); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	// The newest revision at connect is read from the table, not from the cache: "newest at connect"
	// must not wait for the next poll.
	if _, err := s.refresh(ctx); err != nil {
		s.log.Error("gateway State stream: reading the newest revision failed", "err", err)
		return connect.NewError(connect.CodeInternal, "internal error")
	}
	s.cfg.Metrics.Streams.Inc()
	defer s.cfg.Metrics.Streams.Dec()
	// Headers now, so the copy's connect succeeds even before the first revision exists.
	if err := stream.SendHeaders(); err != nil {
		return err
	}
	var (
		sent    int64
		hasSent bool
	)
	for {
		cur, changed := s.view() // the channel first: a change after this read closes it
		if cur != nil && (!hasSent || cur.GetRevision() != sent) {
			if err := stream.Send(cur); err != nil {
				return err
			}
			s.cfg.Metrics.Sent.Inc()
			sent, hasSent = cur.GetRevision(), true
		}
		select {
		case <-changed:
		case <-s.stopped:
			return connect.NewError(connect.CodeUnavailable, "koinon-api is shutting down")
		case <-ctx.Done():
			return nil
		}
	}
}

func (h rpcHandler) AckRevision(ctx context.Context, req *gwv1.AckRevisionRequest) (*gwv1.AckRevisionResponse, error) {
	s := h.s
	half := halfLabel(req.GetHalf())
	reject := func(msg string) error {
		s.cfg.Metrics.Acks.WithLabelValues(half, resultRejected).Inc()
		return connect.NewError(connect.CodeInvalidArgument, msg)
	}
	if err := validCopyID(req.GetCopyId()); err != nil {
		return nil, reject(err.Error())
	}
	if req.GetHalf() != gwv1.Half_HALF_MODEL && req.GetHalf() != gwv1.Half_HALF_MCP {
		return nil, reject("half must be HALF_MODEL or HALF_MCP")
	}
	if req.GetRevision() <= 0 {
		return nil, reject("revision must be positive")
	}
	err := s.cfg.Store.Ack(ctx, req.GetCopyId(), req.GetRevision(), req.GetHalf())
	switch {
	case errors.Is(err, ErrUnknownRevision):
		s.cfg.Metrics.Acks.WithLabelValues(half, resultRejected).Inc()
		return nil, connect.NewError(connect.CodeFailedPrecondition, "that revision is not stored; reconnect to get the newest")
	case err != nil:
		s.cfg.Metrics.Acks.WithLabelValues(half, resultError).Inc()
		s.log.Error("recording a gateway State ack failed", "err", err)
		return nil, connect.NewError(connect.CodeInternal, "internal error")
	}
	s.cfg.Metrics.Acks.WithLabelValues(half, resultOK).Inc()
	return &gwv1.AckRevisionResponse{}, nil
}

// Fixed label values: no copy, org or agent ids in any label (ADR 0023).
const (
	sourcePoll     = "poll"
	sourceNotify   = "notify"
	resultOK       = "ok"
	resultRejected = "rejected"
	resultError    = "error"
)

func halfLabel(h gwv1.Half) string {
	switch h {
	case gwv1.Half_HALF_MODEL:
		return "model"
	case gwv1.Half_HALF_MCP:
		return "mcp"
	}
	return "invalid"
}

// Metrics are the server's metrics, all with fixed label sets.
type Metrics struct {
	Streams       prometheus.Gauge
	Sent          prometheus.Counter
	Acks          *prometheus.CounterVec
	Refreshes     *prometheus.CounterVec
	RefreshErrors prometheus.Counter
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Streams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_api_gateway_state_streams",
			Help: "Open gateway State streams.",
		}),
		Sent: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_api_gateway_state_revisions_sent_total",
			Help: "State revisions sent to gateway copies.",
		}),
		Acks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_gateway_state_acks_total",
			Help: "Gateway State acks by half (model, mcp, invalid) and result (ok, rejected, error).",
		}, []string{"half", "result"}),
		Refreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_gateway_state_refreshes_total",
			Help: "Reads of the newest State revision by what woke them (poll, notify).",
		}, []string{"source"}),
		RefreshErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_api_gateway_state_refresh_errors_total",
			Help: "Reads of the newest State revision that failed.",
		}),
	}
	for _, h := range []string{"model", "mcp", "invalid"} {
		for _, r := range []string{resultOK, resultRejected, resultError} {
			m.Acks.WithLabelValues(h, r)
		}
	}
	m.Refreshes.WithLabelValues(sourcePoll)
	m.Refreshes.WithLabelValues(sourceNotify)
	if reg != nil {
		reg.MustRegister(m.Streams, m.Sent, m.Acks, m.Refreshes, m.RefreshErrors)
	}
	return m
}
