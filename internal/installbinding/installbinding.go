// SPDX-License-Identifier: Apache-2.0

// Package installbinding records which install a database belongs to (ADR 0110 decision 0, issue
// #516) and lets a restored copy tell that it is one.
//
// The install mark (ADR 0106) is a random value in a Secret that never enters Postgres; only its
// hash, execution.OwnerID, does. A one-row table, install_binding, holds the hash of the mark of the
// install that last ran unheld on this database. At start each binary compares its own hash with the
// row and lands in one of three states:
//
//   - same: the row carries this process's mark. Normal.
//   - copy: the row carries another mark, or no mark (an unbound row). The database came from
//     somewhere else. API tokens and sessions issued by the other install are refused, and the row is
//     never overwritten automatically; [Adopt] is the one way back to same.
//   - unknown: no row. A held start writes an unbound row (so the state becomes copy and stays copy
//     after the release); an unheld start is an upgrade of a live install and claims the row.
//
// The package is a leaf: it imports no auth or token code, so both can use it.
package installbinding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// State is how this process's mark relates to the install_binding row.
type State string

// The states. The set is fixed: it labels the metric koinon_install_binding (ADR 0023).
const (
	StateSame    State = "same"
	StateCopy    State = "copy"
	StateUnknown State = "unknown"
)

// States lists every State.
var States = []State{StateSame, StateCopy, StateUnknown}

// ReassertInterval is how often a held process re-asserts the hold (ADR 0110 decision 0). It is a
// constant, the same in all three binaries.
const ReassertInterval = 30 * time.Second

// stateTTL bounds how stale a cached state read is. It is short because an adopt-restore must take
// effect on a running api without a restart.
const stateTTL = 5 * time.Second

// QuarantineReason is why authentication refuses a credential of another install. Callers show it.
const QuarantineReason = "issued by another install (restored copy)"

// Targets are the outward targets of an install (ADR 0110 decision 0), each as scheme://host[:port]
// ([Origin]) or the checkpoint endpoint and bucket. An empty field is not recorded (NULL).
type Targets struct {
	PublicURL          string
	OIDCRedirectURL    string
	CheckpointEndpoint string
	CheckpointBucket   string
}

// Origin returns raw as scheme://host[:port] and nothing more: user info, path, query and fragment
// could carry a credential and do not tell two installs apart. An empty or unparsable raw gives "".
func Origin(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Row is the install_binding row.
type Row struct {
	// Mark is the hash of the binding install's mark; Bound is false for an unbound row (NULL).
	Mark       string
	Bound      bool
	Targets    Targets
	HeldSince  *time.Time
	ReleasedAt *time.Time
	ReleasedBy string
}

// Conflict is one outward target of this install that equals the recorded one of the binding
// install, so a copy would act on the source's target (decision 4).
type Conflict struct {
	// Target is one of "checkpoints", "public-url", "oidc-redirect" (the metric label values).
	Target string `json:"target"`
	// Value is the shared value.
	Value string `json:"value"`
}

// Conflicts lists the targets in own that equal the same non-empty target in recorded.
func Conflicts(recorded, own Targets) []Conflict {
	var out []Conflict
	if own.CheckpointEndpoint != "" && own.CheckpointEndpoint == recorded.CheckpointEndpoint &&
		own.CheckpointBucket == recorded.CheckpointBucket {
		out = append(out, Conflict{Target: "checkpoints", Value: own.CheckpointEndpoint + "/" + own.CheckpointBucket})
	}
	if own.PublicURL != "" && own.PublicURL == recorded.PublicURL {
		out = append(out, Conflict{Target: "public-url", Value: own.PublicURL})
	}
	if own.OIDCRedirectURL != "" && own.OIDCRedirectURL == recorded.OIDCRedirectURL {
		out = append(out, Conflict{Target: "oidc-redirect", Value: own.OIDCRedirectURL})
	}
	return out
}

// Scope says which outward-target columns a binary owns and may refresh (ADR 0110 decision 0: each
// binary writes only its own columns).
type Scope uint8

const (
	// ScopeEdge is the public URL and the OIDC redirect URL: koinon-api's.
	ScopeEdge Scope = 1 << iota
	// ScopeCheckpoints is the checkpoint endpoint and bucket: koinon-agentd's.
	ScopeCheckpoints
)

// Binding is one process's view of the install_binding row. A nil Binding, or one made with an empty
// mark (a development run, a test), is disabled: it refuses nothing and writes nothing.
type Binding struct {
	pool    *pgxpool.Pool
	mark    string
	held    bool
	scope   Scope
	started time.Time
	now     func() time.Time
	log     *slog.Logger

	mu      sync.Mutex
	state   State
	at      time.Time
	own     Targets
	claimed bool
	// followUp is set while the steps after the row read (refreshTargets, stampLegacy) have not all
	// succeeded; Run repeats Claim until it clears. Both steps are idempotent.
	followUp bool

	quarantined *prometheus.CounterVec

	// sessionsHeld is the session gate of [Binding.HoldSessions].
	sessionsHeld atomic.Bool
}

// New returns a Binding for a process whose mark hashes to ownerID (execution.OwnerID of the install
// mark; "" disables the binding). held is the process's restore-hold flag.
func New(pool *pgxpool.Pool, ownerID string, held bool, log *slog.Logger) *Binding {
	if log == nil {
		log = slog.Default()
	}
	return &Binding{pool: pool, mark: ownerID, held: held, started: time.Now(), now: time.Now, log: log}
}

// WithScope sets the target columns this binary refreshes in state same. The default is none: a
// binary that owns no outward target only claims the row and re-asserts the hold. It returns b.
func (b *Binding) WithScope(s Scope) *Binding {
	b.scope = s
	return b
}

// Enabled says whether the binding refuses and writes anything.
func (b *Binding) Enabled() bool { return b != nil && b.mark != "" }

// Mark is the hash this process stamps on the credentials it issues; "" when disabled.
func (b *Binding) Mark() string {
	if !b.Enabled() {
		return ""
	}
	return b.mark
}

// Stamp is [Binding.Mark] as the nullable column value of a new token or session.
func (b *Binding) Stamp() pgtype.Text {
	m := b.Mark()
	return pgtype.Text{String: m, Valid: m != ""}
}

// readRow reads the row; found is false when there is none.
func readRow(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, lock bool,
) (Row, bool, error) {
	sql := `SELECT mark, public_url, oidc_redirect_url, checkpoint_endpoint, checkpoint_bucket,
	               held_since, released_at, released_by
	        FROM install_binding WHERE singleton`
	if lock {
		sql += " FOR UPDATE"
	}
	var (
		mark, pub, oidc, cpe, cpb, by pgtype.Text
		held, rel                     pgtype.Timestamptz
	)
	err := q.QueryRow(ctx, sql).Scan(&mark, &pub, &oidc, &cpe, &cpb, &held, &rel, &by)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	r := Row{
		Mark: mark.String, Bound: mark.Valid, ReleasedBy: by.String,
		Targets: Targets{PublicURL: pub.String, OIDCRedirectURL: oidc.String, CheckpointEndpoint: cpe.String, CheckpointBucket: cpb.String},
	}
	if held.Valid {
		t := held.Time
		r.HeldSince = &t
	}
	if rel.Valid {
		t := rel.Time
		r.ReleasedAt = &t
	}
	return r, true, nil
}

// StateOf computes the state of ownerID against a row; found is false when there is no row.
func StateOf(ownerID string, r Row, found bool) State {
	switch {
	case !found:
		return StateUnknown
	case r.Bound && r.Mark == ownerID:
		return StateSame
	}
	return StateCopy
}

// Read returns the row and this process's state against it, uncached.
func (b *Binding) Read(ctx context.Context) (Row, State, error) {
	r, found, err := readRow(ctx, b.pool, false)
	if err != nil {
		return Row{}, "", fmt.Errorf("read install_binding: %w", err)
	}
	st := StateOf(b.mark, r, found)
	b.setState(st)
	return r, st, nil
}

func (b *Binding) setState(st State) {
	b.mu.Lock()
	b.state, b.at = st, b.now()
	b.mu.Unlock()
}

// State returns the state, cached for a few seconds. A disabled Binding is StateSame.
func (b *Binding) State(ctx context.Context) (State, error) {
	if !b.Enabled() {
		return StateSame, nil
	}
	b.mu.Lock()
	st, at := b.state, b.at
	b.mu.Unlock()
	if st != "" && b.now().Sub(at) < stateTTL {
		return st, nil
	}
	_, st, err := b.Read(ctx)
	return st, err
}

// Claim runs at start. It inserts the row if there is none (INSERT ... ON CONFLICT DO NOTHING) and
// then re-reads it, so two processes that start at once agree on one row and compute the state from
// what is stored, not from what they tried to write (ADR 0110 decision 0). A held process inserts an
// unbound row (mark NULL) with held_since set; an unheld one inserts a row bound to its own mark.
// Only state "same" refreshes targets, and only an unheld process does it.
func (b *Binding) Claim(ctx context.Context, own Targets) (State, error) {
	if !b.Enabled() {
		return StateSame, nil
	}
	b.mu.Lock()
	b.own = own
	b.mu.Unlock()
	if err := b.insertIfAbsent(ctx); err != nil {
		return "", err
	}
	if b.held {
		// A held process checks at start, not only after the first interval (decision 0).
		if err := b.setHeldSince(ctx); err != nil {
			return "", err
		}
	}
	_, st, err := b.Read(ctx)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.claimed = true
	b.followUp = !b.held && st == StateSame
	b.mu.Unlock()
	if !b.held && st == StateSame {
		if err := b.refreshTargets(ctx, own); err != nil {
			return st, err
		}
		if err := b.stampLegacy(ctx); err != nil {
			return st, err
		}
		b.mu.Lock()
		b.followUp = false
		b.mu.Unlock()
	}
	if st == StateCopy {
		b.log.Warn("install binding: this database was bound to another install; its API tokens and sessions are refused until a platform admin runs adopt-restore or rebinds a token",
			"state", st, "held", b.held)
	}
	return st, nil
}

func (b *Binding) insertIfAbsent(ctx context.Context) error {
	var err error
	if b.held {
		_, err = b.pool.Exec(ctx, `INSERT INTO install_binding (singleton, mark, held_since) VALUES (true, NULL, now()) ON CONFLICT DO NOTHING`)
	} else {
		_, err = b.pool.Exec(ctx, `INSERT INTO install_binding (singleton, mark) VALUES (true, $1) ON CONFLICT DO NOTHING`, b.mark)
	}
	if err != nil {
		return fmt.Errorf("claim install_binding: %w", err)
	}
	return nil
}

// refreshTargets writes the outward targets this binary owns ([Scope]); a column of another binary is
// not touched. The write is guarded by the state: it matches only a row bound to this process's mark.
func (b *Binding) refreshTargets(ctx context.Context, own Targets) error {
	if b.scope&ScopeEdge != 0 {
		if _, err := b.pool.Exec(ctx,
			`UPDATE install_binding SET public_url = NULLIF($2, ''), oidc_redirect_url = NULLIF($3, '') WHERE singleton AND mark = $1`,
			b.mark, own.PublicURL, own.OIDCRedirectURL); err != nil {
			return fmt.Errorf("refresh install_binding targets: %w", err)
		}
	}
	if b.scope&ScopeCheckpoints != 0 {
		if _, err := b.pool.Exec(ctx,
			`UPDATE install_binding SET checkpoint_endpoint = NULLIF($2, ''), checkpoint_bucket = NULLIF($3, '') WHERE singleton AND mark = $1`,
			b.mark, own.CheckpointEndpoint, own.CheckpointBucket); err != nil {
			return fmt.Errorf("refresh install_binding targets: %w", err)
		}
	}
	return nil
}

// stampLegacy marks tokens and sessions that predate the binding as this install's own. It runs only
// in state same on an unheld start, as ADR 0106 claims unmarked objects. In copy and unknown they stay
// unmarked and are refused.
func (b *Binding) stampLegacy(ctx context.Context) error {
	for _, table := range []string{"api_tokens", "sessions"} {
		if _, err := b.pool.Exec(ctx, `UPDATE `+table+` SET install_mark = $1 WHERE install_mark IS NULL`, b.mark); err != nil {
			return fmt.Errorf("stamp %s: %w", table, err)
		}
	}
	return nil
}

// Reassert is one check of a held process (decision 0): it writes the unbound row when the row is
// absent (a load of a dump replaced the table) and sets held_since when it is NULL and this process
// started after released_at, so an old held pod cannot set it again after the release.
func (b *Binding) Reassert(ctx context.Context) error {
	if !b.Enabled() || !b.held {
		return nil
	}
	if err := b.insertIfAbsent(ctx); err != nil {
		return err
	}
	if err := b.setHeldSince(ctx); err != nil {
		return err
	}
	_, _, err := b.Read(ctx)
	return err
}

// setHeldSince sets held_since when it is NULL and this process started after released_at.
func (b *Binding) setHeldSince(ctx context.Context) error {
	if _, err := b.pool.Exec(ctx,
		`UPDATE install_binding SET held_since = now()
		 WHERE singleton AND held_since IS NULL AND (released_at IS NULL OR released_at < $1)`, b.started); err != nil {
		return fmt.Errorf("re-assert hold: %w", err)
	}
	return nil
}

func (b *Binding) isClaimed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.claimed
}

// ClaimRetryInterval is how often Run retries a claim that failed at start.
const ClaimRetryInterval = 5 * time.Second

// Run re-asserts the hold every [ReassertInterval] until ctx ends. A process that is not held only
// refreshes its state (so the gauge follows an adopt-restore); a disabled Binding returns at once.
func (b *Binding) Run(ctx context.Context) {
	if !b.Enabled() {
		return
	}
	t := time.NewTicker(ReassertInterval)
	defer t.Stop()
	retry := time.NewTicker(ClaimRetryInterval)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			// The start-time claim failed (the migration Job has not created install_binding yet, or the
			// database was away). Keep trying; until it succeeds Allows fails closed.
			b.mu.Lock()
			claimed, own, followUp := b.claimed, b.own, b.followUp
			b.mu.Unlock()
			if claimed && !followUp {
				continue
			}
			if _, err := b.Claim(ctx, own); err != nil {
				if ctx.Err() == nil {
					b.log.Warn("install binding: claim not yet possible, retrying", "err", err)
				}
			} else {
				b.log.Info("install binding: claimed", "held", b.held)
			}
		case <-t.C:
			var err error
			if !b.isClaimed() {
				continue
			}
			if b.held {
				err = b.Reassert(ctx)
			} else {
				_, _, err = b.Read(ctx)
			}
			if err != nil && ctx.Err() == nil {
				b.log.Warn("install binding: could not refresh", "held", b.held, "err", err)
			}
		}
	}
}

// Allows says whether a credential stamped with rowMark may be used by this process: its own mark
// always, a NULL mark only in state same. A disabled Binding allows everything.
func (b *Binding) Allows(ctx context.Context, rowMark pgtype.Text) (bool, error) {
	if !b.Enabled() {
		return true, nil
	}
	if rowMark.Valid {
		return rowMark.String == b.mark, nil
	}
	st, err := b.State(ctx)
	if err != nil {
		return false, err
	}
	return st == StateSame, nil
}

// Refused counts one refused credential (kind: token or session) in koinon_auth_quarantined_total.
func (b *Binding) Refused(kind string) {
	if b != nil && b.quarantined != nil {
		b.quarantined.WithLabelValues(kind).Inc()
	}
}

// Metrics registers koinon_install_binding{state} (1 for the current state, 0 for the others) and
// koinon_auth_quarantined_total{kind} on reg. Both label sets are fixed (ADR 0023).
func (b *Binding) Metrics(reg prometheus.Registerer) {
	b.quarantined = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "koinon_auth_quarantined_total",
		Help: "API tokens and sessions refused because another install issued them (a restored copy), by kind.",
	}, []string{"kind"})
	for _, k := range []string{"token", "session"} {
		b.quarantined.WithLabelValues(k)
	}
	reg.MustRegister(b.quarantined)
	b.StateMetric(reg)
}

// StateMetric registers koinon_install_binding{state} alone, for the binaries that refuse nothing.
func (b *Binding) StateMetric(reg prometheus.Registerer) {
	for _, st := range States {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "koinon_install_binding",
			Help:        "1 for the state of this process against the install_binding row (same, copy, unknown), else 0.",
			ConstLabels: prometheus.Labels{"state": string(st)},
		}, func() float64 {
			if !b.Enabled() {
				return 0
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.state == st {
				return 1
			}
			return 0
		}))
	}
}
