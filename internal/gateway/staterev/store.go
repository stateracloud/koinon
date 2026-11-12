// SPDX-License-Identifier: Apache-2.0

package staterev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
)

// NotifyChannel is the Postgres channel a publish notifies, with the new revision as payload (a
// number, nothing secret). The stream server of a later slice LISTENs on it; the payload is a hint,
// the table is the truth.
const NotifyChannel = "koinon_gateway_state"

// DefaultKeep is how many revisions the table keeps. A gateway copy needs only the latest; the rest
// is history for an operator and for the ack check of a copy that is a few revisions behind.
const DefaultKeep = 100

// MaxAge is how long a revision stays (the newest one always does). A snapshot holds org names and
// URLs, so this also bounds how long a deleted org's data stays in the table.
const MaxAge = 30 * 24 * time.Hour

// DB is what Publisher needs of the pool: a transaction. *pgxpool.Pool satisfies it.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Publisher stores revisions of the gateway State.
type Publisher struct {
	DB DB
	// Keep is the number of revisions kept after each publish; zero means DefaultKeep.
	Keep int
}

// Published says what Publish did.
type Published struct {
	// Revision is the stored revision that now holds s: the new one, or the latest when nothing
	// changed.
	Revision int64
	// Changed is true when this call stored a new revision.
	Changed bool
}

// Publish stores s as the next revision and notifies, only when its snapshot differs from the latest
// stored one. It is one transaction: the insert, the notify (delivered on commit) and the retention
// delete happen together or not at all.
func (p Publisher) Publish(ctx context.Context, s gateway.State) (Published, error) {
	snap := Snapshot(s)
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(snap)
	if err != nil {
		return Published{}, fmt.Errorf("staterev: encode the snapshot: %w", err)
	}
	// The deterministic encoding is stable within one build of the protobuf library; a library upgrade
	// can cost one extra revision, never a missed one.
	enc, err := proto.MarshalOptions{Deterministic: true}.Marshal(snap)
	if err != nil {
		return Published{}, fmt.Errorf("staterev: encode the snapshot: %w", err)
	}
	sum := sha256.Sum256(enc)
	digest := hex.EncodeToString(sum[:])
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return Published{}, fmt.Errorf("staterev: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.LockGatewayStatePublish(ctx); err != nil {
		return Published{}, fmt.Errorf("staterev: lock: %w", err)
	}
	latest, err := q.GetLatestGatewayStateRevision(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Published{}, fmt.Errorf("staterev: read the latest revision: %w", err)
	default:
		if latest.Digest == digest {
			// Unchanged: no new revision, but retention still runs, so a quiet install does not keep
			// revisions past the ADR 0097 age bound. The newest is never pruned.
			if err := p.prune(ctx, q); err != nil {
				return Published{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return Published{}, fmt.Errorf("staterev: commit: %w", err)
			}
			return Published{Revision: latest.Revision}, nil
		}
	}
	rev, err := q.InsertGatewayStateRevision(ctx, store.InsertGatewayStateRevisionParams{Snapshot: body, Digest: digest})
	if err != nil {
		return Published{}, fmt.Errorf("staterev: insert: %w", err)
	}
	if err := q.NotifyGatewayStateRevision(ctx, strconv.FormatInt(rev, 10)); err != nil {
		return Published{}, fmt.Errorf("staterev: notify: %w", err)
	}
	if err := p.prune(ctx, q); err != nil {
		return Published{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Published{}, fmt.Errorf("staterev: commit: %w", err)
	}
	return Published{Revision: rev, Changed: true}, nil
}

func (p Publisher) prune(ctx context.Context, q *store.Queries) error {
	keep := p.Keep
	if keep <= 0 {
		keep = DefaultKeep
	}
	if _, err := q.PruneGatewayStateRevisions(ctx, store.PruneGatewayStateRevisionsParams{Keep: int32(keep), MaxAgeSeconds: int32(MaxAge / time.Second)}); err != nil {
		return fmt.Errorf("staterev: retention: %w", err)
	}
	return nil
}

// Latest returns the newest stored revision as a StateUpdate with its revision number, or nil when
// none was stored yet.
func Latest(ctx context.Context, db store.DBTX) (*gwv1.StateUpdate, error) {
	row, err := store.New(db).GetLatestGatewayStateRevision(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("staterev: read the latest revision: %w", err)
	}
	var u gwv1.StateUpdate
	dec := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := dec.Unmarshal(row.Snapshot, &u); err != nil {
		return nil, fmt.Errorf("staterev: decode revision %d: %w", row.Revision, err)
	}
	u.Revision = row.Revision
	return &u, nil
}

// halfName is the value stored in gateway_state_acks.half.
func halfName(h gwv1.Half) (string, error) {
	switch h {
	case gwv1.Half_HALF_MODEL:
		return "model", nil
	case gwv1.Half_HALF_MCP:
		return "mcp", nil
	}
	return "", fmt.Errorf("staterev: unknown half %v", h)
}

// Ack records that copy serves half of revision (and so every earlier revision of that half). It is
// idempotent. An unknown revision (pruned, or never published) is an error from the foreign key.
func Ack(ctx context.Context, db store.DBTX, copyID string, revision int64, half gwv1.Half) error {
	h, err := halfName(half)
	if err != nil {
		return err
	}
	if copyID == "" {
		return errors.New("staterev: ack without a copy id")
	}
	if err := store.New(db).AckGatewayStateRevision(ctx, store.AckGatewayStateRevisionParams{CopyID: copyID, Revision: revision, Half: h}); err != nil {
		return fmt.Errorf("staterev: ack: %w", err)
	}
	return nil
}

// AllAcked reports whether every one of the live gateway copies confirmed half at revision or a
// later one. The caller names the live copies (it knows who is running); the table only knows who
// confirmed. Copy ids are unique per process incarnation (see the proto comment). No copies gives
// false: "nobody is live" must never read as "everybody has it", so S4 must handle "no live copy"
// explicitly (for example serve and proceed, or fail) and must not wait on a false that cannot change.
func AllAcked(ctx context.Context, db store.DBTX, revision int64, half gwv1.Half, liveCopies []string) (bool, error) {
	h, err := halfName(half)
	if err != nil {
		return false, err
	}
	uniq := map[string]struct{}{}
	for _, c := range liveCopies {
		uniq[c] = struct{}{}
	}
	if len(uniq) == 0 {
		return false, nil
	}
	ids := make([]string, 0, len(uniq))
	for c := range uniq {
		ids = append(ids, c)
	}
	n, err := store.New(db).CountGatewayCopiesAcked(ctx, store.CountGatewayCopiesAckedParams{Half: h, Revision: revision, CopyIds: ids})
	if err != nil {
		return false, fmt.Errorf("staterev: count acks: %w", err)
	}
	return n == int64(len(ids)), nil
}
