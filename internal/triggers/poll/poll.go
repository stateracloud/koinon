// SPDX-License-Identifier: Apache-2.0

// Package poll runs one shared poller per (org, connection, resource) for the code-host event
// triggers (ADR 0066, amendment of 2026-11-12; issue #25).
//
// A poller is a row of code_host_pollers. Every trigger that watches the same resource through the
// same connection shares it, so the provider sees one request per interval however many agents
// listen. One replica polls at a time: the one holding the leader advisory lock, as the
// reconciler does (internal/reconciler/loop.go). Each pass syncs the rows with the subscriptions
// in effect, claims the due rows (FOR UPDATE SKIP LOCKED, with a lease), polls each one and
// stores the outcome and the row's own next_poll_at.
//
// A poll is conditional where the provider allows it: GitHub gets If-None-Match with the stored
// ETag and answers 304, which costs no primary rate limit; Gitea sends no validator on lists, so
// the adapter compares a hash of the first page. Either way an unchanged resource ends the poll
// before any trigger work: no workflow, no dedupe record, no subscription lookup.
//
// The interval is computed and stored per row: next_poll_at = now + max(the provider's floor,
// the provider's X-Poll-Interval), or the provider's Retry-After or rate-limit reset, or an
// error backoff, whichever is later. Nothing sleeps on a global minimum: a row told 120 s never
// holds back a row told 60 s.
//
// New items go to a DBOS workflow each (workflow ID and dedupe key built from the connection, the
// resource and the item's ID at the provider), and the cursor is stored only after every item
// was enqueued. The workflow runs the delivery inside durable.Once (SourcePoll), so a re-poll, a
// restart or a second leader deliver an item once.
//
// Prior art. The shape (a shared poller per resource with an ETag, the provider's poll interval
// honoured, per-key scheduling) is adapted from hydro-project/infinity (Apache-2.0), with one
// difference: infinity sleeps on the smallest interval of all keys, so one key told 60 s polls
// every key every 60 s; here each row stores its own next_poll_at. Revalidating on every poll and
// keeping the validator per credential follow Prow's ghcache (kubernetes-sigs/prow, Apache-2.0).
// No code is copied from either.
package poll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
)

// Defaults (recorded in ADR 0066's amendment of 2026-11-12; all are Config fields).
const (
	// DefaultFloor is the shortest interval between two polls of one resource, whatever the
	// provider says.
	DefaultFloor = time.Minute
	// DefaultLease is how long a claimed row stays claimed if its poll dies with its leader.
	DefaultLease = 5 * time.Minute
	// DefaultResync is the longest the leader sleeps between passes, so new subscriptions get
	// their poller.
	DefaultResync = 30 * time.Second
	// DefaultBatch is how many due rows of one org a pass claims at a time.
	DefaultBatch = 50
	// MaxBackoff bounds the wait after errors and rate limits.
	MaxBackoff = time.Hour
	// EventWorkflow is the registered name of the workflow of one polled item.
	EventWorkflow = "koinon.code-host-poll-event"

	errorBackoffBase = time.Minute
	minWake          = time.Second
	maxErrorText     = 500
	maxDedupeKey     = 512
	// leaderLock is the advisory lock key of the polling replica ("koinonpl").
	leaderLock int64 = 0x6b6f696e6f6e706c
)

// Outcomes of a poll, the values of last_status and of the metric's outcome label.
const (
	OutcomeNotModified = "not_modified"
	OutcomeChanged     = "changed"
	OutcomeError       = "error"
	OutcomeRateLimited = "rate_limited"
)

// Subscription is one trigger watching one resource through one connection.
type Subscription struct {
	Org        pgtype.UUID
	Connection pgtype.UUID
	Resource   codehost.Resource
	// Agent and Trigger name the trigger, for the Deliverer.
	Agent   pgtype.UUID
	Trigger string
}

func (s Subscription) keep() string { return s.Connection.String() + " " + s.Resource.String() }

// Subscriptions lists the subscriptions in effect across orgs.
type Subscriptions interface {
	All(ctx context.Context) ([]Subscription, error)
}

// SubscriptionsFunc adapts a function to Subscriptions.
type SubscriptionsFunc func(ctx context.Context) ([]Subscription, error)

// All implements Subscriptions.
func (f SubscriptionsFunc) All(ctx context.Context) ([]Subscription, error) { return f(ctx) }

// NoSubscriptions is the source while no trigger type subscribes to code-host resources: the
// code_host_event trigger filter is not defined yet (templates.ValidateTriggers refuses the type),
// so a running poller has no rows.
var NoSubscriptions = SubscriptionsFunc(func(context.Context) ([]Subscription, error) { return nil, nil })

// Opener opens the poller of a resource through a stored connection. provider is the connector
// type ID, for the floor and the metric label.
type Opener interface {
	OpenPoller(ctx context.Context, q *store.Queries, org, conn pgtype.UUID, res codehost.Resource) (p codehost.Poller, provider string, err error)
}

// Event is the input of the event workflow: one new item of one resource.
type Event struct {
	Org        string `json:"org"`
	Connection string `json:"connection"`
	Resource   string `json:"resource"`
	Provider   string `json:"provider"`
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	// At is when the provider says the item happened, Unix seconds.
	At      int64  `json:"at"`
	Payload []byte `json:"payload,omitempty"`
}

// DedupeKey is the durable.SourcePoll key of the event: stable at the provider.
func (e Event) DedupeKey() string { return DedupeKey(e.Connection, e.Resource, e.Key) }

// DedupeKey is the durable.SourcePoll key of an item of a resource polled through a connection.
// A resource too long for the key column is replaced by its hash.
func DedupeKey(conn, resource, item string) string {
	k := conn + "/" + resource + "#" + item
	if len(k) <= maxDedupeKey {
		return k
	}
	sum := sha256.Sum256([]byte(resource))
	return conn + "/" + hex.EncodeToString(sum[:16]) + "#" + item
}

// Deliverer hands an event to the subscriptions it matched. It runs in the dedupe transaction (q
// is bound to it) and returns what it produced, as a durable.Effect does.
type Deliverer interface {
	Deliver(ctx context.Context, q *store.Queries, ev Event, subs []Subscription) (kind string, id pgtype.UUID, err error)
}

// Config configures a Poller. Zero values take the defaults.
type Config struct {
	// Floor is the shortest interval for a provider without an entry in Floors.
	Floor time.Duration
	// Floors are per provider (connector type ID). An entry never lowers the interval below what
	// the provider asks for.
	Floors map[string]time.Duration
	Lease  time.Duration
	Resync time.Duration
	Batch  int
	// Now is the clock; nil is time.Now.
	Now    func() time.Time
	Logger *slog.Logger
	// Jitter returns a number in [0, 1) that spreads error backoffs; nil is math/rand.
	Jitter func() float64
}

// Poller runs the shared pollers.
type Poller struct {
	cfg  Config
	pool *pgxpool.Pool
	q    *store.Queries
	work *durable.OrgWork
	subs Subscriptions
	open Opener
	log  *slog.Logger

	polls *prometheus.CounterVec

	bound   chan struct{}
	once    sync.Once
	deliver Deliverer

	mu sync.Mutex // one pass at a time in this process
}

// New returns a Poller enqueuing on work. Add Registrar to the durable runtime before Launch, call
// Bind once the connection opener and the deliverer exist, and Run after Bind.
func New(cfg Config, pool *pgxpool.Pool, work *durable.OrgWork, subs Subscriptions) *Poller {
	if cfg.Floor <= 0 {
		cfg.Floor = DefaultFloor
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.Resync <= 0 {
		cfg.Resync = DefaultResync
	}
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultBatch
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Jitter == nil {
		cfg.Jitter = rand.Float64
	}
	if subs == nil {
		subs = NoSubscriptions
	}
	return &Poller{
		cfg: cfg, pool: pool, q: store.New(pool), work: work, subs: subs, log: cfg.Logger,
		polls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_code_host_polls_total",
			Help: "Polls of code-host resources by the shared pollers, by provider and outcome (not_modified, changed, error, rate_limited).",
		}, []string{"provider", "outcome"}),
		bound: make(chan struct{}),
	}
}

// Collector is the poll counter, for a Prometheus registry.
func (p *Poller) Collector() prometheus.Collector { return p.polls }

// Bind sets the connection opener and the deliverer. An event workflow recovered at launch waits
// for it; a pass before it fails.
func (p *Poller) Bind(open Opener, d Deliverer) {
	p.once.Do(func() {
		p.open, p.deliver = open, d
		close(p.bound)
	})
}

// Registrar registers the event workflow for durable.Runtime.Register.
func (p *Poller) Registrar() durable.Registrar {
	return func(c dbos.Context) error {
		dbos.RegisterWorkflow(c, p.event, dbos.WithWorkflowName(EventWorkflow))
		return nil
	}
}

func (p *Poller) floor(provider string) time.Duration {
	if f, ok := p.cfg.Floors[provider]; ok && f > 0 {
		return f
	}
	return p.cfg.Floor
}

// Run polls while this replica holds the leader lock, until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	failures := 0
	for {
		led, err := p.lead(ctx)
		if ctx.Err() != nil {
			return
		}
		if led {
			failures = 1
		} else {
			failures++
		}
		delay := min(time.Second<<min(failures-1, 3), 8*time.Second)
		delay += time.Duration(p.cfg.Jitter() * float64(delay))
		p.log.Warn("code-host pollers: leader loop restarting", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// lead takes the leader lock on a dedicated connection and runs passes until the connection
// fails or ctx ends. The connection is checked before every pass: a leader that lost it (and so
// the lock) stops. A pass already running when the lock is lost may overlap the next leader's
// first pass; the claim lease, SKIP LOCKED and the dedupe key make that harmless.
func (p *Poller) lead(ctx context.Context) (led bool, _ error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer closeConn(conn)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", leaderLock); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("take leader lock: %w", err)
	}
	p.log.Info("code-host pollers: leading")
	for {
		if err := conn.Ping(ctx); err != nil {
			return true, fmt.Errorf("leader connection: %w", err)
		}
		next, err := p.PollDue(ctx, p.cfg.Now())
		if err != nil && ctx.Err() == nil {
			p.log.Warn("code-host pollers: pass incomplete", "err", err)
		}
		wait := p.cfg.Resync
		if !next.IsZero() {
			wait = min(wait, max(time.Until(next), minWake))
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func closeConn(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Conn().Close(ctx)
	conn.Release()
}

// PollDue runs one pass at now: it syncs the rows with the subscriptions in effect, polls every
// due row and returns the earliest next_poll_at (zero when there are no rows).
func (p *Poller) PollDue(ctx context.Context, now time.Time) (time.Time, error) {
	select {
	case <-p.bound:
	default:
		return time.Time{}, errors.New("code-host pollers: pass before Bind")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	orgs, err := p.sync(ctx, now)
	if err != nil {
		return time.Time{}, err
	}
	var errs []error
	for _, org := range orgs {
		if err := p.pollOrg(ctx, org, now); err != nil {
			errs = append(errs, err)
		}
	}
	next, err := p.q.NextCodeHostPollAt(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("next poll: %w", err))
	}
	return next.Time, errors.Join(errs...)
}

// sync creates the rows the subscriptions need and deletes the ones none needs, and returns the
// orgs that have rows.
func (p *Poller) sync(ctx context.Context, now time.Time) ([]pgtype.UUID, error) {
	subs, err := p.subs.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	want := map[pgtype.UUID]map[string]Subscription{}
	for _, s := range subs {
		if want[s.Org] == nil {
			want[s.Org] = map[string]Subscription{}
		}
		want[s.Org][s.keep()] = s
	}
	have, err := p.q.ListOrgIDsWithCodeHostPollers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	orgs := map[pgtype.UUID]bool{}
	for _, o := range have {
		orgs[o] = true
	}
	for o := range want {
		orgs[o] = true
	}
	var out []pgtype.UUID
	for org := range orgs {
		keep := make([]string, 0, len(want[org]))
		for k, s := range want[org] {
			keep = append(keep, k)
			if err := p.q.EnsureCodeHostPoller(ctx, store.EnsureCodeHostPollerParams{
				OrgID: org, ConnectionID: s.Connection, Resource: s.Resource.String(), Now: ts(now),
			}); err != nil {
				return nil, fmt.Errorf("ensure poller %s: %w", k, err)
			}
		}
		if _, err := p.q.DeleteCodeHostPollersNotIn(ctx, store.DeleteCodeHostPollersNotInParams{OrgID: org, Keep: keep}); err != nil {
			return nil, fmt.Errorf("delete pollers: %w", err)
		}
		if len(keep) > 0 {
			out = append(out, org)
		}
	}
	return out, nil
}

func (p *Poller) pollOrg(ctx context.Context, org pgtype.UUID, now time.Time) error {
	for {
		rows, err := p.q.ClaimDueCodeHostPollers(ctx, store.ClaimDueCodeHostPollersParams{
			OrgID: org, Now: ts(now), LeaseUntil: ts(now.Add(p.cfg.Lease)), MaxRows: int32(p.cfg.Batch), //nolint:gosec // a small config value
		})
		if err != nil {
			return fmt.Errorf("claim pollers of org %s: %w", org.String(), err)
		}
		for _, row := range rows {
			if err := p.pollOne(ctx, row, now); err != nil {
				return err
			}
		}
		if len(rows) < p.cfg.Batch {
			return nil
		}
	}
}

// pollOne polls one claimed row and stores the outcome. It returns an error only when the outcome
// could not be stored; the row's lease then retries it.
func (p *Poller) pollOne(ctx context.Context, row store.CodeHostPoller, now time.Time) error {
	save := store.SaveCodeHostPollParams{
		OrgID: row.OrgID, ID: row.ID,
		ValidatorKind: row.ValidatorKind, Validator: row.Validator, Cursor: row.Cursor,
		ProviderIntervalS: row.ProviderIntervalS, ConsecutiveFailures: row.ConsecutiveFailures,
		PolledAt: ts(now),
	}
	provider, outcome, err := p.poll(ctx, row, now, &save)
	if err != nil {
		var rl *codehost.RateLimitError
		if errors.As(err, &rl) {
			outcome = OutcomeRateLimited
			wait := min(max(rl.RetryAt.Sub(now), p.floor(provider)), MaxBackoff)
			save.NextPollAt, save.BackoffUntil = ts(now.Add(wait)), ts(now.Add(wait))
		} else {
			outcome = OutcomeError
			save.ConsecutiveFailures++
			wait := max(p.backoff(int(save.ConsecutiveFailures), err), p.floor(provider))
			save.NextPollAt, save.BackoffUntil = ts(now.Add(wait)), ts(now.Add(wait))
		}
		save.LastError = truncate(err.Error(), maxErrorText)
		p.log.Warn("code-host pollers: poll failed", "org", row.OrgID.String(), "connection", row.ConnectionID.String(),
			"resource", row.Resource, "outcome", outcome, "next", save.NextPollAt.Time, "err", err)
	}
	save.LastStatus = outcome
	if provider == "" {
		provider = "unknown"
	}
	p.polls.WithLabelValues(provider, outcome).Inc()
	if err := p.q.SaveCodeHostPoll(ctx, save); err != nil {
		return fmt.Errorf("save poll of %s: %w", row.Resource, err)
	}
	return nil
}

// poll does the provider's part of one poll and fills save for a successful one.
func (p *Poller) poll(ctx context.Context, row store.CodeHostPoller, now time.Time, save *store.SaveCodeHostPollParams) (provider, outcome string, _ error) {
	res, err := codehost.ParseResource(row.Resource)
	if err != nil {
		return "", "", err
	}
	poller, provider, err := p.open.OpenPoller(ctx, p.q, row.OrgID, row.ConnectionID, res)
	if err != nil {
		return provider, "", fmt.Errorf("open: %w", err)
	}
	st := codehost.PollState{
		Validator: codehost.Validator{Kind: row.ValidatorKind, Value: row.Validator},
		Cursor:    row.Cursor, Baselined: row.BaselineAt.Valid,
	}
	got, err := poller.Poll(ctx, res, st)
	if err != nil {
		return provider, "", err
	}
	if got.Interval > 0 {
		save.ProviderIntervalS = int32(min(got.Interval, MaxBackoff) / time.Second)
	}
	interval := max(p.floor(provider), time.Duration(save.ProviderIntervalS)*time.Second)
	save.NextPollAt = ts(now.Add(interval))
	save.ValidatorKind, save.Validator = got.State.Validator.Kind, got.State.Validator.Value
	if !got.Changed {
		save.ConsecutiveFailures = 0
		return provider, OutcomeNotModified, nil
	}
	// Every item is enqueued before the cursor moves: an enqueue that fails leaves the stored
	// validator and cursor as they were, so the next poll finds the same items again.
	for _, it := range got.Items {
		ev := Event{
			Org: row.OrgID.String(), Connection: row.ConnectionID.String(), Resource: row.Resource, Provider: provider,
			Key: it.Key, Kind: it.Kind, At: it.At.Unix(), Payload: it.Payload,
		}
		if _, err := durable.EnqueueForOrg[string](ctx, p.work, ev.Org, EventWorkflow, ev,
			dbos.WithEnqueueWorkflowID("code-host-poll:"+ev.Org+":"+ev.DedupeKey())); err != nil {
			save.ValidatorKind, save.Validator = row.ValidatorKind, row.Validator
			return provider, "", fmt.Errorf("enqueue %s: %w", it.Key, err)
		}
	}
	save.Cursor = got.State.Cursor
	save.ConsecutiveFailures = 0
	if got.State.Baselined && !row.BaselineAt.Valid {
		save.BaselineAt = ts(now)
	}
	return provider, OutcomeChanged, nil
}

// backoff is the wait after the n-th consecutive failed poll: a minute doubled per failure up
// to MaxBackoff, plus up to a tenth as jitter. A credential the host refuses, or a resource it
// does not have, starts four times higher: polling it faster does not fix it.
func (p *Poller) backoff(n int, err error) time.Duration {
	d := errorBackoffBase
	if codehost.IsStatus(err, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound) ||
		errors.Is(err, codehost.ErrResourceUnsupported) {
		d *= 4
	}
	for i := 1; i < n && d < MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, MaxBackoff)
	return d + time.Duration(p.cfg.Jitter()*float64(d)/10)
}

const stepDeliver = "deliver"

// event is the workflow of one polled item.
func (p *Poller) event(c dbos.Context, ev Event) (string, error) {
	select {
	case <-p.bound:
	case <-c.Done():
		return "", c.Err()
	}
	return dbos.RunAsStep(c, func(ctx context.Context) (string, error) { return p.deliverOnce(ctx, ev) },
		dbos.WithStepName(stepDeliver), dbos.WithStepMaxRetries(5))
}

// deliverOnce delivers ev to the subscriptions in effect now, once per dedupe key.
func (p *Poller) deliverOnce(ctx context.Context, ev Event) (string, error) {
	var org, conn pgtype.UUID
	if err := org.Scan(ev.Org); err != nil {
		return "", fmt.Errorf("org: %w", err)
	}
	if err := conn.Scan(ev.Connection); err != nil {
		return "", fmt.Errorf("connection: %w", err)
	}
	all, err := p.subs.All(ctx)
	if err != nil {
		return "", fmt.Errorf("subscriptions: %w", err)
	}
	var subs []Subscription
	for _, s := range all {
		if s.Org == org && s.Connection == conn && s.Resource.String() == ev.Resource {
			subs = append(subs, s)
		}
	}
	var out durable.Outcome
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		out, err = durable.Once(ctx, store.New(tx), org, durable.SourcePoll, ev.DedupeKey(), func(q *store.Queries) (string, pgtype.UUID, error) {
			if len(subs) == 0 {
				return "", pgtype.UUID{}, nil // the triggers went away since the poll
			}
			return p.deliver.Deliver(ctx, q, ev, subs)
		})
		return err
	})
	if err != nil {
		return "", err
	}
	if out.EffectID.Valid {
		return out.EffectID.String(), nil
	}
	return "", nil
}

// ConnectionOpener opens pollers through stored code-host connections.
type ConnectionOpener struct {
	Hosts *codehost.Registry
	Keys  connections.Opener
}

// OpenPoller implements Opener with the provider's least scope for the resource.
func (o ConnectionOpener) OpenPoller(ctx context.Context, q *store.Queries, org, connID pgtype.UUID, res codehost.Resource) (codehost.Poller, string, error) {
	row, err := q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: connID})
	if err != nil {
		return nil, "", fmt.Errorf("connection %s: %w", connID.String(), err)
	}
	prov, ok := o.Hosts.Get(row.Connector)
	if !ok {
		return nil, row.Connector, fmt.Errorf("%w: connector %s", codehost.ErrResourceUnsupported, row.Connector)
	}
	scoper, ok := prov.(codehost.PollScoper)
	if !ok {
		return nil, row.Connector, fmt.Errorf("%w: connector %s", codehost.ErrResourceUnsupported, row.Connector)
	}
	scope, err := scoper.PollScope(res)
	if err != nil {
		return nil, row.Connector, err
	}
	c, err := connections.Load(ctx, o.Keys, row)
	if err == nil {
		err = prov.Validate(c)
	}
	if err != nil {
		return nil, row.Connector, fmt.Errorf("load connection: %w", err)
	}
	acc, err := prov.Open(ctx, c, scope)
	if err != nil {
		return nil, row.Connector, err
	}
	poller, ok := acc.(codehost.Poller)
	if !ok {
		return nil, row.Connector, fmt.Errorf("%w: connector %s", codehost.ErrResourceUnsupported, row.Connector)
	}
	return poller, row.Connector, nil
}

// LogDeliverer logs each event and produces nothing: the deliverer until code_host_event
// triggers create tasks.
type LogDeliverer struct{ Logger *slog.Logger }

// Deliver implements Deliverer.
func (d LogDeliverer) Deliver(_ context.Context, _ *store.Queries, ev Event, subs []Subscription) (string, pgtype.UUID, error) {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Info("code-host pollers: event", "org", ev.Org, "connection", ev.Connection, "resource", ev.Resource,
		"kind", ev.Kind, "key", ev.Key, "subscriptions", len(subs))
	return "", pgtype.UUID{}, nil
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
