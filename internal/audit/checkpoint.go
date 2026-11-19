// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit/internal/auditstore"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Signed checkpoints (ADR 0100 decision 4.2). After each anchor, koinon-api signs the install chain row of
// that anchor with an Ed25519 key only it can read, and records the signed note as an audit.checkpoint
// install event. The anchor commits to every org head and the install head commits to the anchor, so one
// checkpoint, kept outside the cluster, covers every chain up to it: someone who rewrites any chain, the
// install chain included, can no longer produce a history that matches it. A compromised koinon-api can
// still sign a false checkpoint for the future; external witnesses or an RFC 3161 timestamp are a later step.

const (
	// ActionCheckpoint is the action of a checkpoint event in the install chain.
	ActionCheckpoint = "audit.checkpoint"
	// ActionSigningKeyAdded is the action of the event that publishes a new signing key's public half.
	ActionSigningKeyAdded = "audit.signing_key.added"
	// SigningKeyTargetKind is the target kind of an audit.signing_key.added event; the target ID is the key ID.
	SigningKeyTargetKind = "audit_signing_key"
)

// KeySource gives the signing key of one install. koinon-api reads it from a Kubernetes Secret and creates
// the Secret on its first start; a test gives a fixed key. The name is NoteOrigin of the install: a key
// speaks for one install.
type KeySource interface {
	SigningKey(ctx context.Context, name string) (*SigningKey, error)
}

// checkpointAfter is the "after" part of an audit.checkpoint event's summary.
type checkpointAfter struct {
	// AnchorSeq and Head (hex) name the install chain row that was signed: the anchor event.
	AnchorSeq int64  `json:"anchor_seq"`
	Head      string `json:"head"`
	KeyID     string `json:"key_id"`
	// Note is the whole signed note, as it is exported (a public value).
	Note string `json:"note"`
}

// signingKeyAfter is the "after" part of an audit.signing_key.added event's summary.
type signingKeyAfter struct {
	KeyID string `json:"key_id"`
	// PublicKey is PublicKey.String: what a verifier keeps off the cluster.
	PublicKey string `json:"public_key"`
}

func parseAfter(text string, into any) error {
	var s struct {
		After json.RawMessage `json:"after"`
	}
	if err := json.Unmarshal([]byte(text), &s); err != nil || len(s.After) == 0 {
		return errors.New("the event has no \"after\" in its summary")
	}
	return json.Unmarshal(s.After, into)
}

// CheckpointOptions tunes Checkpoint.
type CheckpointOptions struct {
	// Wait waits for another anchor or checkpoint in progress instead of skipping (a run made on demand).
	Wait bool
}

// CheckpointResult says what Checkpoint did.
type CheckpointResult struct {
	// Written is true when a checkpoint was written. Otherwise Skipped says why: "busy" (another transaction
	// holds the anchor lock), "no_anchor" (nothing to sign yet) or "current" (the newest anchor is signed).
	Written bool
	Skipped string
	// ChainSeq is the checkpoint event's chain_seq; HeadSeq and Head (hex) are the signed install chain row.
	ChainSeq int64
	HeadSeq  int64
	Head     string
	KeyID    string
	// KeyAdded is true when the same transaction also published the key (audit.signing_key.added).
	KeyAdded bool
	// Note is the signed note.
	Note string
}

// ErrCheckpointInconsistent is returned when the install chain no longer holds the head the last checkpoint
// signed. A signer never signs a chain that disagrees with one it signed before (the rule of C2SP
// tlog-checkpoint, as in Anchor), so it refuses and the checkpoint age grows until an operator looks.
var ErrCheckpointInconsistent = errors.New("audit checkpoint: the install chain no longer holds the head of the last checkpoint")

// Checkpoint signs the newest anchor of the install chain when no checkpoint covers it yet, and records the
// signed note as an audit.checkpoint event. It takes the anchor lock, so the anchor job and the checkpoint
// of one replica never overlap, and only one replica signs a given anchor. Before it signs it checks that
// the chain still holds the head of the last checkpoint (ErrCheckpointInconsistent). The first checkpoint
// of a key also writes audit.signing_key.added, in the same transaction, so a checkpoint never lacks its
// published key. A database that refuses the insert for lack of privilege returns ErrInstallWriteDenied.
func Checkpoint(ctx context.Context, db Beginner, keys KeySource, opt CheckpointOptions) (CheckpointResult, error) {
	var res CheckpointResult
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		res = CheckpointResult{}
		q := store.New(tx)
		if opt.Wait {
			if err := q.LockAuditAnchor(ctx); err != nil {
				return fmt.Errorf("audit checkpoint: lock: %w", err)
			}
		} else {
			locked, err := q.TryLockAuditAnchor(ctx)
			if err != nil {
				return fmt.Errorf("audit checkpoint: lock: %w", err)
			}
			if !locked {
				res.Skipped = "busy"
				return nil
			}
		}
		anchor, err := q.GetLastInstallAnchor(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Skipped = "no_anchor"
			return nil
		}
		if err != nil {
			return fmt.Errorf("audit checkpoint: read the newest anchor: %w", err)
		}
		last, err := q.GetLastInstallCheckpoint(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("audit checkpoint: read the newest checkpoint: %w", err)
		default:
			var prev checkpointAfter
			if err := parseAfter(last.SummaryText, &prev); err != nil {
				return fmt.Errorf("audit checkpoint: the checkpoint at chain_seq %d: %w", last.ChainSeq, err)
			}
			want, err := decodeHash(prev.Head)
			if err != nil {
				return fmt.Errorf("audit checkpoint: the checkpoint at chain_seq %d: head: %w", last.ChainSeq, err)
			}
			got, err := q.GetInstallEventHash(ctx, prev.AnchorSeq)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !bytes.Equal(got, want)) {
				return fmt.Errorf("%w: chain_seq %d", ErrCheckpointInconsistent, prev.AnchorSeq)
			}
			if err != nil {
				return fmt.Errorf("audit checkpoint: read the head of the last checkpoint: %w", err)
			}
			if prev.AnchorSeq >= anchor.ChainSeq {
				res.Skipped = "current"
				return nil
			}
		}
		installID, err := q.GetInstallID(ctx)
		if err != nil {
			return fmt.Errorf("audit checkpoint: read the install ID: %w", err)
		}
		origin := NoteOrigin(installID.String())
		key, err := keys.SigningKey(ctx, origin)
		if err != nil {
			return fmt.Errorf("audit checkpoint: the signing key: %w", err)
		}
		head, err := q.GetInstallEventHash(ctx, anchor.ChainSeq)
		if err != nil {
			return fmt.Errorf("audit checkpoint: read the anchor's hash: %w", err)
		}
		note, err := key.Sign(Note{Origin: origin, ChainSeq: anchor.ChainSeq, Hash: head})
		if err != nil {
			return fmt.Errorf("audit checkpoint: %w", err)
		}
		pub := key.Public()
		published, err := signingKeyPublished(ctx, q, pub.ID())
		if err != nil {
			return err
		}
		b := &batch{}
		if !published {
			b.Install(InstallEvent{
				Actor: System(AnchorActorID), Action: ActionSigningKeyAdded, TargetKind: SigningKeyTargetKind, TargetID: pub.ID(),
				After: signingKeyAfter{KeyID: pub.ID(), PublicKey: pub.String()},
			})
		}
		b.Install(InstallEvent{
			Actor: System(AnchorActorID), Action: ActionCheckpoint, TargetKind: "install", TargetID: installID.String(),
			After: checkpointAfter{AnchorSeq: anchor.ChainSeq, Head: hex.EncodeToString(head), KeyID: pub.ID(), Note: string(note)},
		})
		w, err := b.write(ctx, auditstore.New(tx))
		if err != nil {
			return err
		}
		ev := w.Install[len(w.Install)-1]
		res = CheckpointResult{
			Written: true, ChainSeq: ev.ChainSeq, HeadSeq: anchor.ChainSeq, Head: hex.EncodeToString(head),
			KeyID: pub.ID(), KeyAdded: !published, Note: string(note),
		}
		return nil
	})
	if err != nil {
		return CheckpointResult{}, err
	}
	return res, nil
}

// signingKeyPublished reports whether an audit.signing_key.added event exists for the key ID.
func signingKeyPublished(ctx context.Context, q *store.Queries, keyID string) (bool, error) {
	rows, err := q.ListInstallSigningKeys(ctx)
	if err != nil {
		return false, fmt.Errorf("audit checkpoint: read the published keys: %w", err)
	}
	for _, r := range rows {
		if r.TargetID == keyID {
			return true, nil
		}
	}
	return false, nil
}

// SigningKeys returns the public keys published in the install chain, oldest first. A verifier that kept
// none of them off the cluster can start from these, but trusts only the ones it pinned when they were new.
func SigningKeys(ctx context.Context, q *store.Queries) ([]PublicKey, error) {
	rows, err := q.ListInstallSigningKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the published signing keys: %w", err)
	}
	out := make([]PublicKey, 0, len(rows))
	for _, r := range rows {
		var a signingKeyAfter
		if err := parseAfter(r.SummaryText, &a); err != nil {
			return nil, fmt.Errorf("the signing key event at chain_seq %d: %w", r.ChainSeq, err)
		}
		k, err := ParsePublicKey(a.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("the signing key event at chain_seq %d: %w", r.ChainSeq, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// LatestCheckpoint returns the newest signed note recorded in the install chain; ok is false when there is none.
func LatestCheckpoint(ctx context.Context, q *store.Queries) (note []byte, ok bool, err error) {
	row, err := q.GetLastInstallCheckpoint(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the newest checkpoint: %w", err)
	}
	var a checkpointAfter
	if err := parseAfter(row.SummaryText, &a); err != nil {
		return nil, false, fmt.Errorf("the checkpoint at chain_seq %d: %w", row.ChainSeq, err)
	}
	return []byte(a.Note), true, nil
}
